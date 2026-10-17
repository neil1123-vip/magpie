package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APIBrowserRejectsAnUnconfiguredBalanceOnAnotherSite(t *testing.T) {
	a := sub2api.AccountView{ID: "candidate", Base: "https://panel.test"}
	for _, balance := range []string{"", "https://other.test/v1/usage"} {
		p := Provider{Chat: "https://other.test/v1", Key: "sk-fixture", BalanceURL: balance}
		calls := 0
		err := Sub2APIBrowserValidator(p)(t.Context(), a, func(context.Context, string, string, http.Header) ([]byte, error) {
			calls++
			return []byte(`{"data":{"balance":12}}`), nil
		})
		if err == nil || calls != 0 {
			t.Fatalf("a source from another site reached the panel: balance=%q calls=%d err=%v", balance, calls, err)
		}
	}
}

func TestSub2APIBrowserValidatesTheActualBalanceQuery(t *testing.T) {
	a := sub2api.AccountView{ID: "candidate", Base: "https://panel.test", UserAgent: "Browser fixture"}
	for _, custom := range []bool{false, true} {
		p := Provider{Key: "sk-fixture", BalanceURL: a.Base + sub2APIProfile}
		url, method, body := p.BalanceURL, http.MethodGet, `{"data":{"balance":12}}`
		if custom {
			p.BalanceURL, p.BalanceMethod, p.BalancePath = a.Base+"/wallet/{key}", "POST", "$available"
			p.Headers = map[string]string{"X-Wallet": "fixture"}
			url, method, body = a.Base+"/wallet/sk-fixture", http.MethodPost, `{"available":12}`
		}
		calls := 0
		read := func(_ context.Context, m, u string, headers http.Header) ([]byte, error) {
			calls++
			if u != url || m != method || custom && headers.Get("X-Wallet") != "fixture" {
				t.Fatalf("candidate used another query: %s %s %+v", m, u, headers)
			}
			return []byte(body), nil
		}
		if err := Sub2APIBrowserValidator(p)(context.Background(), a, read); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("balance query called %d times", calls)
		}
		p.BalanceURL = "https://other.test/wallet"
		if err := Sub2APIBrowserValidator(p)(context.Background(), a, read); err == nil || calls != 1 {
			t.Fatal("a cross-origin balance sent the candidate credentials")
		}
	}
}

