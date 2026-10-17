package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sub2api"
)

func storeFixture(t *testing.T, f file) error {
	t.Helper()
	current, err := read()
	if err != nil {
		return err
	}
	f.baseline = current.baseline
	return store(f)
}

func TestProviderConfigRejectsStaleWrite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	first, err := read()
	if err != nil {
		t.Fatal(err)
	}
	second := first
	first.Providers = []Provider{{ID: "first"}}
	if err := store(first); err != nil {
		t.Fatal(err)
	}
	second.Providers = []Provider{{ID: "stale"}}
	if err := store(second); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale absent baseline: %v", err)
	}
	current, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), append(utf8BOM, []byte("{\"providers\":[{\"id\":\"other\"}]}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store(current); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("external raw-byte change: %v", err)
	}
	if err := store(file{}); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("missing baseline: %v", err)
	}
}

func TestProviderSaveRevisionRejectsOldEditor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", PriceRate: 1}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	opened, err := Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	p.PriceRate = 2
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	opened.Name = "Old editor"
	if err := SaveWithRevision(*opened, opened.SaveRevision); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale form: %v", err)
	}
	current, _ := Find("relay")
	if current.PriceRate != 2 || current.Name != "Relay" {
		t.Fatalf("stale form changed the provider: %+v", current)
	}
	if err := SaveWithRevision(*current, ""); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("missing revision: %v", err)
	}
	current.Name = "Current editor"
	if err := SaveWithRevision(*current, current.SaveRevision); err != nil {
		t.Fatalf("current form: %v", err)
	}
}

func TestProviderSaveRejectsStaleSnapshot(t *testing.T) {
	for _, change := range []string{"form", "key"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			p := Provider{ID: "relay", Name: "Relay", Key: "first", Chat: "http://127.0.0.1:1/v1"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			pending, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "key" {
				err = AddKey(p.ID, "", "second", "")
			} else {
				current, findErr := Find(p.ID)
				if findErr != nil {
					t.Fatal(findErr)
				}
				current.Name = "New name"
				err = SaveWithRevision(*current, current.SaveRevision)
			}
			if err != nil {
				t.Fatal(err)
			}
			pending.Keys = append(pending.Keys, KeyAccount{Key: "third"})
			if err := Save(*pending); !errors.Is(err, ErrConfigConflict) {
				t.Fatalf("a local edit made from an old snapshot: %v", err)
			}
			current, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.HasKey("third") || change == "key" && !current.HasKey("second") || change == "form" && current.Name != "New name" {
				t.Fatalf("the delayed local edit overwrote a saved change: %+v", current)
			}
		})
	}
}

func TestProviderSaveRevisionKeepsNewAccountModels(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%v", clear), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ref := KeyID("k")
			p := Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"one", "two"}}
			if clear {
				p.AccountModels = map[string][]string{ref: {"one"}}
			}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			opened, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			var allow []string
			if !clear {
				allow = []string{"two"}
			}
			if err := SetAccountModels(p.ID, ref, allow); err != nil {
				t.Fatal(err)
			}
			opened.Name = "Edited"
			if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
				t.Fatalf("saving the form after its account-model picks: %v", err)
			}
			current, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Name != "Edited" || current.AccountServes(ref, "one") != clear || !current.AccountServes(ref, "two") {
				t.Fatalf("the form lost its edits or restored old model picks: %+v", current)
			}
			if current.AccountModelsAll[ref] != clear {
				t.Fatalf("the form lost the key's explicit all-models choice: %v", current.AccountModelsAll)
			}
		})
	}
}

func TestProviderSaveRevisionKeepsLatestExtraKeys(t *testing.T) {
	for _, action := range []string{"add", "remove", "rename", "off", "protocol", "weight"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			p := Provider{ID: "relay", Name: "Relay", Key: "first", Keys: []KeyAccount{{Key: "second"}}, Chat: "http://127.0.0.1:1/v1"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			opened, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			ref := KeyID("second")
			switch action {
			case "add":
				err = AddKey(p.ID, "Third", "third", "")
			case "remove":
				err = RemoveKey(p.ID, ref)
			case "rename":
				err = RenameKey(p.ID, ref, "Renamed")
			case "off":
				err = SetKeyOn(p.ID, ref, false)
			case "protocol":
				err = SetKeyProtocol(p.ID, ref, Chat)
			case "weight":
				err = SetKeyWeight(p.ID, ref, 2)
			}
			if err != nil {
				t.Fatal(err)
			}
			latest, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			opened.Name = "Edited"
			if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
				t.Fatalf("saving the open form after %s: %v", action, err)
			}
			current, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Name != "Edited" || !slices.Equal(current.Keys, latest.Keys) {
				t.Fatalf("lost form edits or restored old keys: %+v", current)
			}
		})
	}
}

