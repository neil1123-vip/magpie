package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
)

const searchMessage = `{"content":[{"type":"server_tool_use","id":"search-1","name":"web_search","input":{"query":"Go"}},{"type":"web_search_tool_result","tool_use_id":"search-1","content":[{"type":"web_search_result","url":"https://go.dev","title":"Go"}]}]}`
const searchResponse = `{"status":"completed","output":[{"type":"web_search_call","id":"search-1","status":"completed","action":{"type":"search","query":"Go"}}]}`
const searchResponseStream = "data: " + `{"type":"response.completed","response":` + searchResponse + "}\n\n"
const searchMessageStream = "data: " + `{"type":"content_block_start","content_block":{"type":"server_tool_use","id":"s","name":"web_search"}}` + "\n\ndata: " + `{"type":"content_block_start","content_block":{"type":"web_search_tool_result","tool_use_id":"s","content":[{"type":"web_search_result","url":"https://go.dev"}]}}` + "\n\n"

func TestNativeSearchEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		proto      Protocol
		stream, ok bool
		err        string
	}{
		{"messages", searchMessage, Anthropic, false, true, ""},
		{"responses", searchResponse, Responses, false, true, ""},
		{"plain answer", `{"content":[{"type":"text","text":"I searched https://go.dev"}]}`, Anthropic, false, false, ""},
		{"function call", `{"output":[{"type":"function_call","name":"web_search","arguments":"{}"}]}`, Responses, false, false, ""},
		{"tool announced", `{"output":[{"type":"web_search_call","status":"in_progress","action":{"type":"search"}}]}`, Responses, false, false, ""},
		{"unmatched result", `{"content":[{"type":"web_search_tool_result","tool_use_id":"unknown","content":[{"type":"web_search_result","url":"https://go.dev"}]}]}`, Anthropic, false, false, ""},
		{"empty results", `{"content":[{"type":"server_tool_use","id":"s","name":"web_search"},{"type":"web_search_tool_result","tool_use_id":"s","content":[]}]}`, Anthropic, false, false, ""},
		{"tool error in 200", `{"content":[{"type":"server_tool_use","id":"s","name":"web_search"},{"type":"web_search_tool_result","tool_use_id":"s","content":{"type":"web_search_tool_result_error","error_code":"unavailable"}}]}`, Anthropic, false, false, "unavailable"},
		{"invalid body", `<html>error</html>`, Responses, false, false, "invalid JSON"},
		{"wrong protocol", searchMessage, Responses, false, false, ""},
		{"response stream", searchResponseStream + "data: [DONE]\n\n", Responses, true, true, ""},
		{"message stream", searchMessageStream + "data: {\"type\":\"message_stop\"}\n\n", Anthropic, true, true, ""},
		{"multiline event", "data: {\"type\":\"response.completed\",\ndata: \"response\":" + searchResponse + "}\n\n", Responses, true, true, ""},
		{"event at EOF", strings.TrimSuffix(searchResponseStream, "\n\n"), Responses, true, true, ""},
		{"response without completion", "data: " + `{"type":"response.output_item.done","item":{"type":"web_search_call","status":"completed","action":{"type":"search"}}}` + "\n\ndata: [DONE]\n\n", Responses, true, false, "before completion"},
		{"message without completion", searchMessageStream, Anthropic, true, false, "before completion"},
		{"completion missing status", "data: " + `{"type":"response.completed","response":{"output":[{"type":"web_search_call","status":"completed","action":{"type":"search"}}]}}` + "\n\n", Responses, true, false, "before completion"},
		{"in-progress JSON", strings.Replace(searchResponse, `"status":"completed"`, `"status":"in_progress"`, 1), Responses, false, false, ""},
		{"failed JSON", strings.Replace(searchResponse, `"status":"completed"`, `"status":"failed"`, 1), Responses, false, false, "search request failed"},
		{"incomplete JSON", strings.Replace(searchResponse, `"status":"completed"`, `"status":"incomplete"`, 1), Responses, false, false, "search request failed"},
		{"nested stream error", "data: " + `{"type":"response.completed","response":{"status":"completed","error":{"message":"search lost"},"output":[{"type":"web_search_call","status":"completed","action":{"type":"search"}}]}}` + "\n\n", Responses, true, false, "search lost"},
		{"late stream error", "data: " + `{"type":"response.completed","response":` + searchResponse + "}\n\ndata: " + `{"type":"error","error":{"message":"upstream search failed"}}` + "\n\n", Responses, true, false, "upstream search failed"},
		{"stream failure", "data: " + `{"type":"response.failed","response":{"error":{"message":"search refused"}}}` + "\n\n", Responses, true, false, "search refused"},
		{"truncated stream JSON", "data: {\"type\":", Responses, true, false, "invalid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := nativeSearchAnswer(strings.NewReader(tc.body), tc.proto, tc.stream)
			if ok != tc.ok || (tc.err == "" && err != nil) || (tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err))) {
				t.Fatalf("got %v, %v; want %v, %q", ok, err, tc.ok, tc.err)
			}
		})
	}
}

