package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yetone/magpie/internal/access"
	"github.com/yetone/magpie/internal/catalog"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

const recoveryDone = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-done\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

func recoveryPost(t *testing.T, body, session string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("Authorization", "Bearer magpie")
	if session != "" {
		r.Header.Set(SessionHeader, session)
	}
	w := httptest.NewRecorder()
	New().Front().ServeHTTP(w, r)
	return w
}

func recoverySetup(t *testing.T) (*fake, *fake) {
	t.Helper()
	a := &fake{t: t, reply: nativeSealedReply("local-C1")}
	setup(t, provider.Responses, a)
	nativeOn(t, "fake")
	b := &fake{t: t, reply: recoveryDone}
	up := httptest.NewServer(b)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "other", Name: "Other", Key: "k2", Models: []string{"m1"}, Contexts: map[string]int{"m1": 200000}, Responses: up.URL + "/v1", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	if err := recoverystore.SaveConfig(recoverystore.Config{Providers: []string{"fake", "other"}}); err != nil {
		t.Fatal(err)
	}
	return a, b
}

func recoveryNext(model, enc, text string) string {
	return fmt.Sprintf(`{"model":%q,"stream":true,"input":[{"type":"compaction","encrypted_content":%q},{"role":"user","content":%q}]}`, model, enc, text)
}

func TestNativeRecoveryDoesNotRetryForeignResourceWithStrippedHistory(t *testing.T) {
	var foreign atomic.Bool
	up := foreignUp(t, &foreign)
	s := prevSandbox(t, provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"m1"}, Responses: up.srv.URL + "/v1"})
	nativeOn(t, "fake")
	if err := recoverystore.SaveConfig(recoverystore.Config{Providers: []string{"fake"}}); err != nil {
		t.Fatal(err)
	}
	id := responseID(t, askResponses(t, s, "", `{"model":"fake/m1","input":"first turn"}`))
	foreign.Store(true)
	rec := askResponses(t, s, "", `{"model":"fake/m1","stream":true,"previous_response_id":"`+id+`","input":[{"role":"user","content":"second turn"},{"type":"compaction_trigger"}]}`)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "different Azure OpenAI resource") || up.calls() != 2 {
		t.Fatalf("foreign refusal: %d %s after %d calls, want 400 after 2", rec.Code, rec.Body, up.calls())
	}
	last := up.last()
	if _, has := last["previous_response_id"]; has {
		t.Fatal("compaction still depends on previous_response_id")
	}
	in, err := json.Marshal(last["input"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first turn", `"id":"msg_1"`, `"id":"rs_1"`, "second turn", "compaction_trigger"} {
		if !strings.Contains(string(in), want) {
			t.Fatalf("compaction history missing %q: %s", want, in)
		}
	}
}

func TestNativeRecoveryRoundTrip(t *testing.T) {
	a, b := recoverySetup(t)
	w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "branch-a")
	if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "native-backed" || a.calls != 1 {
		t.Fatalf("compact: %d %s, headers %v calls %d", w.Code, w.Body, w.Header(), a.calls)
	}
	record, err := recoverystore.Load("local", checkpointKey("local-C1"))
	if err != nil || strings.Contains(string(record.Input), "compaction_trigger") || !strings.Contains(string(record.Input), "fix the bug") {
		t.Fatalf("source: %+v, %v", record, err)
	}
	a.reply = recoveryDone
	w = recoveryPost(t, recoveryNext("fake/m1", "local-C1", "continue"), "branch-a")
	if w.Code != 200 || !strings.Contains(string(a.got), "local-C1") {
		t.Fatalf("native continuation: %d %s, sent %s", w.Code, w.Body, a.got)
	}
	w = recoveryPost(t, recoveryNext("other/m1", "local-C1", "continue"), "branch-a")
	if w.Code != 200 || b.calls != 1 || strings.Contains(string(b.got), "local-C1") || strings.Count(string(b.got), "fix the bug") != 1 || !strings.Contains(string(b.got), "continue") {
		t.Fatalf("restored: %d %s, calls %d sent %s", w.Code, w.Body, b.calls, b.got)
	}
	// A fresh Server reads the adopted branch from disk, even back on A.
	p, _ := provider.Find("fake")
	p.Contexts = map[string]int{"m1": 200000}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	w = recoveryPost(t, recoveryNext("fake/m1", "local-C1", "continue again"), "branch-a")
	if w.Code != 200 || strings.Contains(string(a.got), "local-C1") || !strings.Contains(string(a.got), "fix the bug") {
		t.Fatalf("adopted after restart: %d %s sent %s", w.Code, w.Body, a.got)
	}
}

