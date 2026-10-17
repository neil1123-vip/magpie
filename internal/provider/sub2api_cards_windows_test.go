package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APIKeptBalanceCards(t *testing.T) {
	for _, mode := range []string{"current", "external-logout", "changed-query", "cold-cache"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			ForgetKeptCardsForTest()
			t.Cleanup(ForgetKeptCardsForTest)
			var wallets atomic.Int32
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"access_token":"test-access","refresh_token":"test-refresh"}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					wallets.Add(1)
					w.Write([]byte(`{"balance":12.5}`))
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(panel.Close)
			p := Provider{ID: "balance-panel", Name: "Balance panel", Chat: panel.URL + "/v1", Key: "sk-test",
				BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			session, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			p, _ = panelProvider(p.ID)
			input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "test-password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			login, err := session.Login(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if err := BindSub2API(t.Context(), p.ID, Sub2APIBinding{Session: login.Account.ID}, login.Revision, login.Account.Generation); err != nil {
				t.Fatal(err)
			}
			ForgetBalances()
			seed := KeyBalances(t.Context())
			if len(seed) != 1 || seed[0].Balance != "$12.50" || seed[0].Error != "" {
				t.Fatalf("initial balance: %+v", seed)
			}
			bound, err := panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			persistedTag := p.ID + "/" + keyTag("sub2api", sub2apiBalanceID(bound))
			ForgetKeptCardsForTest()
			persisted, ok := lastReading(persistedTag)
			if !ok || persisted.Balance != "$12.50" || persisted.AsOf == nil {
				t.Fatalf("persisted balance fixture was not retained: %+v %v", persisted, ok)
			}
			before := wallets.Load()
			switch mode {
			case "external-logout":
				owner := sub2api.New(filepath.Join(appdir.Config(), "sub2api-local"))
				if _, err := owner.LogoutExpected(t.Context(), login.Account.ID, login.Account.Generation); err != nil {
					t.Fatal(err)
				}
			case "changed-query":
				bound.BalancePath = "$balance / 2"
				if err := Save(bound); err != nil {
					t.Fatal(err)
				}
			case "cold-cache":
				keepLast(SubscriptionQuota{Provider: p.ID, Balance: "$999.00"}, keyTag("balance", p.Key))
				ForgetBalances()
				ForgetKeptCardsForTest()
			}
			if mode == "current" || mode == "external-logout" {
				keyAllowances.Lock()
				if keyAllowances.m == nil {
					keyAllowances.m = map[string]*keyAllowance{}
				}
				keyAllowances.m[keyAllowanceID(p)] = &keyAllowance{read: keyReading{
					balance: "$99.00", at: time.Now(), seq: seed[0].readSeq + 1,
				}}
				keyAllowances.Unlock()
			}
			cards := slices.DeleteFunc(CachedCards(time.Now()), func(q SubscriptionQuota) bool {
				return q.Provider != p.ID || q.Kind != "balance"
			})
			report := slices.DeleteFunc(KeptQuotaReport(time.Now()), func(q Quota) bool {
				return q.Provider != p.ID || q.Kind != "balance"
			})
			if wallets.Load() != before {
				t.Fatal("kept cards unexpectedly asked the panel")
			}
			stale := mode == "external-logout" || mode == "changed-query"
			historical := stale || mode == "cold-cache"
			if len(cards) != 1 || cards[0].Balance != "$12.50" ||
				(cards[0].Error != "") != stale || (cards[0].AsOf != nil) != historical {
				t.Errorf("CachedCards lost the balance or its authority: %+v", cards)
			}
			if len(report) != 1 || report[0].Balance != "$12.50" ||
				(report[0].Error != "") != stale || (report[0].AsOf != nil) != historical {
				t.Errorf("KeptQuotaReport lost the balance or its authority: %+v", report)
			}
			keyBalanceCache.Lock()
			unchanged := len(keyBalanceCache.data) == 1 && keyBalanceCache.data[0].Error == "" && keyBalanceCache.data[0].AsOf == nil
			keyBalanceCache.Unlock()
			if mode != "cold-cache" && !unchanged {
				t.Error("a kept read changed the live balance cache")
			}
			if mode == "cold-cache" {
				copyID, err := AddCopy(bound, bound.ID)
				if err != nil {
					t.Fatal(err)
				}
				balances := func() []SubscriptionQuota {
					return slices.DeleteFunc(CachedCards(time.Now()), func(q SubscriptionQuota) bool {
						return q.Kind != "balance"
					})
				}
				if got := balances(); len(got) != 1 || got[0].Balance != "$12.50" {
					t.Fatalf("a shared account query gained a duplicate card: %+v", got)
				}
				copy, err := panelProvider(copyID)
				if err != nil {
					t.Fatal(err)
				}
				copy.BalancePath = "$balance / 2"
				if err := Save(copy); err != nil {
					t.Fatal(err)
				}
				ForgetBalances()
				if got := KeyBalances(t.Context()); len(got) != 2 {
					t.Fatalf("distinct query fixture: %+v", got)
				}
				ForgetBalances()
				ForgetKeptCardsForTest()
				before = wallets.Load()
				got := balances()
				if len(got) != 2 || !slices.ContainsFunc(got, func(q SubscriptionQuota) bool { return q.Balance == "$6.25" }) ||
					wallets.Load() != before || slices.ContainsFunc(got, func(q SubscriptionQuota) bool { return q.AsOf == nil || q.Error != "" }) {
					t.Errorf("distinct persisted queries were lost or read again: %+v", got)
				}
			}
		})
	}
}