func TestNativeSearchReadLimits(t *testing.T) {
	const limit = 2 << 20
	for _, stream := range []bool{false, true} {
		body := searchResponse
		if stream {
			body = searchResponseStream + ":"
		}
		for _, extra := range []int{0, 1} {
			padded := body + strings.Repeat(" ", limit-len(body)+extra)
			ok, err := nativeSearchAnswer(strings.NewReader(padded), Responses, stream)
			if extra == 0 && (!ok || err != nil) {
				t.Fatalf("exact limit (stream %v): %v, %v", stream, ok, err)
			}
			if extra == 1 && (ok || err == nil || !strings.Contains(err.Error(), "too large")) {
				t.Fatalf("over limit (stream %v): %v, %v", stream, ok, err)
			}
		}
	}
	padded := searchResponseStream + strings.Repeat("\r\n", limit/2)
	if ok, err := nativeSearchAnswer(strings.NewReader(padded), Responses, true); ok || err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("CRLF over limit: %v, %v", ok, err)
	}
	broken := errors.New("search disconnected")
	if ok, err := nativeSearchAnswer(io.MultiReader(strings.NewReader(searchResponseStream), iotest.ErrReader(broken)), Responses, true); ok || !errors.Is(err, broken) {
		t.Fatalf("read failure: %v, %v", ok, err)
	}
}

func TestProviderNativeSearch(t *testing.T) {
	detectHome(t)
	for _, proto := range []Protocol{Anthropic, Responses} {
		t.Run(string(proto), func(t *testing.T) {
			posts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
					return
				}
				posts++
				b, _ := io.ReadAll(r.Body)
				var body map[string]any
				if err := json.Unmarshal(b, &body); err != nil {
					t.Error(err)
				}
				if body["model"] != "claude-test" || body["stream"] != true || body["relay_field"] != "kept" || r.Header.Get("X-Relay") != "typed" {
					t.Errorf("form not respected: %s, headers %v", b, r.Header)
				}
				if r.Header.Get("Authorization") != "Bearer sk-test" {
					t.Error("missing auth")
				}
				if proto == Anthropic {
					if r.URL.Path != "/v1/messages" || r.Header.Get("anthropic-version") != "2023-06-01" || !strings.Contains(string(b), `"web_search_20250305"`) {
						t.Errorf("not an Anthropic search: %s %s", r.URL.Path, b)
					}
					io.WriteString(w, searchMessage)
				} else {
					if r.URL.Path != "/v1/responses" || r.Header.Get("anthropic-version") != "" || body["tool_choice"] != "required" || !strings.Contains(string(b), `"type":"web_search"`) {
						t.Errorf("not a Responses search: %s %s", r.URL.Path, b)
					}
					w.Header().Set("Content-Type", "text/event-stream")
					io.WriteString(w, "data: "+`{"type":"response.completed","response":`+searchResponse+"}\n\n")
				}
			}))
			defer srv.Close()
			p := Provider{ID: "relay", Key: "sk-test", Models: []string{"claude-old"}, Headers: map[string]string{"X-Relay": "typed"}, ExtraBody: ExtraBody{"relay_field": json.RawMessage(`"kept"`)}}
			if proto == Anthropic {
				p.Anthropic = srv.URL
			} else {
				p.Responses = srv.URL + "/v1"
			}
			got, err := p.TestSearch(context.Background(), []string{"claude-test"})
			if err != nil || len(got) != 1 || !got[0].OK || got[0].Model != "claude-test" || posts != 1 {
				t.Fatalf("got %+v, %v; posts %d", got, err, posts)
			}
		})
	}
	t.Run("HTTP failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"search tool unsupported"}}`, http.StatusBadRequest)
		}))
		defer srv.Close()
		got, err := (Provider{Responses: srv.URL, Key: "sk-test"}).TestSearch(context.Background(), []string{"gpt-test"})
		if err != nil || len(got) != 1 || got[0].OK || got[0].Status != 400 || got[0].Error != "search tool unsupported" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("no native API", func(t *testing.T) {
		if _, err := (Provider{Chat: "http://127.0.0.1"}).TestSearch(context.Background(), nil); err == nil {
			t.Fatal("a Chat-only endpoint cannot prove native search")
		}
	})
}