func TestNativeRecoveryGroupAcceptsCheckpointOnlyReply(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			a, b := recoverySetup(t)
			if err := provider.SaveGroup(provider.Group{Name: "Recovery", Members: []string{"fake/m1", "other/m1"}, Routing: provider.Ordered}); err != nil {
				t.Fatal(err)
			}
			reply := `{"id":"r1","status":"completed","output":[{"type":"compaction","encrypted_content":"local-C1"}]}`
			a.reply, a.ctype = reply, "application/json"
			if stream {
				a.reply, a.ctype = sse(`data: {"type":"response.completed","response":`+reply+`}`), "text/event-stream"
			}
			b.reply = nativeSealedReply("other-C2")
			body := strings.Replace(fmt.Sprintf(compactTurn, "group/recovery"), `"stream":true`, fmt.Sprintf(`"stream":%t`, stream), 1)
			w := recoveryPost(t, body, "branch-a")
			if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "native-backed" || a.calls != 1 || b.calls != 0 || !strings.Contains(w.Body.String(), "local-C1") {
				t.Fatalf("checkpoint-only reply: %d %s, headers %v calls %d/%d", w.Code, w.Body, w.Header(), a.calls, b.calls)
			}
			record, err := recoverystore.Load("local", checkpointKey("local-C1"))
			if err != nil || record.Provider != "fake" || !strings.Contains(string(record.Input), "fix the bug") {
				t.Fatalf("first writer's copy: %+v, %v", record, err)
			}
		})
	}
}

func TestNativeRecoveryWithoutBranchIsStateless(t *testing.T) {
	a, b := recoverySetup(t)
	if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), ""); w.Code != 200 {
		t.Fatalf("compact: %d %s", w.Code, w.Body)
	}
	w := recoveryPost(t, recoveryNext("other/m1", "local-C1", "go on"), "No session ID")
	if w.Code != 200 || b.calls != 1 || w.Header().Get("X-Magpie-Recovery-State") != "replayed-this-request" {
		t.Fatalf("stateless replay: %d %s %v", w.Code, w.Body, w.Header())
	}
	a.reply = recoveryDone
	w = recoveryPost(t, recoveryNext("fake/m1", "local-C1", "go on"), "No session ID")
	if w.Code != 200 || !strings.Contains(string(a.got), "local-C1") {
		t.Fatalf("independent request: %d %s, sent %s", w.Code, w.Body, a.got)
	}
	st, err := recoverystore.Stats()
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range st.Records {
		if record.Kind == "view" {
			t.Fatal("identity-free replay saved shared branch state")
		}
	}
}

func TestNativeRecoveryNeverPublishesUncommittedSeal(t *testing.T) {
	for _, why := range []string{"full", "clear-during-upstream", "failed-stream"} {
		t.Run(why, func(t *testing.T) {
			a, _ := recoverySetup(t)
			switch why {
			case "full":
				if err := recoverystore.SaveConfig(recoverystore.Config{Providers: []string{"fake"}, MaxBytes: 1}); err != nil {
					t.Fatal(err)
				}
			case "clear-during-upstream":
				a.refuse = func([]byte) (int, string) {
					if err := recoverystore.Clear(); err != nil {
						t.Error(err)
					}
					return 0, ""
				}
			case "failed-stream":
				a.reply = sse(`data: {"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"local-C1"}}`, `data: {"type":"response.failed","response":{"status":"failed","error":{"message":"bad request"}}}`)
			}
			w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "branch-a")
			if w.Code < 400 || strings.Contains(w.Body.String(), `"encrypted_content":"local-C1"`) {
				t.Fatalf("published failed compaction: %d %s", w.Code, w.Body)
			}
			if why == "full" && a.calls != 0 {
				t.Fatalf("storage was full before %d upstream calls", a.calls)
			}
			if _, err := recoverystore.Load("local", checkpointKey("local-C1")); err == nil {
				t.Fatal("failed compression committed a copy")
			}
		})
	}
}

