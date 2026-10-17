package usage

import (
	"math"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
)

func TestSub2APIPricesTheKeyThatServedEachCall(t *testing.T) {
	pageHome(t)
	first, second, inherited := provider.KeyID("first"), provider.KeyID("second"), provider.KeyID("inherited")
	p := provider.Provider{ID: "relay", Chat: "https://relay.example/v1", Catalog: "openai", Key: "first",
		Keys: []provider.KeyAccount{{Key: "second"}, {Key: "inherited"}}, PriceRate: 2,
		Sub2API: &provider.Sub2APIBinding{Session: "panel", Keys: []provider.Sub2APIKeyBinding{
			{ID: first, GroupID: "discount", PriceRate: 0.5},
			{ID: second, GroupID: "premium", PriceRate: 3},
			{ID: inherited, GroupID: "default"},
		}},
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	own := 7.0
	cases := []struct {
		key, model, served string
		own                *float64
		want               float64
	}{
		{first, "m", "", nil, 6.5},
		{second, "m", "", nil, 39},
		{"", "m", "", nil, 26},
		{inherited, "m", "", nil, 26},
		{provider.KeyID("unknown"), "m", "", nil, 26},
		{second, "unlisted", "m", nil, 39},
		{first, "m", "", &own, own},
	}
	cost := NewCoster()
	var recs []Record
	wantTotal := 0.0
	now := time.Now()
	for i, tc := range cases {
		r := Record{Time: now.Add(-time.Duration(i) * time.Second), Agent: "codex", Provider: p.ID,
			Model: tc.model, Served: tc.served, ProviderKeyID: tc.key, Input: 1_000_000,
			Output: 1_000_000, CacheRead: 1_000_000, CacheWrite: 1_000_000, Status: 200, OwnCost: tc.own}
		recs = append(recs, r)
		Append(r)
		got, ok := cost(r)
		if !ok || math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("key %q model %q: cost %g priced=%t, want %g", tc.key, tc.model, got, ok, tc.want)
		}
		wantTotal += tc.want
	}
	if got := NewPricer()(recs).Cost; math.Abs(got-wantTotal) > 1e-9 {
		t.Errorf("combined key costs %g, want %g", got, wantTotal)
	}
	page := QueryPage(All, Filter{}, 0, 100)
	if len(page.Rows) != len(cases) || math.Abs(page.Sum.Cost-wantTotal) > 1e-9 {
		t.Fatalf("request page rows=%d cost=%g, want %d and %g", len(page.Rows), page.Sum.Cost, len(cases), wantTotal)
	}
	for i, row := range page.Rows {
		if math.Abs(row.Cost-cases[i].want) > 1e-9 {
			t.Errorf("request page key %q: cost %g, want %g", row.ProviderKeyID, row.Cost, cases[i].want)
		}
	}
	if pr, ok := provider.EffectivePriceIn(settings.Load(), p.ID, "m"); !ok || pr.Input != 4 {
		t.Fatalf("a model without a key must keep the source rate: %+v priced=%t", pr, ok)
	}
	if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{
		"relay/m": {Input: new(2.0), Output: new(3.0), CacheRead: new(4.0), CacheWrite: new(5.0)},
	}}); err != nil {
		t.Fatal(err)
	}
	cost = NewCoster()
	for _, r := range recs {
		want := 14.0
		if r.OwnCost != nil {
			want = own
		}
		if got, ok := cost(r); !ok || got != want {
			t.Errorf("explicit price key %q: cost %g priced=%t, want %g", r.ProviderKeyID, got, ok, want)
		}
	}
	if own != 7 || recs[len(recs)-1].OwnCost == nil || *recs[len(recs)-1].OwnCost != 7 {
		t.Fatal("pricing changed the call's own reported cost")
	}
}