func TestNativeProbeModelDiscoveryUsesProtocolKey(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto Protocol
		probe func(Provider, context.Context, []string) ([]Result, error)
	}{
		{"search responses", Responses, Provider.TestSearch},
		{"search anthropic", Anthropic, Provider.TestSearch},
		{"compaction", Responses, Provider.TestCompaction},
	} {
		t.Run(tc.name, func(t *testing.T) {
			detectHome(t)
			dialed := dialsOut(t)
			var lists, posts atomic.Int32
			host, model, body := "responses.invalid", "gpt-discovered", searchResponse
			if tc.proto == Anthropic {
				host, model, body = "anthropic.invalid", "claude-discovered", searchMessage
			} else if tc.name == "compaction" {
				body = compactionResponse
			}
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Host != host || r.Header.Get("Authorization") != "Bearer sk-probe" {
					t.Errorf("wrong endpoint or key: %s %s", r.URL, r.Header.Get("Authorization"))
					http.Error(w, "wrong key", http.StatusUnauthorized)
					return
				}
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models") {
					lists.Add(1)
					io.WriteString(w, `{"data":[{"id":"`+model+`"}]}`)
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
					http.NotFound(w, r)
					return
				}
				posts.Add(1)
				var got struct{ Model string }
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Model != model {
					t.Errorf("discovered model: %+v, %v", got, err)
				}
				io.WriteString(w, body)
			}))
			defer proxy.Close()
			p := Provider{
				ID: "relay", Key: "sk-other", KeyProtocol: Chat,
				Responses: "http://responses.invalid/v1", Anthropic: "http://anthropic.invalid",
				Keys:  []KeyAccount{{Key: "sk-probe", Protocol: tc.proto}},
				Proxy: "http://127.0.0.1:1", AccountProxies: map[string]string{KeyID("sk-probe"): proxy.URL},
			}
			if tc.proto == Anthropic {
				p.Responses = ""
			} else {
				p.Anthropic = ""
			}
			got, err := tc.probe(p, context.Background(), nil)
			if err != nil || len(got) != 1 || !got[0].OK || got[0].Model != model || lists.Load() != 1 || posts.Load() != 1 {
				t.Fatalf("got %+v, %v; lists %d, posts %d", got, err, lists.Load(), posts.Load())
			}
			if out := dialed(); len(out) != 0 {
				t.Fatalf("probe bypassed the key's proxy: %v", out)
			}
			if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("probe wrote providers.json: %v", err)
			}
		})
	}
}