func TestNativeRecoveryWindowAndExcerpt(t *testing.T) {
	a, b := recoverySetup(t)
	history := `{"model":"fake/m1","stream":true,"input":[{"role":"user","content":"` + strings.Repeat("old context ", 3000) + `"},{"role":"assistant","content":"old reply"},{"role":"user","content":"recent requirement"},{"role":"assistant","content":"recent reply"},{"type":"compaction_trigger"}]}`
	if w := recoveryPost(t, history, "branch-a"); w.Code != 200 {
		t.Fatalf("compact: %d %s", w.Code, w.Body)
	}
	p, _ := provider.Find("other")
	p.Contexts["m1"] = 16000
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	next := recoveryNext("other/m1", "local-C1", "continue")
	w := recoveryPost(t, next, "branch-a")
	if w.Code != 413 || b.calls != 0 {
		t.Fatalf("oversized history sent: %d %s calls %d", w.Code, w.Body, b.calls)
	}
	choices := RecoveryChoices()
	if len(choices) != 1 || AllowRecovery(choices[0].ID, "excerpt") != nil {
		t.Fatalf("missing excerpt choice: %+v", choices)
	}
	w = recoveryPost(t, next, "branch-a")
	if w.Code != 200 || b.calls != 1 || strings.Contains(string(b.got), "old context") || !strings.Contains(string(b.got), "recent requirement") {
		t.Fatalf("excerpt: %d %s calls %d sent %s", w.Code, w.Body, b.calls, b.got)
	}
	// C2's source is the adopted excerpt plus its suffix, never old C1.
	b.reply = nativeSealedReply("local-C2")
	var request map[string]json.RawMessage
	_ = json.Unmarshal([]byte(next), &request)
	input := strings.TrimSuffix(string(request["input"]), "]") + `,{"type":"compaction_trigger"}]`
	request["input"] = json.RawMessage(input)
	second, _ := json.Marshal(request)
	w = recoveryPost(t, string(second), "branch-a")
	if w.Code != 200 {
		t.Fatalf("second compaction: %d %s", w.Code, w.Body)
	}
	c2, err := recoverystore.Load("local", checkpointKey("local-C2"))
	if err != nil || !c2.Lossy || strings.Contains(string(c2.Input), "old context") || !strings.Contains(string(c2.Input), "continue") {
		t.Fatalf("C2 lost actual lineage: %+v %v", c2, err)
	}
	p, _ = provider.Find("fake")
	p.Contexts = map[string]int{"m1": 200000}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	a.reply = recoveryDone
	w = recoveryPost(t, recoveryNext("fake/m1", "local-C2", "after C2"), "branch-a")
	if w.Code != 200 || strings.Contains(string(a.got), "old context") || !strings.Contains(string(a.got), "recent requirement") || !strings.Contains(string(a.got), "after C2") {
		t.Fatalf("C2 restored discarded history: %d %s sent %s", w.Code, w.Body, a.got)
	}
}

func TestNativeRecoveryPreservedOutputWindow(t *testing.T) {
	a, b := recoverySetup(t)
	retained := `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}`
	output := `[` + retained + `,{"type":"compaction","encrypted_content":"local-C1"}]`
	a.reply = sse(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":` + output + `}}`)
	if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "branch-a"); w.Code != 200 {
		t.Fatalf("compact: %d %s", w.Code, w.Body)
	}
	next := `{"model":"other/m1","stream":true,"instructions":"current instructions","input":` + strings.TrimSuffix(output, "]") + `,{"role":"user","content":"new suffix"}]}`
	w := recoveryPost(t, next, "branch-a")
	if w.Code != 200 || strings.Count(string(b.got), "fix the bug") != 1 || gjson.GetBytes(b.got, "instructions").Str != "current instructions" {
		t.Fatalf("replacement duplicated history: %d %s sent %s", w.Code, w.Body, b.got)
	}
}

