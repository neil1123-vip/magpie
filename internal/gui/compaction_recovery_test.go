package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

func TestCompactionRecoveryLocalSettings(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Responses: "http://127.0.0.1:1/v1", Key: "k", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	compactionRecoveryRoutes(mux)
	post := func(action, body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/compaction-recovery/"+action, strings.NewReader(body)))
		return w
	}
	if on, err := recoverystore.Enabled("relay"); err != nil || on {
		t.Fatalf("new provider opted in automatically: %v %v", on, err)
	}
	if w := post("configure", `{"provider":"missing","enabled":true}`); w.Code == 200 {
		t.Fatalf("opted in a provider before it existed: %s", w.Body)
	}
	if w := post("configure", `{"provider":"relay","enabled":true}`); w.Code != 200 {
		t.Fatalf("enable: %d %s", w.Code, w.Body)
	}
	if w := post("configure", `{"maxBytes":104857600}`); w.Code != 200 {
		t.Fatalf("limit: %d %s", w.Code, w.Body)
	}
	config, err := recoverystore.LoadConfig()
	if err != nil || len(config.Providers) != 1 || config.Providers[0] != "relay" || config.MaxBytes != 104857600 {
		t.Fatalf("independent fields were overwritten: %+v %v", config, err)
	}
	p, err := provider.Find("relay")
	if err != nil || !p.NativeCompaction {
		t.Fatalf("recovery changed provider configuration: %+v %v", p, err)
	}
	if w := post("clear", `{}`); w.Code == 200 {
		t.Fatal("clear accepted without confirmation")
	}
	if w := post("delete", `{"id":"missing"}`); w.Code == 200 {
		t.Fatal("selected delete accepted without confirmation")
	}
	if w := post("clear", `{"confirm":true}`); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/compaction-recovery", nil))
	var s struct {
		Config  recoverystore.Config `json:"config"`
		Choices []json.RawMessage    `json:"choices"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || w.Code != 200 || s.Config.MaxBytes != 104857600 {
		t.Fatalf("status: %d %s %v", w.Code, w.Body, err)
	}
	for _, body := range []string{`{"enabled":true}`, `{"maxBytes":-1}`, `{"maxBytes":123,"unknown":true}`, `{"maxBytes":123} {}`} {
		if w := post("configure", body); w.Code == 200 {
			t.Fatalf("invalid configuration accepted: %s", body)
		}
	}
}

func TestCompactionRecoveryProviderIDs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"data": []any{}})
	}))
	defer upstream.Close()
	p := provider.Provider{ID: "relay", Name: "Relay", Responses: upstream.URL + "/v1", Key: "first", NativeCompaction: true}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/save", strings.NewReader(currentProviderForm(t, body))))
		return w
	}
	w := post(`{"id":"relay","name":"Relay","key":"second","responses":"` + p.Responses + `","icon":"generic","new":true,"nativeCompaction":true}`)
	var saved providersJSON
	if err := json.Unmarshal(w.Body.Bytes(), &saved); err != nil || w.Code != 200 || saved.SavedID != "relay-2" {
		t.Fatalf("Add did not return its actual ID: %d %s %v", w.Code, w.Body, err)
	}
	on := true
	if err := recoverystore.Configure("relay", &on, nil); err != nil {
		t.Fatal(err)
	}
	before, err := provider.Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	w = post(`{"id":"renamed","from":"relay","name":"Changed","responses":"` + p.Responses + `","nativeCompaction":true}`)
	if w.Code == 200 || !strings.Contains(w.Body.String(), "local recovery") {
		t.Fatalf("rename accepted recovery state: %d %s", w.Code, w.Body)
	}
	after, err := provider.Find("relay")
	if err != nil || after.ID != "relay" || after.Name != before.Name || after.SaveRevision != before.SaveRevision {
		t.Fatalf("rejected rename partially saved the draft: %+v %v", after, err)
	}
	w = post(`{"id":"relay","from":"relay","name":"Changed","responses":"` + p.Responses + `","nativeCompaction":true}`)
	if w.Code != 200 {
		t.Fatalf("display name change was blocked: %d %s", w.Code, w.Body)
	}
}
