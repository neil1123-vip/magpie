package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/recoverystore"
	"github.com/yetone/magpie/internal/settings"
)

func TestRenameKeepsLocalRecoveryOwnership(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Responses: "http://127.0.0.1:1/v1", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	on, off := true, false
	if err := recoverystore.Configure("relay", &on, nil); err != nil {
		t.Fatal(err)
	}
	refused := func(to string) {
		t.Helper()
		if err := Rename("relay", to); err == nil || !strings.Contains(err.Error(), "local recovery") {
			t.Fatalf("recovery ownership did not block rename: %v", err)
		}
		p, err := Find("relay")
		if err != nil || p.ID != "relay" {
			t.Fatalf("refused rename changed the ID: %+v %v", p, err)
		}
	}
	refused("renamed")
	r := recoverystore.Record{Scope: "caller", Provider: "relay", Writer: "account", Model: "m", Hash: "sealed",
		Input: json.RawMessage(`[{"role":"user","content":"saved history"}]`), Output: json.RawMessage(`[{"type":"compaction","encrypted_content":"sealed"}]`)}
	pending, err := recoverystore.Begin(r)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Cancel()
	if err := pending.Commit(r); err != nil {
		t.Fatal(err)
	}
	if err := recoverystore.Configure("relay", &off, nil); err != nil {
		t.Fatal(err)
	}
	refused("renamed")
	if err := recoverystore.Configure("relay", &on, nil); err != nil {
		t.Fatal(err)
	}
	if copy, err := recoverystore.Load(r.Scope, r.Hash); err != nil || string(copy.Input) != string(r.Input) {
		t.Fatalf("refused rename damaged the copy: %+v %v", copy, err)
	}
	if err := recoverystore.Configure("relay", &off, nil); err != nil {
		t.Fatal(err)
	}
	if err := recoverystore.Clear(); err != nil {
		t.Fatal(err)
	}
	if err := recoverystore.Configure("reused", &on, nil); err != nil {
		t.Fatal(err)
	}
	refused("reused")
	if err := recoverystore.Configure("reused", &off, nil); err != nil {
		t.Fatal(err)
	}
	if err := Rename("relay", "renamed"); err != nil {
		t.Fatalf("rename after explicit recovery removal: %v", err)
	}
}

func TestRecoveryConfigureRejectsRenamedProviderSnapshot(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Responses: "http://127.0.0.1:1/v1", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	old, err := Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	if err := Rename("relay", "renamed"); err != nil {
		t.Fatal(err)
	}
	on := true
	if err := configureRecovery(*old, &on, nil); err == nil {
		t.Fatal("the provider read before rename opted in its old ID")
	}
	config, err := recoverystore.LoadConfig()
	if err != nil || len(config.Providers) != 0 {
		t.Fatalf("stale provider opt-in was saved: %+v %v", config, err)
	}
	if id, err := ConfigureRecovery("relay", &on, nil); err != nil || id != "renamed" {
		t.Fatalf("a fresh alias did not use the current ID: %q %v", id, err)
	}
	config, err = recoverystore.LoadConfig()
	if err != nil || !slices.Equal(config.Providers, []string{"renamed"}) {
		t.Fatalf("alias opted in the old ID: %+v %v", config, err)
	}
}

