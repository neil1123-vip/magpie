package gui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
)

func TestProviderSearchDraft(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	type request struct{ Key, Model, Header string }
	asked := make(chan request, 8)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		asked <- request{r.Header.Get("Authorization"), body.Model, r.Header.Get("X-Relay")}
		if r.URL.Path == "/v1/messages" {
			if r.Header.Get("Authorization") != "" && r.Header.Get("X-Api-Key") != "" {
				http.Error(w, `{"error":{"message":"x-api-key 和 Authorization 不可以同时存在"}}`, http.StatusUnauthorized)
				return
			}
			io.WriteString(w, `{"content":[{"type":"server_tool_use","id":"s","name":"web_search"},{"type":"web_search_tool_result","tool_use_id":"s","content":[{"type":"web_search_result","url":"https://go.dev"}]}]}`)
			return
		}
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"output":[{"type":"web_search_call","status":"completed","action":{"type":"search","query":"Go"}}]}`)
	}))
	defer up.Close()
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	post := func(h http.Handler, action string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/provider/"+action, strings.NewReader(string(b))))
		return w
	}
	base := up.URL + "/v1"
	if w := post(mux, "save", map[string]any{"id": "search-relay", "name": "Search Relay", "key": "sk-saved", "responses": base, "models": []string{"saved-m"}, "new": true}); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	before := draftFiles(t, appdir.Config())
	for _, tc := range []struct {
		name string
		form map[string]any
		key  string
	}{
		{"new", map[string]any{"key": "sk-typed"}, "sk-typed"},
		{"saved", map[string]any{"id": "search-relay"}, "sk-saved"},
		{"replaced key", map[string]any{"id": "search-relay", "key": "sk-typed"}, "sk-typed"},
		{"copy", map[string]any{"new": true, "copyOf": "search-relay"}, "sk-saved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.form["typed"] = true
			tc.form["responses"] = base
			tc.form["picked"] = []string{"draft-m"}
			tc.form["headers"] = map[string]string{"X-Relay": "typed"}
			w := post(mux, "test-search", tc.form)
			var got struct{ Results []provider.Result }
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Results) != 1 || !got.Results[0].OK || got.Results[0].Model != "draft-m" {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			if req := <-asked; req != (request{"Bearer " + tc.key, "draft-m", "typed"}) {
				t.Fatalf("asked with %+v", req)
			}
		})
	}
	// The form's Anthropic header applies; retrying both must not save the
	// learned choice, or quietly ask the saved Responses endpoint as well.
	for _, auth := range []string{"bearer", ""} {
		w := post(mux, "test-search", map[string]any{"id": "search-relay", "typed": true, "anthropic": up.URL, "responses": "", "authHeader": auth, "picked": []string{"claude-draft"}})
		var got struct{ Results []provider.Result }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Results) != 1 || !got.Results[0].OK || got.Results[0].Protocol != provider.Anthropic {
			t.Fatalf("Anthropic auth %q: %d %s", auth, w.Code, w.Body)
		}
		if req := <-asked; req.Key != "Bearer sk-saved" || req.Model != "claude-draft" {
			t.Fatalf("Anthropic auth %q: %+v", auth, req)
		}
		if auth == "" {
			if req := <-asked; req.Key != "" || req.Model != "claude-draft" {
				t.Fatalf("Anthropic auth retry: %+v", req)
			}
		}
	}
	// Clearing a saved address tests the empty form, never the old address.
	if w := post(mux, "test-search", map[string]any{"id": "search-relay", "typed": true, "responses": "", "picked": []string{"draft-m"}}); w.Code == http.StatusOK {
		t.Fatalf("cleared endpoint was accepted: %s", w.Body)
	}
	guarded := webHandler("3999", "0123456789abcdef-search", time.Hour, mux)
	if w := post(guarded, "test-search", map[string]any{"responses": base, "key": "sk-typed", "picked": []string{"draft-m"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("no web key: %d %s", w.Code, w.Body)
	}
	select {
	case req := <-asked:
		t.Fatalf("unexpected upstream request: %+v", req)
	default:
	}
	if after := draftFiles(t, appdir.Config()); !draftSame(before, after) {
		t.Fatalf("search detection changed configuration: before %v, after %v", draftNames(before), draftNames(after))
	}
}
