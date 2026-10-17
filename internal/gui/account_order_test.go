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

func TestAccountArrangeRouteAndEditorSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Windows' home: without it the real Claude sign-in is read, and its
	// models are cached for the tests after this one
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	p := provider.Provider{ID: "arrange-test", Name: "Arrange", Chat: "https://example.invalid/v1", Key: "primary", Keys: []provider.KeyAccount{{Key: "second"}}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	opened, err := provider.Find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if opened.SaveRevision == "" {
		t.Fatal("opened editor has no revision")
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	order := []string{provider.KeyID("second"), provider.KeyID("primary")}
	post := func(action string, body any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/provider/"+action, strings.NewReader(string(b))))
		return w
	}
	w := post("arrange", map[string]any{"id": p.ID, "accountOrder": order})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var state providersJSON
	if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Providers) == 0 || state.Providers[0].KeyList[0].ID != order[0] || !state.Providers[0].KeyList[0].Active {
		t.Fatal("arrange response must include the new First key")
	}
	if strings.Contains(w.Body.String(), `"key":"primary"`) || strings.Contains(w.Body.String(), `"key":"second"`) {
		t.Fatal("leaked key")
	}
	// The form opened before arranging cannot overwrite the newer provider.
	w = post("save", map[string]any{"id": p.ID, "name": p.Name, "chat": p.Chat, "saveRevision": opened.SaveRevision})
	if w.Code != http.StatusConflict {
		t.Fatalf("old editor: %d %s", w.Code, w.Body)
	}
	got, err := provider.Find(p.ID)
	if err != nil || !reflect.DeepEqual([]string{provider.KeyID(got.Key), provider.KeyID(got.Keys[0].Key)}, order) {
		t.Fatalf("old editor changed order: %v", err)
	}
	if state.Providers[0].SaveRevision == "" || state.Providers[0].SaveRevision == opened.SaveRevision {
		t.Fatal("arrange response must include a new editor revision")
	}
	// Reopening takes that revision; saving without accountOrder keeps the order.
	w = post("save", map[string]any{"id": p.ID, "name": p.Name, "chat": p.Chat, "saveRevision": state.Providers[0].SaveRevision})
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	got, err = provider.Find(p.ID)
	if err != nil || !reflect.DeepEqual([]string{provider.KeyID(got.Key), provider.KeyID(got.Keys[0].Key)}, order) {
		t.Fatalf("lost order: %v", err)
	}
	info := providerInfo(*got, nil)
	if info.KeyList[0].ID != order[0] || !info.KeyList[0].Active {
		t.Fatal("displayed first key does not match routing")
	}
	if w = post("arrange", map[string]any{"id": p.ID, "accountOrder": []string{"missing"}}); w.Code == 200 {
		t.Fatal("accepted stale order")
	}
}
