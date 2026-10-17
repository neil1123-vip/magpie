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
