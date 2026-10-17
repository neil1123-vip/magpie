package provider

import "testing"

func TestSub2APIImportedOperationVersionIsLocal(t *testing.T) {
	here := Provider{ID: "relay", Chat: "https://panel.example/v1", Key: "sk-here",
		BalanceURL: "https://panel.example/api/v1/user/profile",
		Sub2API: &Sub2APIBinding{Session: "local", GroupID: "g", Version: "local-3",
			KeyIDs: []string{keyID("sk-here")}}}
	in := here
	in.Sub2API = cloneSub2APIBinding(here.Sub2API)
	in.Sub2API.Version = "foreign-99"
	if got := KeepSecrets(in, here, false); got.Sub2API.Version != here.Sub2API.Version {
		t.Fatal("an unchanged import replaced the local operation version")
	}
	in.Sub2API.GroupID, in.Sub2API.Version = "other", here.Sub2API.Version
	got := KeepSecrets(in, here, false)
	if got.Sub2API.Version == here.Sub2API.Version {
		t.Fatal("a changed import revived the old operation version")
	}
	first, second := importedSub2APIBinding(in), importedSub2APIBinding(in)
	if first.Version == in.Sub2API.Version || first.Version == second.Version {
		t.Fatal("new imports reused a portable operation version")
	}
}

func TestSub2APIRestoreKeepsOnlyMatchingLocalReferences(t *testing.T) {
	here := Provider{ID: "relay", Chat: "https://panel.example/v1", Key: "sk-here",
		BalanceURL: "https://panel.example/api/v1/user/profile",
		Sub2API:    &Sub2APIBinding{Session: "local", GroupID: "g", Version: "local-3", KeyIDs: []string{keyID("sk-here")}}}
	oldBackup := here
	oldBackup.Sub2API = nil
	if got := KeepSecrets(oldBackup, here, false); got.Sub2API == nil || got.Sub2API.Session != "local" {
		t.Fatal("an old backup erased a matching local panel association")
	}
	oldBackup.Key = ""
	if got := KeepSecrets(oldBackup, here, true); got.Sub2API == nil || len(got.Sub2API.KeyIDs) != 1 {
		t.Fatal("a keyless backup erased the matching local association")
	}
	otherSite := oldBackup
	otherSite.BalanceURL = "https://other.example/api/v1/user/profile"
	if got := KeepSecrets(otherSite, here, true); got.Sub2API != nil {
		t.Fatal("a local account was inherited by another balance site")
	}
	otherKey := oldBackup
	otherKey.Key = "sk-other"
	if got := KeepSecrets(otherKey, here, false); got.Sub2API != nil {
		t.Fatal("a different account inherited the local association")
	}
	carried := otherKey
	carried.Sub2API = &Sub2APIBinding{Session: "portable", GroupID: "g", Version: "portable-1",
		KeyIDs: []string{keyID("sk-here"), keyID("sk-other")}}
	got := KeepSecrets(carried, here, false)
	if got.Sub2API == nil || got.Sub2API.Session != "portable" ||
		len(got.Sub2API.KeyIDs) != 1 || got.Sub2API.KeyIDs[0] != keyID("sk-other") {
		t.Fatalf("portable reference inherited a replaced key: %+v", got.Sub2API)
	}
	if len(carried.Sub2API.KeyIDs) != 2 {
		t.Fatal("filtering a restored binding mutated the source backup")
	}
}

func TestSub2APIKeyReferencesSurviveAnUnchangedRestore(t *testing.T) {
	here := Provider{ID: "relay", Chat: "https://panel.example/v1", Key: "sk-one",
		Keys: []KeyAccount{{Key: "sk-two"}}, BalanceURL: "https://panel.example/wallet",
		Sub2API: &Sub2APIBinding{Session: "local", Version: "local-ticket", Keys: []Sub2APIKeyBinding{
			{ID: keyID("sk-one"), GroupID: "premium", PriceRate: .8},
			{ID: keyID("sk-two"), GroupID: "standard", PriceRate: 1.5},
		}}}
	in := here
	in.Sub2API = cloneSub2APIBinding(here.Sub2API)
	in.Sub2API.Version = "foreign-ticket"
	got := KeepSecrets(in, here, false)
	if got.Sub2API.Version != here.Sub2API.Version || len(got.Sub2API.Keys) != 2 {
		t.Fatal("an unchanged restore dropped a key or its local authority")
	}
	in.Sub2API.Keys[0].GroupID = "changed"
	got = KeepSecrets(in, here, false)
	if got.Sub2API.Version == here.Sub2API.Version || here.Sub2API.Keys[0].GroupID != "premium" {
		t.Fatal("a changed key reused an old operation ticket or changed the original")
	}
	in.Keys = []KeyAccount{{Key: "sk-replaced"}}
	got = KeepSecrets(in, here, false)
	if len(got.Sub2API.Keys) != 1 || got.Sub2API.Keys[0].ID != keyID("sk-one") {
		t.Fatal("a restored replacement inherited the old key's group or price")
	}
	legacy := here
	legacy.Sub2API = &Sub2APIBinding{Session: "local", GroupID: "old-group", GroupName: "Old group",
		KeyIDs: []string{keyID("sk-one"), keyID("sk-two")}, Version: "legacy"}
	view := Sub2APISafeBinding(legacy)
	if len(view.Keys) != 2 || view.Keys[0].GroupID != "old-group" || view.Keys[1].GroupID != "old-group" {
		t.Fatal("legacy selections were not expanded by key")
	}
}

func TestSub2APIOldRestoreKeepsAnUnchangedSiblingPrice(t *testing.T) {
	here := Provider{ID: "relay", Key: "sk-one", Keys: []KeyAccount{{Key: "sk-two"}},
		KeyPriceRates: map[string]float64{keyID("sk-one"): .8, keyID("sk-two"): 1.5}}
	in := here
	in.Key, in.KeyPriceRates = "sk-replaced", nil
	got := KeepSecrets(in, here, false)
	if len(got.KeyPriceRates) != 1 || got.KeyPriceRates[keyID("sk-two")] != 1.5 {
		t.Fatalf("a primary key replacement lost the unchanged sibling price: %+v", got.KeyPriceRates)
	}
	if len(here.KeyPriceRates) != 2 {
		t.Fatal("restoring a source changed the original price map")
	}
}
