package provider

import (
	"testing"

	"github.com/yetone/magpie/internal/settings"
)

// A provider's price rate (#819) scales the list price a call is counted
// at, as a relay charging 0.8 of the official price bills; a price the user
// set for a model is what they said it costs, and stays as set.
func TestPriceRateScalesListPrice(t *testing.T) {
	priceHome(t)
	priceCatalog(t)
	if err := Save(Provider{ID: "openai", Name: "OpenAI", Chat: "https://api.openai.com/v1", Key: "k", Models: []string{"sol"}, PriceRate: 0.5}); err != nil {
		t.Fatal(err)
	}
	pr, ok := EffectivePrice("openai", "sol")
	if !ok || pr.Input != 1 || pr.Output != 5 || pr.CacheRead != 0.125 || pr.CacheWrite != 1.25 {
		t.Fatalf("at half the list price: %+v %v", pr, ok)
	}
	if err := settings.Save(settings.Settings{ModelPrices: map[string]settings.ModelPrice{"openai/sol": statePrice(3)}}); err != nil {
		t.Fatal(err)
	}
	if pr, ok := EffectivePrice("openai", "sol"); !ok || pr.Input != 3 || pr.Output != 3 {
		t.Fatalf("a price set for the model is not scaled: %+v %v", pr, ok)
	}
}

func TestPriceRateOK(t *testing.T) {
	for _, r := range []float64{0, 0.8, 1.5, 0.125, 1000} {
		if bad := PriceRateOK(r); bad != "" {
			t.Errorf("%v: %s", r, bad)
		}
	}
	for _, r := range []float64{-1, 1000.5, 0.1234} {
		if PriceRateOK(r) == "" {
			t.Errorf("%v taken", r)
		}
	}
}

func TestSub2APIKeyPriceKeepsFallbacksAndExplicitPrices(t *testing.T) {
	priceHome(t)
	priceCatalog(t)
	for _, tc := range []struct {
		name        string
		source, key float64
		actualKey   string
		want        float64
	}{
		{"key override", 2, 0.5, "confirmed", 1},
		{"missing fingerprint", 2, 0.5, "", 4},
		{"unknown fingerprint", 2, 0.5, "other", 4},
		{"inherited source", 2, 0, "confirmed", 4},
		{"default source", 0, 0, "confirmed", 2},
		{"negative imported rate", 2, -1, "confirmed", 4},
		{"overlarge imported rate", 2, 1000.5, "confirmed", 4},
		{"overprecise imported rate", 2, 0.1234, "confirmed", 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := read()
			if err != nil {
				t.Fatal(err)
			}
			f.Providers = []Provider{{ID: "relay", Key: "confirmed", Chat: "https://relay.example/v1",
				Catalog: "openai", PriceRate: tc.source, Sub2API: &Sub2APIBinding{Session: "panel",
					Keys: []Sub2APIKeyBinding{{ID: KeyID("confirmed"), GroupID: "group", PriceRate: tc.key}}}}}
			// Imported configuration can predate validation at the binding editor.
			if err := store(f); err != nil {
				t.Fatal(err)
			}
			keyID := ""
			if tc.actualKey != "" {
				keyID = KeyID(tc.actualKey)
			}
			st := settings.Settings{}
			if pr, ok := EffectiveKeyPriceIn(st, "relay", "sol", keyID); !ok || pr.Input != tc.want {
				t.Errorf("key list price %+v priced=%t, want input %g", pr, ok, tc.want)
			}
			st.ModelPrices = map[string]settings.ModelPrice{"relay/sol": statePrice(0)}
			if pr, ok := EffectiveKeyPriceIn(st, "relay", "sol", keyID); !ok || pr.Input != 0 || pr.Output != 0 {
				t.Errorf("an explicit zero price was scaled or replaced: %+v priced=%t", pr, ok)
			}
		})
	}
}

func TestSub2APIKeyPriceSurvivesUnlink(t *testing.T) {
	priceHome(t)
	priceCatalog(t)
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	keyID := KeyID("confirmed")
	p := Provider{ID: "relay", Key: "confirmed", Chat: "https://relay.example/v1",
		Catalog: "openai", PriceRate: 2, Sub2API: &Sub2APIBinding{Session: "panel", Version: "binding",
			Keys: []Sub2APIKeyBinding{{ID: keyID, GroupID: "group", PriceRate: 0.5}}}}
	f.Providers = []Provider{p}
	if err := store(f); err != nil {
		t.Fatal(err)
	}
	if err := UnbindSub2API(p.ID, p.Sub2API.Version, Sub2APIRevision(p)); err != nil {
		t.Fatal(err)
	}
	if pr, ok := EffectiveKeyPriceIn(settings.Settings{}, p.ID, "sol", keyID); !ok || pr.Input != 1 {
		t.Fatalf("unlinking the account discarded the key's local rate: %+v priced=%t", pr, ok)
	}
	if pr, ok := EffectivePriceIn(settings.Settings{}, p.ID, "sol"); !ok || pr.Input != 4 {
		t.Fatalf("unlinking changed the source's fallback rate: %+v priced=%t", pr, ok)
	}
}