func TestSub2APIKeyPricesFollowRenamedProvider(t *testing.T) {
	pageHome(t)
	first, second := provider.KeyID("first"), provider.KeyID("second")
	p := provider.Provider{ID: "relay", Chat: "https://relay.example/v1", Catalog: "openai",
		Key: "first", Keys: []provider.KeyAccount{{Key: "second"}}, PriceRate: 2,
		KeyPriceRates: map[string]float64{first: 0.5, second: 3}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := provider.Rename("relay", "renamed"); err != nil {
		t.Fatal(err)
	}
	p.KeyPriceRates = map[string]float64{first: 4, second: 5}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cases := []struct {
		id, key, provider string
		at                time.Time
		cost              float64
	}{
		{"old-first", first, "renamed", now.Add(-time.Hour), 1},
		{"old-second", second, "renamed", now.Add(-time.Hour + time.Second), 6},
		{"new-first", first, "relay", now.Add(time.Second), 8},
		{"new-second", second, "relay", now.Add(2 * time.Second), 10},
	}
	var recs []Record
	cost := NewCoster()
	for _, tc := range cases {
		r := Record{Time: tc.at, RequestID: tc.id, Agent: "codex", Provider: "relay",
			Model: "m", ProviderKeyID: tc.key, Input: 1_000_000, Status: 200}
		Append(r)
		recs = append(recs, r)
		if got, ok := cost(r); !ok || got != tc.cost {
			t.Errorf("%s cost %g priced=%t, want %g", tc.id, got, ok, tc.cost)
		}
	}
	if got := NewPricer()(recs).Cost; got != 25 {
		t.Errorf("combined cost %g, want 25", got)
	}
	page := QueryPage(All, Filter{}, 0, 100)
	if len(page.Rows) != len(cases) || page.Sum.Cost != 25 {
		t.Fatalf("request page rows=%d cost=%g, want 4 and 25", len(page.Rows), page.Sum.Cost)
	}
	for _, tc := range cases {
		found := false
		for _, row := range page.Rows {
			if row.RequestID != tc.id {
				continue
			}
			found = true
			if row.Provider != tc.provider || row.Cost != tc.cost {
				t.Errorf("%s provider=%q cost=%g, want %q and %g", tc.id, row.Provider, row.Cost, tc.provider, tc.cost)
			}
		}
		if !found {
			t.Errorf("missing request %s", tc.id)
		}
	}
}

func TestSub2APIKeyPricesKeepEachCallsRateWindow(t *testing.T) {
	pageHome(t)
	first, second := provider.KeyID("first"), provider.KeyID("second")
	p := provider.Provider{ID: "relay", Chat: "https://relay.example/v1", Catalog: "openai",
		Key: "first", Keys: []provider.KeyAccount{{Key: "second"}}, PriceRate: 2,
		KeyPriceRates: map[string]float64{first: 0.5, second: 3},
		RateWindows:   []provider.RateWindow{{Start: "09:00", End: "12:00", Rate: 3}}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	peak := time.Date(2026, 10, 20, 10, 0, 0, 0, time.Local)
	off := peak.Add(5 * time.Hour)
	own := 7.0
	cases := []struct {
		at     time.Time
		key    string
		own    *float64
		want   float64
		custom float64
	}{
		{peak, first, nil, 3, 15},
		{off, first, nil, 1, 5},
		{peak, second, nil, 18, 15},
		{off, second, nil, 6, 5},
		{peak, "", nil, 12, 15},
		{peak, first, &own, 7, 7},
	}
	cost, total := NewCoster(), 0.0
	var recs []Record
	for i, tc := range cases {
		r := Record{Time: tc.at.Add(time.Duration(i) * time.Second), Provider: p.ID,
			Model: "m", ProviderKeyID: tc.key, Input: 1_000_000, OwnCost: tc.own, Status: 200}
		recs = append(recs, r)
		Append(r)
		if got, ok := cost(r); !ok || got != tc.want {
			t.Errorf("case %d: cost %g priced=%t, want %g", i, got, ok, tc.want)
		}
		total += tc.want
	}
	if got := NewPricer()(recs).Cost; got != total {
		t.Errorf("ledger cost %g, want %g", got, total)
	}
	if page := QueryPage(All, Filter{}, 0, 100); len(page.Rows) != len(cases) || page.Sum.Cost != total {
		t.Errorf("request page rows=%d cost=%g, want %d and %g", len(page.Rows), page.Sum.Cost, len(cases), total)
	}
	if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{
		"relay/m": {Input: new(5.0), Output: new(0.0), CacheRead: new(0.0), CacheWrite: new(0.0)},
	}}); err != nil {
		t.Fatal(err)
	}
	cost = NewCoster()
	for i, r := range recs {
		if got, ok := cost(r); !ok || got != cases[i].custom {
			t.Errorf("explicit price case %d: cost %g priced=%t, want %g", i, got, ok, cases[i].custom)
		}
	}
}
