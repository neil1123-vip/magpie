package gui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/provider"
)

func TestProviderCompactionDraft(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	type request struct {
		Path, Key, Model, Header, Extra string
		Trigger, Stream                 bool
	}
	asked := make(chan request, 16)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
				io.WriteString(w, `{"data":[{"id":"saved-m"}]}`)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		var body struct {
			Model      string
			Stream     bool
			RelayField string `json:"relay_field"`
			Input      []struct{ Type string }
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		trigger := false
		for _, item := range body.Input {
			trigger = trigger || item.Type == "compaction_trigger"
		}
		asked <- request{r.URL.Path, r.Header.Get("Authorization"), body.Model, r.Header.Get("X-Relay"), body.RelayField, trigger, body.Stream}
		if r.URL.Path != "/draft/v1/responses" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"completed","output":[{"type":"compaction","encrypted_content":"sealed-upstream-compaction"}]}`)
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
	if w := post(mux, "save", map[string]any{
		"id": "compaction-relay", "name": "Compaction Relay", "key": "sk-saved",
		"responses": up.URL + "/saved/v1", "models": []string{"saved-m"}, "new": true,
		"headers": map[string]string{"X-Relay": "saved"}, "extraBody": map[string]any{"relay_field": "saved"},
	}); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	before := draftFiles(t, appdir.Config())
	for _, tc := range []struct {
		name string
		form map[string]any
		key  string
	}{
		{"new", map[string]any{"new": true, "key": "sk-typed"}, "sk-typed"},
		{"saved", map[string]any{"id": "compaction-relay"}, "sk-saved"},
		{"replaced key", map[string]any{"id": "compaction-relay", "key": "sk-typed"}, "sk-typed"},
		{"copy", map[string]any{"new": true, "copyOf": "compaction-relay"}, "sk-saved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.form["typed"] = true
			tc.form["responses"] = up.URL + "/draft/v1"
			tc.form["picked"] = []string{"draft-m"}
			tc.form["headers"] = map[string]string{"X-Relay": "typed"}
			tc.form["extraBody"] = map[string]any{"relay_field": "typed"}
			w := post(mux, "test-compaction", tc.form)
			var got struct{ Results []provider.Result }
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Results) != 1 || !got.Results[0].OK || got.Results[0].Model != "draft-m" || got.Results[0].Protocol != provider.Responses {
				t.Fatalf("%d %s", w.Code, w.Body)
			}
			want := request{"/draft/v1/responses", "Bearer " + tc.key, "draft-m", "typed", "typed", true, true}
			select {
			case req := <-asked:
				if req != want {
					t.Fatalf("asked with %+v, want %+v", req, want)
				}
			default:
				t.Fatal("no upstream compaction request")
			}
		})
	}
	// A cleared Responses endpoint cannot fall back to the saved endpoint.
	if w := post(mux, "test-compaction", map[string]any{"id": "compaction-relay", "typed": true, "responses": "", "picked": []string{"draft-m"}}); w.Code == http.StatusOK {
		t.Fatalf("cleared endpoint was accepted: %s", w.Body)
	}
	guarded := webHandler("3999", "0123456789abcdef-compaction", time.Hour, mux)
	if w := post(guarded, "test-compaction", map[string]any{"responses": up.URL + "/draft/v1", "key": "sk-typed", "picked": []string{"draft-m"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("no web key: %d %s", w.Code, w.Body)
	}
	select {
	case req := <-asked:
		t.Fatalf("unexpected upstream request: %+v", req)
	default:
	}
	if after := draftFiles(t, appdir.Config()); !draftSame(before, after) {
		t.Fatalf("compaction detection changed configuration: before %v, after %v", draftNames(before), draftNames(after))
	}
}

func TestNativeProbeDraftProxies(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var oldCalls, newCalls atomic.Int32
	oldProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oldCalls.Add(1)
		http.Error(w, `{"error":{"message":"saved proxy was used"}}`, http.StatusBadGateway)
	}))
	defer oldProxy.Close()
	newProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		newCalls.Add(1)
		if r.Method != http.MethodPost || r.URL.String() != "http://relay.invalid/v1/responses" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"status":"completed","output":[{"type":"compaction","encrypted_content":"sealed-upstream"},{"type":"web_search_call","status":"completed","action":{"type":"search"}}]}`)
	}))
	defer newProxy.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = netproxy.Func
	oldTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = netproxy.Dispatch(base)
	t.Cleanup(func() {
		http.DefaultClient.Transport = oldTransport
		base.CloseIdleConnections()
	})
	keyID := provider.KeyID("sk-saved")
	if err := provider.Save(provider.Provider{
		ID: "proxy-relay", Name: "Proxy Relay", Key: "sk-saved",
		Responses: "http://relay.invalid/v1", Models: []string{"gpt-test"},
		Proxy: oldProxy.URL, AccountProxies: map[string]string{keyID: oldProxy.URL},
	}); err != nil {
		t.Fatal(err)
	}
	before := draftFiles(t, appdir.Config())
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	for _, action := range []string{"test-search", "test-compaction"} {
		for _, own := range []bool{true, false} {
			proxies := map[string]string{}
			proxy := newProxy.URL
			if own {
				proxies[keyID] = newProxy.URL
				proxy = oldProxy.URL
			}
			b, err := json.Marshal(map[string]any{
				"id": "proxy-relay", "responses": "http://relay.invalid/v1",
				"picked": []string{"gpt-test"}, "proxy": proxy, "accountProxies": proxies,
			})
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/provider/"+action, strings.NewReader(string(b))))
			var got struct{ Results []provider.Result }
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil || len(got.Results) != 1 || !got.Results[0].OK {
				t.Errorf("%s own=%v: %d %s", action, own, w.Code, w.Body)
			}
		}
	}
	if oldCalls.Load() != 0 || newCalls.Load() != 4 {
		t.Errorf("saved proxy asked %d times, draft proxy %d; want 0, 4", oldCalls.Load(), newCalls.Load())
	}
	if after := draftFiles(t, appdir.Config()); !draftSame(before, after) {
		t.Fatal("proxy detection changed saved configuration")
	}
}
