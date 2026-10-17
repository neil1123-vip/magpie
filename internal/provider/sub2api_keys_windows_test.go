package provider

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestSub2APIKeysKeepIndependentGroupsAndPrices(t *testing.T) {
	isolate(t)
	priceHome(t)
	var firstReads, secondReads atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/wallet":
			w.Write([]byte(`{"balance":12.5}`))
		case "/v1/sub2api/billing":
			switch r.Header.Get("Authorization") {
			case "Bearer sk-one":
				firstReads.Add(1)
				w.Write([]byte(`{"effective_rate_multiplier":0.8}`))
			case "Bearer sk-two":
				secondReads.Add(1)
				w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
			default:
				t.Error("billing used a panel credential")
				http.Error(w, "unknown key", 401)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	p := Provider{ID: "panel", Chat: panel.URL + "/v1", Key: "sk-one",
		Keys: []KeyAccount{{Key: "sk-two"}}, PriceRate: 1.2, BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
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
	first, second := keyID("sk-one"), keyID("sk-two")
	bind := func(id, group string) {
		t.Helper()
		version := ""
		if p.Sub2API != nil {
			version = p.Sub2API.Version
		}
		if err := BindSub2APIKey(t.Context(), p.ID, login.Account.ID, id, group, group,
			version, Sub2APIRevision(p), login.Account.Generation); err != nil {
			t.Fatal(err)
		}
		p, _ = panelProvider(p.ID)
	}
	bind(first, "premium")
	bind(second, "standard")
	keys := Sub2APIKeyBindings(p)
	if len(keys) != 2 || keys[0].GroupID != "premium" || keys[1].GroupID != "standard" {
		t.Fatalf("keys shared a source-wide group: %+v", keys)
	}
	crossSite := p
	crossSite.Chat = "https://other.test/v1"
	if err := Save(crossSite); err == nil {
		t.Fatal("changing the API site retained a group confirmation for the old site")
	}
	target := sub2apiTarget(p, *login.Account)
	if len(target.Keys) != 2 || target.Keys[0].GroupID != "premium" || target.Keys[1].GroupID != "standard" {
		t.Fatalf("monitor lost the per-key groups: %+v", target.Keys)
	}
	crossSite.Sub2API = cloneSub2APIBinding(p.Sub2API)
	imported, err := read()
	if err != nil {
		t.Fatal(err)
	}
	imported.Providers[0] = crossSite
	if err := store(imported); err != nil {
		t.Fatal(err)
	}
	changed := sub2apiTarget(crossSite, *login.Account)
	if _, err := readSub2APIBilling(t.Context(), changed, changed.Keys[0]); err == nil || firstReads.Load() != 0 {
		t.Fatal("an imported cross-site binding sent the key to its previous site")
	}
	imported, err = read()
	if err != nil {
		t.Fatal(err)
	}
	imported.Providers[0] = p
	if err := store(imported); err != nil {
		t.Fatal(err)
	}
	revision := Sub2APIRevision(p)
	apply := func(id string, rate, old float64) error {
		return ApplySub2APIKeyRate(t.Context(), p.ID, id, rate, old, p.Sub2API.Version, Sub2APIRevision(p), login.Account.Generation)
	}
	if err := apply(first, .8, 1.2); err != nil {
		t.Fatal(err)
	}
	if firstReads.Load() != 1 || secondReads.Load() != 0 {
		t.Fatal("applying one key also queried another key")
	}
	p, _ = panelProvider(p.ID)
	if p.PriceRate != 1.2 || sub2apiLocalKeyRate(p, first) != .8 || sub2apiLocalKeyRate(p, second) != 1.2 {
		t.Fatalf("applying a key changed sibling pricing: %+v", p.Sub2API)
	}
	if err := ApplySub2APIKeyRate(t.Context(), p.ID, second, 1.5, 1.2, p.Sub2API.Version, revision, login.Account.Generation); err != ErrSub2APIConflict {
		t.Fatal("a stale source revision authorized a second price write")
	}
	if err := apply(second, 1.5, 1.2); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	if p.PriceRate != 1.2 || sub2apiLocalKeyRate(p, first) != .8 || sub2apiLocalKeyRate(p, second) != 1.5 {
		t.Fatalf("keys cannot keep different local rates: %+v", p.Sub2API)
	}
	bind(first, "other")
	if sub2apiLocalKeyRate(p, first) != .8 || Sub2APIKeyBindings(p)[1].GroupID != "standard" {
		t.Fatal("rebinding a key changed a sibling or a manually applied price")
	}
	bind(first, "")
	if Sub2APIKeyBindings(p)[0].GroupID != "" || Sub2APIKeyBindings(p)[1].GroupID != "standard" {
		t.Fatal("unlinking one key cleared another key's group")
	}
	if err := apply(first, .8, .8); err == nil {
		t.Fatal("a key with no confirmed group could apply a price")
	}
	// The ordinary source editor must retain these references.
	p.Name = "Renamed"
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	if len(p.Sub2API.Keys) != 2 || p.Sub2API.Keys[1].GroupID != "standard" ||
		sub2apiLocalKeyRate(p, first) != .8 || sub2apiLocalKeyRate(p, second) != 1.5 {
		t.Fatal("ordinary source saving dropped key bindings or local prices")
	}
	oldVersion := p.Sub2API.Version
	p.Keys[0].Key = "sk-replaced"
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	if len(p.Sub2API.Keys) != 1 || p.Sub2API.Keys[0].ID != first || p.Sub2API.Version == oldVersion ||
		len(p.KeyPriceRates) != 1 || p.KeyPriceRates[first] != .8 {
		t.Fatal("a replaced key inherited its old group or price")
	}
}
