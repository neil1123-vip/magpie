package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// Ordinary editor fixtures take the revision when the form opens.
func currentProviderForm(t *testing.T, body string) string {
	t.Helper()
	var form map[string]any
	if err := json.Unmarshal([]byte(body), &form); err != nil {
		t.Fatal(err)
	}
	if form["new"] == true || form["saveRevision"] != nil {
		return body
	}
	id, _ := form["from"].(string)
	if id == "" {
		id, _ = form["id"].(string)
	}
	if p, err := provider.Find(id); err == nil {
		form["saveRevision"] = p.SaveRevision
	}
	b, err := json.Marshal(form)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProviderEditorRejectsOldRateAndMissingRevision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := provider.Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", PriceRate: 1}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	oldForm := currentProviderForm(t, `{"id":"relay","from":"relay","name":"Old","chat":"http://127.0.0.1:1/v1","priceRate":1}`)
	p.PriceRate = 2
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	for _, form := range []string{oldForm, `{"id":"relay","name":"Missing revision","chat":"http://127.0.0.1:1/v1"}`} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(form)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("old form: %d %s", rec.Code, rec.Body)
		}
	}
	got, err := provider.Find("relay")
	if err != nil || got.PriceRate != 2 || got.Name != "Relay" {
		t.Fatalf("old form changed the provider: %+v, %v", got, err)
	}
}

func TestProviderEditorSavesAfterAddingKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]string{{"id": "one"}, {"id": "two"}}})
	}))
	defer server.Close()
	for _, action := range []string{"add", "import"} {
		t.Run(action, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			p := provider.Provider{ID: "relay", Name: "Relay", Key: "first", Chat: server.URL + "/v1", Models: []string{"one", "two"}}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			form := currentProviderForm(t, `{"id":"relay","from":"relay","name":"Unsaved name","chat":"`+p.Chat+`","models":["one"]}`)
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			rec := httptest.NewRecorder()
			keys := "second"
			if action == "import" {
				keys += "\nthird"
			}
			body, err := json.Marshal(map[string]string{"id": p.ID, "key": keys})
			if err != nil {
				t.Fatal(err)
			}
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/keys/"+action, strings.NewReader(string(body))))
			if rec.Code != http.StatusOK {
				t.Fatalf("adding keys: %d %s", rec.Code, rec.Body)
			}
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(form)))
			if rec.Code != http.StatusOK {
				t.Fatalf("saving the open form after adding keys: %d %s", rec.Code, rec.Body)
			}
			got, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != "Unsaved name" || len(got.Models) != 1 || got.Models[0] != "one" || !got.HasKey("first") || !got.HasKey("second") || action == "import" && !got.HasKey("third") {
				t.Fatalf("lost form edits or keys: %+v", got)
			}
		})
	}
}

func TestProviderEditorSavesAfterIndependentKeyActions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]string{{"id": "one"}}})
	}))
	defer server.Close()
	first, second := provider.KeyID("first"), provider.KeyID("second")
	for _, tc := range []struct {
		name, path string
		body       map[string]any
		proxy      bool
	}{
		{"first-rename", "keys/rename", map[string]any{"ref": first, "name": "Personal"}, false},
		{"first-protocol", "keys/protocol", map[string]any{"ref": first, "protocol": "chat"}, false},
		{"first-weight", "keys/weight", map[string]any{"ref": first, "weight": 2}, false},
		{"make-second-first", "keys/use", map[string]any{"ref": second}, false},
		{"first-off", "keys/off", map[string]any{"ref": first}, false},
		{"first-remove", "keys/remove", map[string]any{"ref": first}, false},
		{"arrange-second-first", "provider/arrange", map[string]any{"accountOrder": []string{second, first}}, false},
		{"extra-rename", "keys/rename", map[string]any{"ref": second, "name": "Team"}, false},
		{"extra-replace", "keys/replace", map[string]any{"ref": second, "key": "third"}, false},
		{"extra-replace-own-proxy", "keys/replace", map[string]any{"ref": second, "key": "third"}, true},
		{"extra-remove-own-proxy", "keys/remove", map[string]any{"ref": second}, true},
		{"key-concurrency", "provider/accountconcurrency", map[string]any{"account": first, "limit": 3}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			p := provider.Provider{
				ID: "relay", Name: "Relay", Key: "first",
				Keys: []provider.KeyAccount{{Key: "second"}}, Chat: server.URL + "/v1",
				AccountConcurrency: map[string]int{second: 2},
			}
			if tc.proxy {
				p.AccountProxies = map[string]string{second: "direct"}
			}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			form := currentProviderForm(t, `{"id":"relay","from":"relay","name":"Pending name","chat":"`+p.Chat+`"}`)
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			tc.body["id"] = p.ID
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/"+tc.path, strings.NewReader(string(body))))
			if rec.Code != http.StatusOK {
				t.Fatalf("independent key action: %d %s", rec.Code, rec.Body)
			}
			latest, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(form)))
			if rec.Code != http.StatusOK {
				t.Fatalf("saving the open form after a key action: %d %s", rec.Code, rec.Body)
			}
			got, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != "Pending name" || !reflect.DeepEqual(got.KeyList(), latest.KeyList()) ||
				!reflect.DeepEqual(got.AccountProxies, latest.AccountProxies) ||
				!reflect.DeepEqual(got.AccountConcurrency, latest.AccountConcurrency) {
				t.Fatalf("lost form edits or independently saved key state:\nlatest: %+v\nsaved: %+v", latest, got)
			}
		})
	}
}