func TestNativeRecoveryIdentityUsesActualBranch(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r.Header.Set("session_id", "lead")
	r.Header.Set("x-codex-parent-thread-id", "lead")
	_, a := recoveryIdentity(r, []byte(`{"client_metadata":{"thread_id":"worker-a"}}`))
	_, b := recoveryIdentity(r, []byte(`{"client_metadata":{"thread_id":"worker-b"}}`))
	if a == "" || b == "" || a == b {
		t.Fatal("shared Codex session merged different worker threads")
	}
	if _, missing := recoveryIdentity(r, []byte(`{}`)); missing != "" {
		t.Fatal("shared lead session was treated as a worker identity")
	}
	r.Header.Set("thread-id", "different")
	if _, conflict := recoveryIdentity(r, []byte(`{"client_metadata":{"thread_id":"worker-a"}}`)); conflict != "" {
		t.Fatal("conflicting branch identities were silently selected")
	}
	r.Header.Del("thread-id")
	r.Header.Del("x-codex-parent-thread-id")
	r.Header.Set("session_id", strings.Repeat("x", 129))
	if _, over := recoveryIdentity(r, []byte(`{}`)); over != "" {
		t.Fatal("truncated session accepted as reliable identity")
	}
}

func TestNativeRecoveryCallerIsolationAndPermission(t *testing.T) {
	_, b := recoverySetup(t)
	_, secrets := newCaller(t, "Owner", "Other")
	postAs := func(secret, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set(SessionHeader, "shared-session")
		w := httptest.NewRecorder()
		New().Front().ServeHTTP(w, r)
		return w
	}
	if w := postAs(secrets[0], fmt.Sprintf(compactTurn, "fake/m1")); w.Code != 200 {
		t.Fatalf("owner compaction: %d %s", w.Code, w.Body)
	}
	if w := postAs(secrets[1], recoveryNext("other/m1", "local-C1", "go on")); w.Code < 400 || b.calls != 0 {
		t.Fatalf("another caller read owner's history: %d %s calls %d", w.Code, w.Body, b.calls)
	}
	if w := postAs(secrets[0], recoveryNext("other/m1", "local-C1", "go on")); w.Code != 200 || b.calls != 1 {
		t.Fatalf("owner could not restore: %d %s calls %d", w.Code, w.Body, b.calls)
	}
	// The authenticated identity's current model restrictions still govern a
	// request whose readable history is already materialized by the wrapper.
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	r = r.WithContext(access.WithIdentity(r.Context(), access.Identity{KeyID: "restricted", Models: []string{"fake/m1"}}))
	w := httptest.NewRecorder()
	New().serve(w, r, provider.Responses, []byte(`{"model":"other/m1","input":[{"role":"user","content":"restored"}]}`))
	if w.Code < 400 || b.calls != 1 {
		t.Fatalf("restricted caller reached other: %d %s", w.Code, w.Body)
	}
}

func TestNativeRecoveryConsumedChoiceIsInvalidAfterClear(t *testing.T) {
	recoverySetup(t)
	body := []byte(fmt.Sprintf(compactTurn, "fake/m1"))
	id, err := offerRecoveryChoice("local", "branch", body, "full", "native-only")
	if err != nil {
		t.Fatal(err)
	}
	if err = AllowRecovery(id, "native-only"); err != nil {
		t.Fatal(err)
	}
	ok, generation, err := consumeRecoveryChoiceAt("local", "branch", body, "native-only")
	if err != nil || !ok {
		t.Fatalf("consume: %t %v", ok, err)
	}
	x := &nativeRecovery{nativeOnly: true, choiceGeneration: generation}
	if err = recoverystore.Clear(); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
	if err = recoveryWireCheck(ctx, provider.Responses, body); !errors.Is(err, recoverystore.ErrStale) {
		t.Fatalf("old approval survived clear: %v", err)
	}
}

func TestNativeRecoveryDoesNotSendOversizeFallback(t *testing.T) {
	_, b := recoverySetup(t)
	if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "branch-a"); w.Code != 200 {
		t.Fatalf("compact: %d %s", w.Code, w.Body)
	}
	c := &fake{t: t, reply: recoveryDone}
	up := httptest.NewServer(c)
	t.Cleanup(up.Close)
	if err := provider.Save(provider.Provider{ID: "small", Name: "Small", Key: "small-key", Models: []string{"m1"}, Contexts: map[string]int{"m1": 1000}, Responses: up.URL + "/v1"}); err != nil {
		t.Fatal(err)
	}
	p, _ := provider.Find("other")
	p.Fallback = []string{"small/m1"}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	b.code, b.reply = 429, `{"error":{"message":"quota exhausted"}}`
	w := recoveryPost(t, recoveryNext("other/m1", "local-C1", "continue"), "branch-a")
	if w.Code < 400 || b.calls == 0 || c.calls != 0 {
		t.Fatalf("oversized fallback sent: %d %s b=%d c=%d", w.Code, w.Body, b.calls, c.calls)
	}
}