func TestRecoveryConfigureRefreshesHeldProvider(t *testing.T) {
	isolate(t)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := Save(Provider{ID: "relay", Name: "Relay", Key: "k", Responses: "http://127.0.0.1:1/v1", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	release := Hold()
	defer release()
	defer Changed()
	old, err := Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	// Keep the held result fresh independently of the runner's speed.
	held.Lock()
	cached := held.built["all"]
	cached.at = time.Now().Add(time.Hour)
	held.built["all"] = cached
	held.Unlock()
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers[0].ID, f.Providers[0].Was = "renamed", []string{"relay"}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate another process: no catalog.Touched or Changed in this one.
	if err := os.WriteFile(Path(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	if cached, err := Find("relay"); err != nil || cached.ID != "relay" {
		t.Fatalf("fixture did not preserve the stale held catalog: %+v %v", cached, err)
	}
	on := true
	if err := configureRecovery(*old, &on, nil); err == nil {
		t.Fatal("the held catalog hid another process's rename")
	}
	config, err := recoverystore.LoadConfig()
	if err != nil || len(config.Providers) != 0 {
		t.Fatalf("cached provider opt-in was saved: %+v %v", config, err)
	}
}

func TestRename(t *testing.T) {
	isolate(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	for _, id := range []string{"relay", "other"} {
		if err := Save(Provider{ID: id, Name: "My " + id, Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m1", "m2"}, Fallback: []string{"relay/m2"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := SaveGroup(Group{ID: "top", Name: "Top", Members: []string{"relay/m1", "other/m1"},
		Rules: []Rule{{Use: "relay/m1", Intent: "code"}}, Classifier: "relay/m2"}); err != nil {
		t.Fatal(err)
	}
	s := settings.Load()
	s.Visible = map[string][]string{"codex": {"relay", "top"}}
	if err := settings.Save(s); err != nil {
		t.Fatal(err)
	}
	if err := catalog.SaveLive("relay", "http://127.0.0.1:1/v1", []catalog.Model{{ID: "m1"}}); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"other", "Not A Slug", "magpie", "group", "codex"} {
		if err := Rename("relay", bad); err == nil {
			t.Fatalf("renamed to %q", bad)
		}
	}
	if err := Rename("relay", "fast"); err != nil {
		t.Fatal(err)
	}
	p, err := Find("fast")
	if err != nil || p.Name != "My relay" || !slices.Equal(p.Was, []string{"relay"}) {
		t.Fatalf("%+v %v", p, err)
	}
	// the old id still reaches it, for an agent not yet told
	if q, err := Find("relay"); err != nil || q.ID != "fast" {
		t.Fatalf("%+v %v", q, err)
	}
	if q, m, ok := Resolve("relay/m1"); !ok || q.ID != "fast" || m != "m1" {
		t.Fatalf("%v %q %v", q.ID, m, ok)
	}
	if q, _ := Find("other"); !slices.Equal(q.Fallback, []string{"fast/m2"}) {
		t.Fatalf("fallback %v", q.Fallback)
	}
	g, _, _ := FindGroup("group/top")
	if !slices.Equal(g.Members, []string{"fast/m1", "other/m1"}) || g.Rules[0].Use != "fast/m1" || g.Classifier != "fast/m2" {
		t.Fatalf("%+v", g)
	}
	if v := settings.Load().Visible["codex"]; !slices.Equal(v, []string{"fast", "top"}) {
		t.Fatalf("visible %v", v)
	}
	if _, err := os.Stat(catalog.LivePath("fast")); err != nil {
		t.Fatal("the fetched list stayed behind")
	}
	if got := RenamedRef("magpie/relay/m1"); got != "magpie/fast/m1" {
		t.Fatal(got)
	}
	// a save that doesn't say keeps what it was
	p.Key = "k2"
	if err := Save(*p); err != nil {
		t.Fatal(err)
	}
	if p, _ := Find("fast"); !slices.Equal(p.Was, []string{"relay"}) {
		t.Fatalf("was %v", p.Was)
	}
	// its old id taken by another: that one is found by it
	if err := Rename("other", "relay"); err != nil {
		t.Fatal(err)
	}
	if q, _ := Find("relay"); q.Name != "My other" {
		t.Fatalf("%+v", q)
	}
	if p, _ := Find("fast"); len(p.Was) != 0 {
		t.Fatalf("was %v", p.Was)
	}
}

// An id given up by a rename and taken by another provider later keeps
// the calls recorded under it before then with the one renamed, and
// gives the new one only its own (cybercaius: a renamed provider's usage
// was shown under both ids).
func TestRenamesByWhenTheIDWasTaken(t *testing.T) {
	isolate(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := Save(Provider{ID: "deepseek", Name: "DeepSeek", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	if err := Rename("deepseek", "ds"); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Hour)
	if r := UsageRenames(); r.Of("deepseek", before) != "ds" || r.Of("deepseek", time.Now()) != "ds" || r.Timed("deepseek") {
		t.Fatalf("renamed: %+v", r)
	}
	// one added again from the list is not given the old id
	if got := FreeID("deepseek"); got != "deepseek-2" {
		t.Fatalf("free id %q", got)
	}
	// one saved under it on purpose takes it from now on
	if err := Save(Provider{ID: "deepseek", Name: "DeepSeek again", Key: "k2", Chat: "http://127.0.0.1:2/v1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	if p, _ := Find("ds"); len(p.Was) != 0 || len(p.Gave) != 1 || p.Gave[0].ID != "deepseek" {
		t.Fatalf("ds %+v", p)
	}
	r := UsageRenames()
	if got := r.Of("deepseek", before); got != "ds" {
		t.Fatalf("a call before the id was taken went to %q", got)
	}
	if got := r.Of("deepseek", time.Now().Add(time.Second)); got != "deepseek" {
		t.Fatalf("a call after went to %q", got)
	}
	if got := Renamed(); len(got) != 0 {
		t.Fatalf("refs renamed %v", got)
	}
	if got := r.Each("deepseek"); !slices.Equal(got, []string{"deepseek", "ds"}) {
		t.Fatalf("each %v", got)
	}

	// a rename onto it hands it over the same way
	if err := Rename("deepseek", "ds2"); err != nil {
		t.Fatal(err)
	}
	mid := time.Now()
	if err := Save(Provider{ID: "other", Name: "Other", Key: "k3", Chat: "http://127.0.0.1:3/v1", Models: []string{"m"}}); err != nil {
		t.Fatal(err)
	}
	if err := Rename("other", "deepseek"); err != nil {
		t.Fatal(err)
	}
	r = UsageRenames()
	if r.Of("deepseek", before) != "ds" || r.Of("deepseek", mid) != "ds2" || r.Of("deepseek", time.Now().Add(time.Second)) != "deepseek" || r.Of("other", before) != "deepseek" {
		t.Fatalf("%q %q %q %q", r.Of("deepseek", before), r.Of("deepseek", mid), r.Of("deepseek", time.Now().Add(time.Second)), r.Of("other", before))
	}

	// a providers.json that can't be read is not "nothing renamed"
	if err := os.WriteFile(Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(Path() + "*")
	for _, m := range matches {
		if m != Path() {
			os.Remove(m)
		}
	}
	if _, err := read(); err == nil {
		t.Fatal("the broken file was read")
	}
	if got := UsageRenames(); got.Of("deepseek", mid) != "ds2" || got.Of("other", before) != "deepseek" {
		t.Fatalf("unreadable: %+v", got)
	}
}

// A host's site is its registrable domain whatever its subdomain, case or
// port, and none for an address, a bare name or a tenant of a hosting
// platform's suffix (CyberCaius: leee.baby and api.leee.baby are one).
func TestSiteOf(t *testing.T) {
	for host, want := range map[string]string{
		"leee.baby": "leee.baby", "API.Leee.Baby": "leee.baby", "www.leee.baby:443": "leee.baby",
		"api.example.co.uk": "example.co.uk", "mine.vercel.app": "mine.vercel.app",
		"127.0.0.1:8080": "", "[::1]:8": "", "localhost:3000": "", "": "",
	} {
		if got := SiteOf(host); got != want {
			t.Errorf("SiteOf(%q) = %q, want %q", host, got, want)
		}
	}
}