func TestProviderSaveRevisionKeepsIndependentAccountSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	ref := KeyID("first")
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "first", Chat: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
	opened, err := Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Find(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.AccountConcurrency = map[string]int{ref: 3}
	latest.AccountCaps = map[string]int{ref: 75}
	latest.AccountWindowCaps = map[string]map[string]int{ref: {"5h": 80}}
	latest.AccountWeights = map[string]int{ref: 2}
	latest.AccountNames = map[string]string{ref: "Personal"}
	latest.KeySpendCaps = map[string]SpendCap{ref: {USD: 5, Period: SpendTotal, Since: time.Unix(1, 0).UTC()}}
	latest.KeepLogin, latest.KeepLoginAs, latest.SwitchOnly = true, "user", true
	latest.RestartApp = true
	latest.KeepSelf = true
	latest.CapStop, latest.CapCareful, latest.Off = true, true, true
	if err := Save(*latest); err != nil {
		t.Fatal(err)
	}
	latest, err = Find(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened.Name = "Edited"
	if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
		t.Fatalf("the open form rejected separately saved settings: %v", err)
	}
	got, err := Find(opened.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.Name = opened.Name
	if !sameStored(*got, *latest) {
		t.Fatal("the form lost its edit or restored old account settings")
	}
}

func TestProviderSaveRevisionKeepsLatestDelistedAt(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	before := time.Unix(1, 0)
	p := Provider{ID: "relay", Name: "Relay", Key: "first", Chat: "http://127.0.0.1:1/v1",
		Delisted: []string{"gone"}, DelistedAt: before}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	opened, err := Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest, err := Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	latest.DelistedAt = before.Add(time.Minute)
	if err := Save(*latest); err != nil {
		t.Fatal(err)
	}
	opened.Name = "Edited"
	if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
		t.Fatalf("the background delisting timestamp blocked the open form: %v", err)
	}
	got, err := Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != opened.Name || !got.DelistedAt.Equal(latest.DelistedAt) {
		t.Fatalf("the form lost its edit or restored the old delisting timestamp: %+v", got)
	}
}

func TestProviderSaveRevisionChecksExplicitKeyAndProxyEdits(t *testing.T) {
	for _, change := range []string{"add-key", "reorder", "replace", "proxy"} {
		t.Run(change, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			p := Provider{ID: "relay", Name: "Relay", Key: "first", Keys: []KeyAccount{{Key: "second"}}, Chat: "http://127.0.0.1:1/v1"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			opened, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			first, second := KeyID("first"), KeyID("second")
			opened.Key = ""
			switch change {
			case "add-key":
				err = AddKey(p.ID, "", "third", "")
				opened.Key = "replacement"
			case "reorder":
				err = UseKey(p.ID, second)
				opened.AccountProxies = map[string]string{first: "direct"}
			case "replace":
				_, _, err = ReplaceKey(p.ID, second, "third")
				opened.AccountProxies = map[string]string{second: "direct"}
			case "proxy":
				latest, findErr := Find(p.ID)
				if findErr != nil {
					t.Fatal(findErr)
				}
				latest.AccountProxies = map[string]string{first: "direct"}
				err = Save(*latest)
				opened.AccountProxies = map[string]string{first: "http://127.0.0.1:7890"}
			}
			if err != nil {
				t.Fatal(err)
			}
			err = SaveWithRevision(*opened, opened.SaveRevision)
			if change == "replace" || change == "proxy" {
				if !errors.Is(err, ErrConfigConflict) {
					t.Fatalf("overlapping proxy edit: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unrelated key action blocked an explicit edit: %v", err)
			}
			got, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if change == "add-key" && (got.Key != "replacement" || !got.HasKey("third")) ||
				change == "reorder" && (got.Key != "second" || got.AccountProxies[first] != "direct") {
				t.Fatal("the explicit edit lost an independent key change")
			}
		})
	}
}

func TestProviderSaveRevisionKeepsSub2APISync(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := Provider{ID: "relay", Name: "Relay", Key: "first", Chat: "http://127.0.0.1:1/v1",
		Sub2API: &Sub2APIBinding{Session: "panel", Version: "before"}}
	if err := storeFixture(t, file{Providers: []Provider{p}}); err != nil {
		t.Fatal(err)
	}
	opened, err := Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	target := sub2apiTarget(p, sub2api.AccountView{ID: "panel", Base: "http://127.0.0.1:1"})
	rate := 0.15
	sample := sub2api.MonitorAccount{Sources: []sub2api.SourceSnapshot{{
		ID: p.ID, Keys: []sub2api.KeySnapshot{{ID: KeyID(p.Key), GroupID: "new", GroupName: "New group", GroupVerified: true, Rate: &rate}},
	}}}
	if _, err := syncSub2APIAccount(t.Context(), []sub2api.Target{target}, &sample); err != nil {
		t.Fatal(err)
	}
	opened.Name = "Edited"
	if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
		t.Fatalf("the background group and price update blocked the open form: %v", err)
	}
	got, err := Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	bindings := Sub2APIKeyBindings(*got)
	if got.Name != opened.Name || got.KeyPriceRates[KeyID(p.Key)] != rate || len(bindings) != 1 || bindings[0].GroupID != "new" {
		t.Fatal("the form lost its edit or restored the old group and price")
	}
}