func TestProviderEditorRejectsStaleKeyAndProxyEdits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, map[string]any{"data": []map[string]string{{"id": "one"}}})
	}))
	defer server.Close()
	first, second := provider.KeyID("first"), provider.KeyID("second")
	for _, tc := range []struct {
		name, action string
		mutation     map[string]string
		edit         map[string]any
	}{
		{"old-key-after-use", "use", map[string]string{"ref": second}, map[string]any{"key": "first"}},
		{"old-key-after-replace", "replace", map[string]string{"ref": first, "key": "third"}, map[string]any{"key": "first"}},
		{"old-proxy-after-replace", "replace", map[string]string{"ref": second, "key": "third"}, map[string]any{"accountProxies": map[string]string{second: "direct"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			p := provider.Provider{
				ID: "relay", Name: "Relay", Key: "first",
				Keys: []provider.KeyAccount{{Key: "second"}}, Chat: server.URL + "/v1",
				AccountProxies:     map[string]string{first: "direct", second: "direct"},
				AccountConcurrency: map[string]int{first: 2, second: 3},
			}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			saved, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			form := map[string]any{
				"id": p.ID, "from": p.ID, "name": "Stale name", "chat": p.Chat,
				"saveRevision": saved.SaveRevision,
			}
			for name, value := range tc.edit {
				form[name] = value
			}
			body, err := json.Marshal(form)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutation["id"] = p.ID
			mutation, err := json.Marshal(tc.mutation)
			if err != nil {
				t.Fatal(err)
			}
			mux := http.NewServeMux()
			providerRoutes(mux, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/keys/"+tc.action, strings.NewReader(string(mutation))))
			if rec.Code != http.StatusOK {
				t.Fatalf("independent key action: %d %s", rec.Code, rec.Body)
			}
			latest, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			rec = httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(string(body))))
			if rec.Code != http.StatusConflict {
				t.Fatalf("stale key or proxy edit: %d %s", rec.Code, rec.Body)
			}
			got, err := provider.Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Name != latest.Name || !reflect.DeepEqual(got.KeyList(), latest.KeyList()) ||
				!reflect.DeepEqual(got.AccountProxies, latest.AccountProxies) ||
				!reflect.DeepEqual(got.AccountConcurrency, latest.AccountConcurrency) {
				t.Fatalf("stale form overwrote the latest key state:\nlatest: %+v\nsaved: %+v", latest, got)
			}
		})
	}
}

func TestProviderEditorClearsAccountProxies(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := provider.Provider{
		ID: "relay", Name: "Relay", Key: "first", Chat: "http://127.0.0.1:1/v1",
		AccountProxies: map[string]string{provider.KeyID("first"): "direct"},
	}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	form := currentProviderForm(t, `{"id":"relay","from":"relay","name":"Cleared","chat":"`+p.Chat+`","accountProxies":{}}`)
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(form)))
	if rec.Code != http.StatusOK {
		t.Fatalf("clearing per-key proxies: %d %s", rec.Code, rec.Body)
	}
	got, err := provider.Find(p.ID)
	if err != nil || got.Name != "Cleared" || len(got.AccountProxies) != 0 {
		t.Fatalf("per-key proxies were not cleared: %+v, %v", got, err)
	}
}