func TestNativeRecoverySingleExceptionDoesNotPersist(t *testing.T) {
	a, _ := recoverySetup(t)
	if err := recoverystore.SaveConfig(recoverystore.Config{Providers: []string{"fake"}, MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(compactTurn, "fake/m1")
	w := recoveryPost(t, body, "branch-a")
	if w.Code < 400 || a.calls != 0 {
		t.Fatalf("unconfirmed: %d %s calls %d", w.Code, w.Body, a.calls)
	}
	choices := RecoveryChoices()
	if len(choices) != 1 {
		t.Fatalf("choices: %+v", choices)
	}
	if err := AllowRecovery(choices[0].ID, "native-only"); err != nil {
		t.Fatal(err)
	}
	w = recoveryPost(t, body, "branch-a")
	if w.Code != 200 || a.calls != 1 || w.Header().Get("X-Magpie-Recovery-State") != "native-only" {
		t.Fatalf("confirmed exception: %d %s calls %d headers %v", w.Code, w.Body, a.calls, w.Header())
	}
	w = recoveryPost(t, body, "branch-a")
	if w.Code < 400 || a.calls != 1 {
		t.Fatalf("automatic retry reused exception: %d %s calls %d", w.Code, w.Body, a.calls)
	}
}

func TestNativeRecoveryRejectsUncapturedHistory(t *testing.T) {
	recoverySetup(t)
	original := []byte(fmt.Sprintf(compactTurn, "fake/m1"))
	changed := []byte(strings.Replace(string(original), "fix the bug", "a different conversation", 1))
	if !recoveryHistoryChanged(original, changed) {
		t.Fatal("provider middleware's replacement input was not detected")
	}
	x := &nativeRecovery{scope: "local", branch: "branch", original: original, body: original, compact: true, changedInput: true}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), nativeRecoveryKey{}, x))
	p, _ := provider.Find("fake")
	if err := New().prepareNativeRecovery(r, candidate{p: *p, model: "m1"}, changed); err == nil || x.pending != nil {
		t.Fatalf("saved a different source than the compacted input: %v", err)
	}
	x.compact, x.restored = false, true
	for _, field := range []string{"previous_response_id", "conversation"} {
		body := []byte(fmt.Sprintf(`{"%s":"remote-only-history","input":[]}`, field))
		if err := recoveryWireCheck(r.Context(), provider.Responses, body); err == nil {
			t.Fatalf("unmaterialized %s reached the upstream", field)
		}
	}
}

func TestNativeRecoveryUsesFinalOutputLimit(t *testing.T) {
	for _, tt := range []struct {
		name     string
		window   int
		override string
		wantOK   bool
		excerpt  bool
	}{
		{name: "short reply fits", window: 6000, wantOK: true},
		{name: "wire limit exceeds window", window: 20000, override: "12288", excerpt: true},
		{name: "null is not a limit", window: 20000, override: "null"},
		{name: "zero is not a limit", window: 20000, override: "0"},
		{name: "fraction is not a limit", window: 20000, override: "256.5"},
		{name: "string is not a limit", window: 20000, override: `"256"`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, b := recoverySetup(t)
			history := fmt.Sprintf(compactTurn, "fake/m1")
			if tt.excerpt {
				history = `{"model":"fake/m1","stream":true,"input":[{"role":"user","content":"` + strings.Repeat("old ", 1500) + `"},{"role":"assistant","content":"old reply"},{"role":"user","content":"recent requirement"},{"role":"assistant","content":"recent reply"},{"type":"compaction_trigger"}]}`
			}
			if w := recoveryPost(t, history, "branch-a"); w.Code != 200 {
				t.Fatalf("compact: %d %s", w.Code, w.Body)
			}
			p, _ := provider.Find("other")
			p.Contexts["m1"] = tt.window
			if tt.override != "" {
				p.ExtraBody = provider.ExtraBody{"!max_output_tokens": json.RawMessage(tt.override)}
			}
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			next := withFields([]byte(recoveryNext("other/m1", "local-C1", "continue")), map[string]any{"max_output_tokens": 256})
			w := recoveryPost(t, string(next), "branch-a")
			if tt.wantOK {
				if w.Code != 200 || b.calls != 1 || gjson.GetBytes(b.got, "max_output_tokens").Int() != 256 {
					t.Fatalf("bounded reply did not fit: %d %s calls=%d sent=%s", w.Code, w.Body, b.calls, b.got)
				}
			} else if w.Code < 400 || b.calls != 0 {
				t.Fatalf("unbudgeted reply reached upstream: %d %s calls=%d", w.Code, w.Body, b.calls)
			}
			if tt.excerpt && (w.Code != 413 || len(RecoveryChoices()) != 1) {
				t.Fatalf("wire capacity failure lost excerpt choice: %d %s choices=%v", w.Code, w.Body, RecoveryChoices())
			}
			if tt.excerpt {
				if err := AllowRecovery(RecoveryChoices()[0].ID, "excerpt"); err != nil {
					t.Fatal(err)
				}
				w = recoveryPost(t, string(next), "branch-a")
				if w.Code != 200 || b.calls != 1 || strings.Contains(string(b.got), "old reply") || !strings.Contains(string(b.got), "recent requirement") {
					t.Fatalf("excerpt ignored final output reserve: %d %s calls=%d sent=%s", w.Code, w.Body, b.calls, b.got)
				}
			}
		})
	}
}