func TestProviderConfigWriterProcess(t *testing.T) {
	shared := os.Getenv("MAGPIE_CONFIG_CAS_SHARED")
	if shared == "" {
		t.Skip("run by TestProviderConfigTwoProcesses")
	}
	t.Setenv("XDG_CONFIG_HOME", shared)
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("config-cas:ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	f.Providers[0].Name = os.Getenv("MAGPIE_CONFIG_CAS_WRITER")
	switch err := store(f); {
	case err == nil:
		fmt.Println("config-cas:saved")
	case errors.Is(err, ErrConfigConflict):
		fmt.Println("config-cas:conflict")
	default:
		t.Fatal(err)
	}
}

func TestProviderConfigTwoProcesses(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", shared)
	if err := Save(Provider{ID: "relay", Name: "Original", Key: "k", Chat: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	type writer struct {
		cmd    *exec.Cmd
		lines  *bufio.Scanner
		input  func()
		stderr bytes.Buffer
	}
	writers := make([]*writer, 2)
	for i := range writers {
		w := &writer{}
		w.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderConfigWriterProcess$", "-test.count=1")
		w.cmd.Env = append(os.Environ(), "MAGPIE_CONFIG_CAS_SHARED="+shared, fmt.Sprintf("MAGPIE_CONFIG_CAS_WRITER=writer-%d", i))
		stdout, err := w.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := w.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		w.lines = bufio.NewScanner(stdout)
		w.cmd.Stderr = &w.stderr
		w.input = func() {
			if _, err := fmt.Fprintln(stdin, "write"); err != nil {
				t.Error(err)
			}
			stdin.Close()
		}
		if err := w.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if w.cmd.ProcessState == nil {
				w.cmd.Process.Kill()
				w.cmd.Wait()
			}
		})
		if !w.lines.Scan() || w.lines.Text() != "config-cas:ready" {
			t.Fatalf("writer did not read the baseline: %q %s", w.lines.Text(), w.stderr.String())
		}
		writers[i] = w
	}
	for _, w := range writers {
		w.input()
	}
	saved, conflicted := 0, 0
	for _, w := range writers {
		for w.lines.Scan() {
			switch w.lines.Text() {
			case "config-cas:saved":
				saved++
			case "config-cas:conflict":
				conflicted++
			}
		}
		if err := w.cmd.Wait(); err != nil {
			t.Fatalf("writer: %v %s", err, w.stderr.String())
		}
	}
	if saved != 1 || conflicted != 1 {
		t.Fatalf("saved=%d conflicted=%d; want one of each", saved, conflicted)
	}
	f, err := read()
	if err != nil || len(f.Providers) != 1 || !strings.HasPrefix(f.Providers[0].Name, "writer-") {
		t.Fatalf("winning write lost: %+v, %v", f.Providers, err)
	}
	if f.baseline.path != filepath.Join(shared, "magpie", "providers.json") {
		t.Fatalf("unexpected shared file: %s", f.baseline.path)
	}
}
