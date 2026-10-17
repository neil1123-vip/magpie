package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APIAutomaticGroupsAndPricesRespectCurrentKeys(t *testing.T) {
	isolate(t)
	priceHome(t)
	var phase, keyReads, metricReads, disabledReads, crossSiteReads atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/wallet":
			w.Write([]byte(`{"balance":12.5}`))
		case "/api/v1/groups/available":
			w.Write([]byte(`{"data":[{"id":142,"name":"Pro","rate_multiplier":0.14},{"id":151,"name":"ProMax","rate_multiplier":0.2},{"id":67,"name":"Standard","rate_multiplier":0.6},{"id":105,"name":"Alternate","rate_multiplier":0.8}]}`))
		case "/api/v1/settings/public":
			w.Write([]byte(`{"data":{"channel_monitor_mode":"v2"}}`))
		case "/api/v1/channel-monitor-v2/matrix":
			metricReads.Add(1)
			w.Write([]byte(`{"data":{"items":[{"group_id":142,"metrics":{"cache_rate":0.75,"error_rate":0.002,"ttft":{"p50_ms":8000}}},{"group_id":151,"metrics":{"cache_rate":0.6,"error_rate":0.01,"ttft":{"p50_ms":4000}}},{"group_id":67,"metrics":{"cache_rate":0.5,"error_rate":0}},{"group_id":105,"metrics":{"cache_rate":0.8,"error_rate":0}}]}}`))
		case "/api/v1/keys":
			keyReads.Add(1)
			current := phase.Load()
			if current == 2 {
				w.Write([]byte(`{"data":{"items":[],"total":5}}`))
				return
			}
			if current == 3 {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
			}
			first, second := 142, 67
			if current > 0 {
				first, second = 151, 105
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
				"items": []any{
					map[string]any{"key": "sk-one", "group_id": first},
					map[string]any{"key": "sk-two", "group_id": second},
					map[string]any{"key": "sk-zero", "group_id": 142},
					map[string]any{"key": "sk-invalid", "group_id": 142},
					map[string]any{"key": "sk-no-group", "group_id": nil},
					map[string]any{"key": "sk-routed", "group_id": 142, "group_ids": []int{142, 151}},
				}, "total": 6, "page": 1, "page_size": 100,
			}})
		case "/v1/sub2api/billing":
			var rate any
			switch r.Header.Get("Authorization") {
			case "Bearer sk-one":
				rate = .14
				switch phase.Load() {
				case 1:
					rate = .2
				case 2:
					rate = .91
				case 3:
					rate = .3
				}
			case "Bearer sk-two":
				rate = .6
				switch phase.Load() {
				case 1:
					rate = .8
				case 2:
					rate = .92
				case 3:
					rate = .9
				}
			case "Bearer sk-zero":
				rate = 0
			case "Bearer sk-invalid":
				rate = "invalid"
			case "Bearer sk-no-group":
				rate = .9
			case "Bearer sk-routed":
				rate = .14
			case "Bearer sk-off":
				disabledReads.Add(1)
				rate = .01
			default:
				t.Error("billing used an unexpected credential")
				http.Error(w, "unknown key", http.StatusUnauthorized)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"effective_rate_multiplier": rate})
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	defer unblock()
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossSiteReads.Add(1)
		t.Errorf("a balance-only source sent a request to its different API site: %s", r.URL.Path)
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))
	defer inference.Close()
	p := Provider{ID: "automatic-panel", Chat: panel.URL + "/v1", Key: "sk-one",
		Keys: []KeyAccount{{Key: "sk-two"}, {Key: "sk-zero"}, {Key: "sk-invalid"},
			{Key: "sk-no-group"}, {Key: "sk-routed"}, {Key: "sk-off", Off: true}},
		PriceRate: 2, BalanceURL: panel.URL + "/wallet", BalancePath: "$balance",
		KeyPriceRates: map[string]float64{keyID("sk-zero"): .4, keyID("sk-invalid"): .5,
			keyID("sk-no-group"): .6, keyID("sk-routed"): .77, keyID("sk-off"): .7}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	session, monitor, err := Sub2APIServices()
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
	binding := Sub2APIBinding{Session: login.Account.ID, Keys: []Sub2APIKeyBinding{
		{ID: keyID("sk-one"), GroupID: "151", GroupName: "ProMax"},
		{ID: keyID("sk-off"), GroupID: "67", GroupName: "Standard"},
		{ID: keyID("sk-routed"), GroupID: "67", GroupName: "Standard"},
	}}
	if err := BindSub2API(t.Context(), p.ID, binding, Sub2APIRevision(p), login.Account.Generation); err != nil {
		t.Fatal(err)
	}
	crossSite := Provider{ID: "balance-only-panel", Chat: inference.URL + "/v1", Key: "sk-cross-site",
		PriceRate: 3, KeyPriceRates: map[string]float64{keyID("sk-cross-site"): .75},
		BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
	if err := Save(crossSite); err != nil {
		t.Fatal(err)
	}
	crossSite, _ = panelProvider(crossSite.ID)
	if err := BindSub2API(t.Context(), crossSite.ID, Sub2APIBinding{Session: login.Account.ID},
		Sub2APIRevision(crossSite), login.Account.Generation); err != nil {
		t.Fatal(err)
	}
	crossSite, _ = panelProvider(crossSite.ID)
	crossSiteRevision := Sub2APIRevision(crossSite)
	// Old drafts kept key prices inside their bindings rather than KeyPriceRates.
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(f.Providers, func(value Provider) bool { return value.ID == p.ID })
	f.Providers[index].Sub2API.Keys = append(f.Providers[index].Sub2API.Keys,
		Sub2APIKeyBinding{ID: keyID("sk-invalid"), GroupID: "151", GroupName: "ProMax", PriceRate: .5})
	delete(f.Providers[index].KeyPriceRates, keyID("sk-invalid"))
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	preview := func(ctx context.Context) (sub2api.MonitorAccount, error) {
		targets, err := Sub2APIPreviewTargets(login.Account.ID, "", "", "", login.Account.Generation)
		if err != nil {
			return sub2api.MonitorAccount{}, err
		}
		cfg, err := monitor.Settings(login.Account.ID)
		if err != nil {
			return sub2api.MonitorAccount{}, err
		}
		return monitor.Preview(ctx, login.Account.ID, login.Account.Generation, cfg.Revision, targets)
	}
	account, err := preview(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	assertPrices := func(first, second float64) {
		t.Helper()
		if p.PriceRate != 2 || sub2apiLocalKeyRate(p, keyID("sk-one")) != first ||
			sub2apiLocalKeyRate(p, keyID("sk-two")) != second ||
			sub2apiLocalKeyRate(p, keyID("sk-zero")) != .4 ||
			sub2apiLocalKeyRate(p, keyID("sk-invalid")) != .5 ||
			sub2apiLocalKeyRate(p, keyID("sk-no-group")) != .6 ||
			sub2apiLocalKeyRate(p, keyID("sk-routed")) != .77 ||
			sub2apiLocalKeyRate(p, keyID("sk-off")) != .7 || disabledReads.Load() != 0 {
			t.Fatalf("automatic sync changed unrelated or unknown prices: %+v", p.KeyPriceRates)
		}
	}
	assertGroup := func(value Provider, key, group string) {
		t.Helper()
		keys := Sub2APIKeyBindings(value)
		i := slices.IndexFunc(keys, func(k Sub2APIKeyBinding) bool { return k.ID == keyID(key) })
		if i < 0 || keys[i].GroupID != group {
			t.Fatalf("key %s did not follow group %s: %+v", keyID(key), group, keys)
		}
	}
	assertPrices(.14, .6)
	assertGroup(p, "sk-one", "142")
	assertGroup(p, "sk-two", "67")
	assertGroup(p, "sk-off", "67")
	assertGroup(p, "sk-invalid", "142")
	assertGroup(p, "sk-routed", "67")
	if keyReads.Load() != 1 || metricReads.Load() != 1 || len(account.Sources) != 2 {
		t.Fatalf("one account query did not share key and metric reads: keys=%d metrics=%d account=%+v", keyReads.Load(), metricReads.Load(), account)
	}
	groupIndex := slices.IndexFunc(account.Groups, func(group sub2api.Group) bool { return group.ID == "142" })
	if groupIndex < 0 {
		t.Fatal("preview omitted the actual key group")
	}
	metrics := account.Groups[groupIndex].Observation
	if metrics == nil || metrics.Scope != "group" || metrics.CacheRate == nil || *metrics.CacheRate != .75 ||
		metrics.Availability == nil || *metrics.Availability != .998 ||
		metrics.TTFTP50MS == nil || *metrics.TTFTP50MS != 8000 {
		t.Fatalf("preview did not carry panel metrics through the provider adapter: %+v", metrics)
	}
	sourceIndex := slices.IndexFunc(account.Sources, func(source sub2api.SourceSnapshot) bool { return source.ID == p.ID })
	if sourceIndex < 0 || len(account.Sources[sourceIndex].Keys) != 6 {
		t.Fatalf("preview omitted the valid source's active keys: %+v", account.Sources)
	}
	for _, key := range account.Sources[sourceIndex].Keys {
		if key.ID == keyID("sk-routed") && (key.GroupVerified || key.GroupError == "" ||
			key.LocalRate == nil || *key.LocalRate != .77) {
			t.Fatalf("dynamic key silently synchronized its primary group: %+v", key)
		}
		if key.ID == keyID("sk-one") && (key.LocalRate == nil || *key.LocalRate != .14 || !key.GroupVerified) {
			t.Fatalf("preview did not expose committed local state: %+v", key)
		}
	}
	crossSource := slices.IndexFunc(account.Sources, func(source sub2api.SourceSnapshot) bool { return source.ID == crossSite.ID })
	crossSite, _ = panelProvider(crossSite.ID)
	if crossSource < 0 || len(account.Sources[crossSource].Keys) != 1 ||
		account.Sources[crossSource].Keys[0].SyncError == "" || account.Sources[crossSource].Keys[0].GroupVerified ||
		Sub2APIRevision(crossSite) != crossSiteRevision || crossSite.PriceRate != 3 ||
		sub2apiLocalKeyRate(crossSite, keyID("sk-cross-site")) != .75 || crossSiteReads.Load() != 0 {
		t.Fatalf("balance-only source blocked valid sync or changed across API origins: %+v", account.Sources)
	}
	cfg, err := monitor.Settings(login.Account.ID)
	if err != nil || cfg.Configured || cfg.Enabled || len(monitor.Snapshot().Changes) != 0 {
		t.Fatalf("preview enabled monitoring or recorded changes: %+v %v", cfg, err)
	}
	for _, name := range []string{"monitor-settings.json", "monitor.json"} {
		if _, err := os.Stat(filepath.Join(appdir.Config(), "sub2api-local", name)); !os.IsNotExist(err) {
			t.Fatalf("preview created monitor state %s: %v", name, err)
		}
	}
	if err := monitor.SetAccountExpected(login.Account.ID, true, nil, login.Account.Generation, ""); err != nil {
		t.Fatal(err)
	}
	cfg, _ = monitor.Settings(login.Account.ID)
	phase.Store(1)
	if err := monitor.RefreshAccount(t.Context(), login.Account.ID, login.Account.Generation, cfg.Revision); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	assertPrices(.2, .8)
	assertGroup(p, "sk-one", "151")
	assertGroup(p, "sk-two", "105")
	if len(monitor.Snapshot().Accounts) != 1 || len(monitor.Snapshot().Accounts[0].Sources) != 2 {
		t.Fatal("enabled monitoring did not commit its sample")
	}
	if metricReads.Load() != 2 || monitor.Snapshot().Accounts[0].ObservationLastSuccess.IsZero() {
		t.Fatal("background monitoring did not refresh shared panel metrics")
	}
	phase.Store(2)
	if _, err := preview(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	assertPrices(.2, .8)
	assertGroup(p, "sk-one", "151")
	assertGroup(p, "sk-two", "105")

	phase.Store(3)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	finished := make(chan struct{})
	defer func() {
		cancel()
		unblock()
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("preview did not stop after cleanup")
		}
	}()
	go func() {
		defer close(finished)
		_, err := preview(ctx)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("preview ended before its key query was held: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("preview did not start its key query")
	}
	p, _ = panelProvider(p.ID)
	p.Key = "sk-replaced"
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a replaced key accepted an old synchronization result")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("stale preview did not finish")
	}
	p, _ = panelProvider(p.ID)
	if p.Key != "sk-replaced" || p.KeyPriceRates[keyID("sk-one")] != 0 ||
		p.KeyPriceRates[keyID("sk-replaced")] != 0 || sub2apiLocalKeyRate(p, keyID("sk-two")) != .8 {
		t.Fatalf("stale synchronization restored a replaced key or its price: %+v", p.KeyPriceRates)
	}
	crossSite, _ = panelProvider(crossSite.ID)
	if Sub2APIRevision(crossSite) != crossSiteRevision || crossSiteReads.Load() != 0 {
		t.Fatal("a different API origin acquired a group, price or billing request")
	}
}