func TestNativeRecoveryBudgetsOutputByProtocol(t *testing.T) {
	for _, tt := range []struct {
		name string
		to   provider.Protocol
		body string
		want int
	}{
		{"responses", provider.Responses, `{"max_output_tokens":256,"max_tokens":64000}`, 256},
		{"chat", provider.Chat, `{"max_tokens":512,"max_completion_tokens":1024}`, 1024},
		{"anthropic", provider.Anthropic, `{"max_tokens":512}`, 512},
		{"gemini", provider.Gemini, `{"generationConfig":{"maxOutputTokens":1024}}`, 1024},
		{"removed", provider.Responses, `{}`, 32768},
		{"wrong protocol field", provider.Responses, `{"max_tokens":256}`, 32768},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := recoveryOutputLimit([]byte(tt.body), tt.to, 32768); err != nil || got != tt.want {
				t.Fatalf("output limit = %d, %v; want %d", got, err, tt.want)
			}
			x := &nativeRecovery{restored: true, currentWindow: 20000, currentOutput: 32768}
			ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
			err := recoveryWireCheck(ctx, tt.to, []byte(tt.body))
			if (tt.want < 20000) != (err == nil) {
				t.Fatalf("wire output budget: %v, want reserve %d", err, tt.want)
			}
		})
	}
	if _, err := recoveryOutputLimit([]byte(`{}`), provider.Responses, 0); !errors.Is(err, errRecoveryWindow) {
		t.Fatalf("unknown uncapped output accepted: %v", err)
	}
}

func TestNativeRecoveryBudgetsRemovedAccountOutputLimit(t *testing.T) {
	codexSignedIn(t)
	catalog.SaveLive("codex", provider.CodexBase, []catalog.Model{{ID: "gpt-5.5", Context: 272000, Output: 64000}})
	if err := provider.Save(provider.Provider{ID: "codex", Models: []string{"gpt-5.5"}}); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetModelOutput("codex/gpt-5.5", 256); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Find("codex")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"restored"}],"max_output_tokens":256}`)
	x := &nativeRecovery{restored: true, body: body}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), nativeRecoveryKey{}, x))
	s := New()
	for _, window := range []int{20000, 1000000} {
		p.Contexts = map[string]int{"gpt-5.5": window}
		if err := s.prepareNativeRecovery(r, candidate{p: *p, model: "gpt-5.5"}, body); err != nil {
			t.Fatal(err)
		}
		wire := p.Prepare(body)
		if gjson.GetBytes(wire, "max_output_tokens").Exists() || x.currentOutput <= 8192 {
			t.Fatalf("fixture did not remove a cap below the model limit: output=%d wire=%s", x.currentOutput, wire)
		}
		err := recoveryWireCheck(r.Context(), provider.Responses, wire)
		if window == 20000 && !errors.Is(err, errRecoveryWindow) || window == 1000000 && err != nil {
			t.Fatalf("window %d ignored uncapped account output %d: %v", window, x.currentOutput, err)
		}
	}
}