func TestSub2APILegacyBindingCannotAuthorizeAnOperation(t *testing.T) {
	var p Provider
	if err := json.Unmarshal([]byte(`{"sub2api":{"session":"local","groupId":"g","version":9007199254740993}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Sub2API == nil || p.Sub2API.Session != "local" || p.Sub2API.Version != "" {
		t.Fatal("a legacy numeric version was accepted as a current operation")
	}
}

func TestSub2APILastBalanceKeepsCurrentFailure(t *testing.T) {
	priceHome(t)
	restart := func() {
		lastQuotas.Lock()
		lastQuotas.m, lastQuotas.loaded = nil, false
		lastQuotas.Unlock()
	}
	restart()
	t.Cleanup(restart)
	good := SubscriptionQuota{Provider: "relay", Name: "Relay", Sub2API: "session", Balance: "$12.50"}
	keepLast(good, "account")
	restart()
	for _, message := range []string{"sign in to the panel", "Cloudflare verification required", "signed out"} {
		failed := good
		failed.Balance, failed.Error = "", message
		got := keepLast(failed, "account")
		if got.Balance != good.Balance || got.AsOf == nil || got.Error != message || got.Sub2API != good.Sub2API {
			t.Fatalf("last balance hid the current failure: %+v", got)
		}
	}
}

func TestSub2APIBindingSurvivesOldEditorAndKeyReplacement(t *testing.T) {
	isolate(t)
	priceHome(t)
	p := Provider{ID: "panel", Name: "Panel", Chat: "https://panel.example/v1", Key: "sk-one",
		BalanceURL: "https://panel.example/api/v1/user/profile",
		Keys:       []KeyAccount{{Key: "sk-two"}}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers[0].Sub2API = &Sub2APIBinding{Session: "local-session", GroupID: "g",
		KeyIDs: []string{keyID("sk-one"), keyID("sk-two")}, Version: "binding-4"}
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	// The old form omitted sub2api altogether.
	p.Name = "Renamed"
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := panelProvider(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sub2API == nil || got.Sub2API.Version != "binding-4" || len(got.Sub2API.KeyIDs) != 2 {
		t.Fatalf("old editor erased the binding: %+v", got.Sub2API)
	}
	got.Keys[0].Key = "sk-replaced"
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	got, _ = panelProvider(p.ID)
	if got.Sub2API.Version == "" || got.Sub2API.Version == "binding-4" || !slices.Equal(got.Sub2API.KeyIDs, []string{keyID("sk-one")}) {
		t.Fatalf("a replacement key inherited an unconfirmed group: %+v", got.Sub2API)
	}
	if err := UnbindSub2API(p.ID, "binding-4", Sub2APIRevision(got)); err != ErrSub2APIConflict {
		t.Fatal("a stale binding version could unlink the current account")
	}
	if err := UnbindSub2API(p.ID, got.Sub2API.Version, "stale"); err != ErrSub2APIConflict {
		t.Fatal("a stale source revision could unlink the current account")
	}
	if err := UnbindSub2API(p.ID, got.Sub2API.Version, Sub2APIRevision(got)); err != nil {
		t.Fatal(err)
	}
	// A stale form with a binding cannot undo explicit unlinking.
	if err := Save(got); err != nil {
		t.Fatal(err)
	}
	got, _ = panelProvider(p.ID)
	if got.Sub2API != nil {
		t.Fatal("a stale form restored an explicitly removed binding")
	}
}

func TestSub2APIBoundAccountDoesNotBecomeKeyQuery(t *testing.T) {
	p := Provider{BalanceURL: "https://panel.example/api/v1/user/profile", BalancePath: "$data.balance",
		Sub2API: &Sub2APIBinding{Session: "session"}}
	if got := keyQueryOf(p); got.BalanceURL != p.BalanceURL || got.BalancePath != p.BalancePath {
		t.Fatalf("a bound account fell back to a key query: %+v", got)
	}
}

func TestSub2APIBalanceHistoryFollowsAccountQuery(t *testing.T) {
	priceHome(t)
	now := time.Now().UTC().Truncate(time.Second)
	at := now.Add(-10 * time.Hour)
	note := func(q SubscriptionQuota) SubscriptionQuota {
		t.Helper()
		qs := []SubscriptionQuota{q}
		noteBalanceHistory(qs, at)
		at = at.Add(time.Hour)
		return qs[0]
	}
	legacy := SubscriptionQuota{Provider: "relay", Balance: "$999"}
	note(legacy)
	legacy.Balance = "$998"
	note(legacy)
	p := Provider{ID: "relay", BalanceURL: "https://panel.example/api/v1/user/profile",
		BalancePath: "$data.balance", Sub2API: &Sub2APIBinding{Session: "first"}}
	card := func(p Provider, amount string) SubscriptionQuota {
		return SubscriptionQuota{Provider: p.ID, Sub2API: p.Sub2API.Session, Balance: amount,
			sub2apiBalance: &sub2apiBalanceStamp{query: sub2apiBalanceID(p)}}
	}
	first := p
	for _, change := range []string{"initial", "account", "query"} {
		if change == "account" {
			p.Sub2API = &Sub2APIBinding{Session: "second"}
		} else if change == "query" {
			p.BalancePath = "$data.available"
		}
		if got := note(card(p, "$100")); got.BalanceTrend != nil {
			t.Fatalf("%s inherited another balance history: %+v", change, got.BalanceTrend)
		}
		got := note(card(p, "$90"))
		if tr := got.BalanceTrend; tr == nil || len(tr.Points) != 2 || tr.Points[0].Amount != 100 || tr.Points[1].Amount != 90 {
			t.Fatalf("%s did not keep its own readings: %+v", change, tr)
		}
	}
	// Deduplicated copies and a new binding ticket still refer to the same account query.
	first.ID = "copy"
	first.Sub2API = &Sub2APIBinding{Session: "first", Version: "new-ticket"}
	got := note(card(first, "$80"))
	if tr := got.BalanceTrend; tr == nil || len(tr.Points) != 3 || tr.Points[0].Amount != 100 || tr.Points[2].Amount != 80 {
		t.Fatalf("the original query lost its history: %+v", tr)
	}
	unknown := card(first, "$70")
	unknown.Provider, unknown.sub2apiBalance = "relay", nil
	if got := note(unknown); got.BalanceTrend != nil {
		t.Fatalf("an unidentified panel reading inherited legacy history: %+v", got.BalanceTrend)
	}
	legacy.AsOf = &now
	if got := note(legacy); got.BalanceTrend == nil || len(got.BalanceTrend.Points) != 2 || got.BalanceTrend.Points[0].Amount != 999 {
		t.Fatalf("panel readings changed the legacy balance history: %+v", got.BalanceTrend)
	}
}