func TestSub2APISingleCardRefresh(t *testing.T) {
	for _, mode := range []string{"single-source", "duplicate-after", "duplicate-before-live", "duplicate-before-cold"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			ForgetKeptCardsForTest()
			t.Cleanup(ForgetKeptCardsForTest)
			var wallets atomic.Int32
			var increased atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"access_token":"test-access","refresh_token":"test-refresh"}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					wallets.Add(1)
					if increased.Load() {
						w.Write([]byte(`{"balance":99}`))
					} else {
						w.Write([]byte(`{"balance":12.5}`))
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(panel.Close)
			p := Provider{ID: "balance-panel", Name: "Balance panel", Chat: panel.URL + "/v1", Key: "sk-test",
				BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			session, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			p, _ = panelProvider(p.ID)
			input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "test-password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			login, err := session.Login(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if err := BindSub2API(t.Context(), p.ID, Sub2APIBinding{Session: login.Account.ID}, login.Revision, login.Account.Generation); err != nil {
				t.Fatal(err)
			}
			seed := KeyBalances(t.Context())
			if len(seed) != 1 || seed[0].Balance != "$12.50" || seed[0].Error != "" {
				t.Fatalf("initial balance: %+v", seed)
			}
			if mode != "single-source" {
				bound, err := panelProvider(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				copyID, err := AddCopy(bound, bound.ID)
				if err != nil {
					t.Fatal(err)
				}
				copy, err := panelProvider(copyID)
				if err != nil || sub2apiBalanceID(copy) != sub2apiBalanceID(bound) {
					t.Fatalf("copy does not share the balance query: %+v %v", copy, err)
				}
				if mode != "duplicate-after" {
					if err := SetOrder([]string{copyID, p.ID}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "duplicate-before-cold" {
				ForgetBalances()
				ForgetKeptCardsForTest()
			}
			before := wallets.Load()
			displayed := slices.DeleteFunc(CachedCards(time.Now()), func(q SubscriptionQuota) bool {
				return q.Kind != "balance"
			})
			if len(displayed) != 1 || displayed[0].Provider != p.ID || displayed[0].Balance != "$12.50" {
				t.Fatalf("displayed card: %+v", displayed)
			}
			if wallets.Load() != before {
				t.Fatal("cached cards asked the panel")
			}
			increased.Store(true)
			report := RefreshReport(t.Context(), p.ID, "", time.Now())
			balances := slices.DeleteFunc(report, func(q Quota) bool { return q.Kind != "balance" })
			if wallets.Load() != before+1 || len(balances) != 1 || balances[0].Provider != p.ID ||
				balances[0].Error != "" || balances[0].Balance != "$99.00" || balances[0].AsOf != nil {
				t.Errorf("single-card refresh did not read the displayed account: wallet-delta=%d report=%+v", wallets.Load()-before, balances)
			}
			if mode == "duplicate-before-live" {
				RefreshUsage(t.Context(), p.ID, "")
				guiCards := KeyBalances(t.Context())
				if wallets.Load() != before+2 || len(guiCards) != 1 || guiCards[0].Balance != "$99.00" {
					t.Errorf("GUI refresh did not read the displayed account: wallet-delta=%d cards=%+v", wallets.Load()-before, guiCards)
				}
			}
		})
	}
}
