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

const compactionItem = `{"type":"compaction","encrypted_content":"gAAAAB-sealed-by-relay"}`
const compactionResponse = `{"id":"resp_c1","status":"completed","output":[` + compactionItem + `]}`
const compactionStream = "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","item":` + compactionItem + "}\n\n" +
	"event: response.completed\ndata: " + `{"type":"response.completed","response":` + compactionResponse + "}\n\n"

func TestNativeCompactionEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		stream, ok bool
		err        string
	}{
		{"response", compactionResponse, false, true, ""},
		{"compaction summary", strings.ReplaceAll(compactionResponse, `"compaction"`, `"compaction_summary"`), false, true, ""},
		{"response stream", compactionStream + "data: [DONE]\n\n", true, true, ""},
		{"multiline event", "data: " + `{"type":"response.completed",` + "\ndata: " + `"response":` + compactionResponse + "}\n\n", true, true, ""},
		{"event at EOF", strings.TrimSuffix(compactionStream, "\n\n"), true, true, ""},
		{"plain summary", `{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"The project is Orchard, written in Go."}]}]}`, false, false, ""},
		{"reasoning", strings.ReplaceAll(compactionResponse, `"compaction"`, `"reasoning"`), false, false, ""},
		{"empty", strings.ReplaceAll(compactionResponse, "gAAAAB-sealed-by-relay", ""), false, false, ""},
		{"whitespace", strings.ReplaceAll(compactionResponse, "gAAAAB-sealed-by-relay", "  "), false, false, ""},
		{"non-string", strings.ReplaceAll(compactionResponse, `"gAAAAB-sealed-by-relay"`, "123"), false, false, ""},
		{"magpie summary", strings.ReplaceAll(compactionResponse, "gAAAAB-sealed-by-relay", "magpie1:summary"), false, false, ""},
		{"padded magpie summary", strings.ReplaceAll(compactionResponse, "gAAAAB-sealed-by-relay", "  magpie1:summary"), false, false, ""},
		{"in-progress JSON", strings.ReplaceAll(compactionResponse, `"completed"`, `"in_progress"`), false, false, ""},
		{"nested lookalike", `{"status":"completed","output":[{"type":"message","content":[` + compactionItem + `]}]}`, false, false, ""},
		{"JSON error", `{"error":{"message":"compaction unsupported"}}`, false, false, "compaction unsupported"},
		{"failed JSON", `{"status":"failed","error":{"message":"cannot compact"}}`, false, false, "cannot compact"},
		{"incomplete JSON", `{"status":"incomplete","output":[` + compactionItem + `]}`, false, false, "compaction request failed"},
		{"invalid body", `<html>error</html>`, false, false, "invalid JSON"},
		{"only DONE", "data: [DONE]\n\n", true, false, "before completion"},
		{"item without completion", "data: " + `{"type":"response.output_item.done","item":` + compactionItem + "}\n\n", true, false, "before completion"},
		{"completion missing status", "data: " + `{"type":"response.completed","response":{"output":[` + compactionItem + "]}}\n\n", true, false, "before completion"},
		{"empty completed stream", "data: " + `{"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n", true, false, ""},
		{"late stream error", compactionStream + "data: " + `{"type":"error","error":{"message":"upstream compact failed"}}` + "\n\n", true, false, "upstream compact failed"},
		{"error after DONE", compactionStream + "data: [DONE]\n\ndata: " + `{"type":"error","error":{"message":"late compact failure"}}` + "\n\n", true, false, "late compact failure"},
		{"nested stream error", "data: " + `{"type":"response.completed","response":{"status":"completed","error":{"message":"sealed output lost"},"output":[` + compactionItem + "]}}\n\n", true, false, "sealed output lost"},
		{"failed stream", compactionStream + "data: " + `{"type":"response.failed","response":{"error":{"message":"compact refused"}}}` + "\n\n", true, false, "compact refused"},
		{"incomplete stream", compactionStream + "data: " + `{"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n", true, false, "compaction request failed"},
		{"late invalid JSON", compactionStream + "data: {\"type\":", true, false, "invalid JSON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, err := nativeCompactionAnswer(strings.NewReader(tc.body), tc.stream)
			if ok != tc.ok || (tc.err == "" && err != nil) || (tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err))) {
				t.Fatalf("got %v, %v; want %v, %q", ok, err, tc.ok, tc.err)
			}
		})
	}
}

func TestNativeCompactionReadLimits(t *testing.T) {
	const limit = 2 << 20
	for _, stream := range []bool{false, true} {
		body := compactionResponse
		if stream {
			body = compactionStream
		}
		for _, extra := range []int{0, 1} {
			padded := body + strings.Repeat(" ", limit-len(body)+extra)
			ok, err := nativeCompactionAnswer(strings.NewReader(padded), stream)
			if extra == 0 && (!ok || err != nil) {
				t.Fatalf("exact limit (stream %v): %v, %v", stream, ok, err)
			}
			if extra == 1 && (ok || err == nil || !strings.Contains(err.Error(), "too large")) {
				t.Fatalf("over limit (stream %v): %v, %v", stream, ok, err)
			}
		}
	}
	broken := errors.New("stream disconnected")
	if ok, err := nativeCompactionAnswer(io.MultiReader(strings.NewReader(compactionStream), iotest.ErrReader(broken)), true); ok || !errors.Is(err, broken) {
		t.Fatalf("read failure: %v, %v", ok, err)
	}
}

func TestProviderNativeCompaction(t *testing.T) {
	detectHome(t)
	dialed := dialsOut(t)
	const model = `gpt-"compact\test`
	var posts atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		if r.Method != http.MethodPost || r.URL.String() != "http://relay.invalid/v1/responses" {
			t.Errorf("not the native trigger endpoint: %s %s", r.Method, r.URL)
		}
		b, _ := io.ReadAll(r.Body)
		var body struct {
			Model      string           `json:"model"`
			Stream     bool             `json:"stream"`
			Store      bool             `json:"store"`
			Input      []map[string]any `json:"input"`
			RelayField string           `json:"relay_field"`
			Session    string           `json:"session"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Error(err)
		}
		if body.Model != model || !body.Stream || body.Store || body.RelayField != "kept" || !strings.HasPrefix(body.Session, "magpie-test-") {
			t.Errorf("form not respected: %s", b)
		}
		if len(body.Input) != 3 || body.Input[2]["type"] != "compaction_trigger" || len(b) > 4096 {
			t.Errorf("not a small compaction trigger: %s", b)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get("X-Relay") != "typed" {
			t.Errorf("wrong key or headers: %v", r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, compactionStream)
	}))
	defer proxy.Close()
	p := Provider{
		ID: "relay", Responses: "http://relay.invalid/v1", Models: []string{"gpt-old"},
		Key: "sk-other-api", KeyProtocol: Anthropic,
		Keys:           []KeyAccount{{Key: "sk-test", Protocol: Responses}},
		AccountProxies: map[string]string{KeyID("sk-test"): proxy.URL},
		Headers:        map[string]string{"X-Relay": "typed"},
		ExtraBody:      ExtraBody{"relay_field": json.RawMessage(`"kept"`), "session": json.RawMessage(`"${sessionId}"`)},
	}
	got, err := p.TestCompaction(context.Background(), []string{model})
	if err != nil || len(got) != 1 || !got[0].OK || got[0].Model != model || got[0].Protocol != Responses || got[0].Status != 200 || posts.Load() != 1 {
		t.Fatalf("got %+v, %v; posts %d", got, err, posts.Load())
	}
	if out := dialed(); len(out) != 0 {
		t.Fatalf("probe bypassed the selected key's proxy: %v", out)
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an unsaved probe wrote providers.json: %v", err)
	}
}

func TestProviderNativeCompactionFailures(t *testing.T) {
	detectHome(t)
	t.Run("HTTP failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":{"message":"compaction_trigger unsupported"}}`, http.StatusBadRequest)
		}))
		defer srv.Close()
		got, err := (Provider{Responses: srv.URL, Key: "sk-test"}).TestCompaction(context.Background(), []string{"gpt-test"})
		if err != nil || len(got) != 1 || got[0].OK || got[0].Status != 400 || got[0].Error != "compaction_trigger unsupported" {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	for _, tc := range []struct {
		name string
		p    Provider
	}{
		{"no Responses API", Provider{Chat: "http://127.0.0.1"}},
		{"invalid URL", Provider{Responses: "file:///responses"}},
		{"subscription", Provider{Responses: "http://127.0.0.1", Account: &Account{Agent: "codex"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.p.TestCompaction(context.Background(), []string{"gpt-test"}); err == nil {
				t.Fatal("invalid provider was accepted")
			}
		})
	}
	t.Run("no protocol key", func(t *testing.T) {
		p := Provider{Responses: "http://127.0.0.1:1", Key: "sk-anthropic", KeyProtocol: Anthropic}
		got, err := p.TestCompaction(context.Background(), []string{"gpt-test"})
		if err != nil || len(got) != 1 || got[0].OK || !strings.Contains(got[0].Error, "no key") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
	t.Run("canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		got, err := (Provider{Responses: "http://127.0.0.1:1", Key: "sk-test"}).TestCompaction(ctx, []string{"gpt-test"})
		if err != nil || len(got) != 1 || got[0].OK || !strings.Contains(got[0].Error, "context canceled") {
			t.Fatalf("got %+v, %v", got, err)
		}
	})
}

func TestProviderNativeCompactionModelDiscovery(t *testing.T) {
	detectHome(t)
	var lists, posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			lists.Add(1)
			io.WriteString(w, `{"data":[{"id":"gpt-discovered"}]}`)
		case "POST /v1/responses":
			posts.Add(1)
			var body struct{ Model string }
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body.Model != "gpt-discovered" {
				t.Errorf("discovered model was not used: %s", body.Model)
			}
			io.WriteString(w, compactionResponse)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	got, err := (Provider{ID: "relay", Responses: srv.URL + "/v1", Key: "sk-test"}).TestCompaction(context.Background(), nil)
	if err != nil || len(got) != 1 || !got[0].OK || got[0].Model != "gpt-discovered" || lists.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("got %+v, %v; lists %d, posts %d", got, err, lists.Load(), posts.Load())
	}
	if _, err := os.Stat(Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model discovery wrote providers.json: %v", err)
	}
}
