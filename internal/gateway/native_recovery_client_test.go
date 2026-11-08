package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

func TestNativeRecoveryClientCompactionMetadata(t *testing.T) {
	for _, kind := range []string{"compaction", "compaction_summary", "context_compaction"} {
		t.Run(kind, func(t *testing.T) {
			output := json.RawMessage(fmt.Sprintf(`[{"type":%q,"id":"cmp_client","metadata":{"turn_id":"old-turn"},"internal_chat_message_metadata_passthrough":{"turn_id":"old-turn"},"encrypted_content":"client-seal"}]`, kind))
			body := []byte(fmt.Sprintf(`{"input":[{"type":%q,"id":"cmp_client","encrypted_content":"client-seal"}]}`, kind))
			got, err := recoveryReplace(body, output, json.RawMessage(`[{"role":"user","content":"original history"}]`))
			if err != nil || !bytes.Contains(got, []byte("original history")) || bytes.Contains(got, []byte("client-seal")) {
				t.Fatalf("client removed turn metadata: %s, %v", got, err)
			}
		})
	}
}

func TestNativeRecoveryClientRetainedPrefixKeepsImagesOnce(t *testing.T) {
	userImage, toolImage := recoveryTestImage(t, 2, 2), recoveryTestImage(t, 3, 3)
	source := json.RawMessage(fmt.Sprintf(`[
		{"type":"message","id":"old-developer","role":"developer","content":[{"type":"input_text","text":"current policy"}]},
		{"type":"message","id":"old-environment","role":"user","content":[{"type":"input_text","text":"current environment"}]},
		{"type":"message","id":"user-image","role":"user","content":[{"type":"input_text","text":"inspect this image"},{"type":"input_image","image_url":%q}]},
		{"type":"function_call","call_id":"screen-call","name":"screen","arguments":"{}"},
		{"type":"function_call_output","call_id":"screen-call","output":[{"type":"input_image","image_url":%q}]},
		{"type":"message","id":"answer","role":"assistant","content":[{"type":"output_text","text":"checked both images"}]}
	]`, userImage, toolImage))
	output := json.RawMessage(`[{"type":"compaction","id":"cmp_client","metadata":{"turn_id":"old-turn"},"encrypted_content":"client-seal"}]`)
	body := []byte(fmt.Sprintf(`{"input":[
		{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"current_tool"}]},
		{"type":"message","id":"new-developer","role":"developer","content":[{"type":"input_text","text":"current policy"}]},
		{"type":"message","id":"new-environment","role":"user","content":[{"type":"input_text","text":"current environment"}]},
		{"type":"message","id":"user-image","role":"user","content":[{"type":"input_text","text":"inspect this image"},{"type":"input_image","image_url":%q}]},
		{"type":"compaction","id":"cmp_client","encrypted_content":"client-seal"},
		{"type":"message","id":"next","role":"user","content":"continue"}
	]}`, userImage))
	got, err := recoveryReplace(body, output, source)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		Input []json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(got, &request); err != nil {
		t.Fatal(err)
	}
	if len(request.Input) != 8 || bytes.Count(got, []byte(userImage)) != 1 || bytes.Count(got, []byte(toolImage)) != 1 || bytes.Count(got, []byte("current environment")) != 1 || !bytes.Contains(request.Input[0], []byte("current_tool")) || !bytes.Contains(request.Input[7], []byte("continue")) {
		t.Fatalf("retained prefix duplicated or current request changed: items=%d, user images=%d, tool images=%d", len(request.Input), bytes.Count(got, []byte(userImage)), bytes.Count(got, []byte(toolImage)))
	}
}

func TestNativeRecoveryClientPrefixWithoutMatchingIDIsKept(t *testing.T) {
	output := json.RawMessage(`[{"type":"compaction","encrypted_content":"client-seal"}]`)
	for _, ids := range []struct{ source, current string }{
		{},
		{source: `"id":"original",`, current: `"id":"separate",`},
	} {
		source := json.RawMessage(`[{"type":"message",` + ids.source + `"role":"user","content":"same words"}]`)
		body := []byte(`{"input":[{"type":"message",` + ids.current + `"role":"user","content":"same words"},{"type":"compaction","encrypted_content":"client-seal"}]}`)
		got, err := recoveryReplace(body, output, source)
		if err != nil || bytes.Count(got, []byte("same words")) != 2 {
			t.Fatalf("content alone removed a current message: %s, %v", got, err)
		}
	}
}

func TestNativeRecoveryClientRetainedOutputAnchorsEarlierPrefix(t *testing.T) {
	const user = `{"type":"message","id":"stable-user","role":"user","content":"original request"}`
	source := json.RawMessage(`[{"type":"message","id":"old-developer","role":"developer","content":"same policy"},{"type":"message","id":"old-environment","role":"user","content":"same environment"},` + user + `,{"role":"assistant","content":"previous answer"}]`)
	output := json.RawMessage(`[` + user + `,{"type":"compaction","encrypted_content":"client-seal"}]`)
	body := []byte(`{"input":[{"type":"message","id":"new-developer","role":"developer","content":"same policy"},{"type":"message","id":"new-environment","role":"user","content":"same environment"},` + user + `,{"type":"compaction","encrypted_content":"client-seal"},{"role":"user","content":"continue"}]}`)
	got, err := recoveryReplace(body, output, source)
	if err != nil || bytes.Count(got, []byte("same policy")) != 1 || bytes.Count(got, []byte("same environment")) != 1 || bytes.Count(got, []byte("original request")) != 1 || !bytes.Contains(got, []byte("previous answer")) || !bytes.Contains(got, []byte("continue")) {
		t.Fatalf("retained output failed to anchor its preceding prefix: %s, %v", got, err)
	}
}

func TestNativeRecoveryClientRefusesChangedOrAmbiguousPrefix(t *testing.T) {
	message := func(id, text string) string {
		return fmt.Sprintf(`{"type":"message","id":%q,"role":"user","content":%q}`, id, text)
	}
	a, b := message("a", "first"), message("b", "second")
	output := json.RawMessage(`[{"type":"compaction","encrypted_content":"client-seal"}]`)
	for _, tt := range []struct {
		name, source, current string
	}{
		{"changed same ID", a, message("a", "edited")},
		{"inserted message", a + "," + b, a + "," + message("new", "inserted") + "," + b},
		{"removed message", a + "," + b, b},
		{"reordered anchors", a + "," + b, b + "," + a},
		{"duplicate source anchor", a + "," + a, a},
		{"duplicate current anchor", a, a + "," + a},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"input":[` + tt.current + `,{"type":"compaction","encrypted_content":"client-seal"}]}`)
			if got, err := recoveryReplace(body, output, json.RawMessage("["+tt.source+"]")); err == nil {
				t.Fatalf("accepted changed or ambiguous retained prefix: %s", got)
			}
		})
	}
}

func TestNativeRecoveryClientKeepsUnknownCompactionFieldsBinding(t *testing.T) {
	output := json.RawMessage(`[{"type":"compaction","id":"cmp_client","metadata":{"turn_id":"old-turn","future":"original"},"encrypted_content":"client-seal"}]`)
	source := json.RawMessage(`[{"role":"user","content":"original history"}]`)
	for _, item := range []string{
		`{"type":"compaction","id":"cmp_client","metadata":{"future":"changed"},"encrypted_content":"client-seal"}`,
		`{"type":"compaction","id":"cmp_client","encrypted_content":"client-seal"}`,
		`{"type":"compaction","id":"cmp_client","metadata":{"future":"original"},"encrypted_content":"changed-seal"}`,
		`{"type":"compaction","id":"changed-id","metadata":{"future":"original"},"encrypted_content":"client-seal"}`,
	} {
		if got, err := recoveryReplace([]byte(`{"input":[`+item+`]}`), output, source); err == nil {
			t.Fatalf("accepted a changed checkpoint field: %s", got)
		}
	}
	item := `{"type":"compaction","id":"cmp_client","metadata":{"future":"original"},"encrypted_content":"client-seal"}`
	if _, err := recoveryReplace([]byte(`{"input":[`+item+`]}`), output, source); err != nil {
		t.Fatalf("matching unknown metadata was not preserved: %v", err)
	}
}

func TestNativeRecoveryClientExcerptDoesNotReviveRetainedPrefix(t *testing.T) {
	_, target := recoverySetup(t)
	const branch = "client-excerpt"
	const old = `{"type":"message","id":"old-client","role":"user","content":"discarded client prefix"}`
	history := `{"model":"fake/m1","stream":true,"input":[` + old + `,{"role":"assistant","content":"` + strings.Repeat("old context ", 3000) + `"},{"type":"message","id":"recent-client","role":"user","content":"recent requirement"},{"role":"assistant","content":"recent reply"},{"type":"compaction_trigger"}]}`
	if w := recoveryPost(t, history, branch); w.Code != 200 {
		t.Fatalf("capture: %d %s", w.Code, w.Body)
	}
	p, err := provider.Find("other")
	if err != nil {
		t.Fatal(err)
	}
	p.Contexts["m1"] = 16000
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	next := func(seal, text string, compact bool) string {
		suffix := ""
		if compact {
			suffix = `,{"type":"compaction_trigger"}`
		}
		return fmt.Sprintf(`{"model":"other/m1","stream":true,"input":[%s,{"type":"compaction","encrypted_content":%q},{"role":"user","content":%q}%s]}`, old, seal, text, suffix)
	}
	request := next("local-C1", "continue", false)
	if w := recoveryPost(t, request, branch); w.Code != 413 || target.calls != 0 {
		t.Fatalf("full history should need an excerpt: %d %s, calls=%d", w.Code, w.Body, target.calls)
	}
	choices := RecoveryChoices()
	if len(choices) != 1 {
		t.Fatalf("excerpt choice: %+v", choices)
	}
	if err := AllowRecovery(choices[0].ID, "excerpt"); err != nil {
		t.Fatal(err)
	}
	check := func(stage, request, session string) {
		t.Helper()
		w := recoveryPost(t, request, session)
		if w.Code != 200 || bytes.Contains(target.got, []byte("discarded client prefix")) || bytes.Contains(target.got, []byte("old context")) || !bytes.Contains(target.got, []byte("recent requirement")) {
			t.Fatalf("%s revived an omitted prefix: status=%d, error=%s, old prefix=%v, recent=%v", stage, w.Code, w.Body, bytes.Contains(target.got, []byte("discarded client prefix")), bytes.Contains(target.got, []byte("recent requirement")))
		}
		if bytes.Contains(w.Body.Bytes(), []byte("discarded client prefix")) {
			t.Fatalf("%s added local matching evidence to the client response", stage)
		}
	}
	check("excerpt", request, branch)
	view, err := recoverystore.View("local", checkpointKey(branch), checkpointKey("local-C1"))
	if err != nil || !view.Lossy || view.Omitted != 2 || view.PrefixItems != 1 || bytes.Contains(view.Input, []byte("discarded client prefix")) {
		t.Fatalf("saved excerpt: lossy=%v, omitted=%d, prefix=%d, error=%v", view.Lossy, view.Omitted, view.PrefixItems, err)
	}
	stats, err := recoverystore.Stats()
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	for _, entry := range stats.Records {
		if entry.Kind == "checkpoint" && entry.Hash == checkpointKey("local-C1") {
			if err := recoverystore.DeleteID(entry.ID); err != nil {
				t.Fatal(err)
			}
			deleted = true
		}
	}
	if !deleted {
		t.Fatal("original checkpoint was not found for the independent-view check")
	}
	// Each request uses a new Server, so this reloads the persisted branch view.
	check("persisted view without original", next("local-C1", "continue after reload", false), branch)
	target.reply = nativeSealedReply("local-C2")
	check("second compaction", next("local-C1", "compact again", true), branch)
	record, err := recoverystore.Load("local", checkpointKey("local-C2"))
	if err != nil || !record.Lossy || record.PrefixItems != 1 || bytes.Contains(record.Input, []byte("discarded client prefix")) || bytes.Contains(record.Input, []byte("old context")) {
		t.Fatalf("second checkpoint revived omitted history: lossy=%v, prefix=%d, error=%v", record.Lossy, record.PrefixItems, err)
	}
	target.reply = recoveryDone
	for _, session := range []string{branch, "client-excerpt-fork", ""} {
		check("second checkpoint "+session, next("local-C2", "continue after second compaction", false), session)
	}
}

func TestNativeRecoveryClientCarriesOnlyProvenPrefix(t *testing.T) {
	const old = `{"type":"message","id":"old-client","role":"user","content":"same words"}`
	const retained = `{"type":"message","id":"recent-client","role":"user","content":"same words"}`
	const seal = `{"type":"compaction","encrypted_content":"client-seal"}`
	parent := recoverystore.Record{Lossy: true, PrefixItems: 1, Input: json.RawMessage("[" + retained + "]"), Output: json.RawMessage("[" + old + "," + retained + "," + seal + "]")}
	body := []byte(`{"input":[{"type":"additional_tools","role":"developer","tools":[]},` + string(parent.Output[1:]) + `}`)
	got, err := recoveryCarryPrefix(body, []recoverystore.Record{parent})
	if err != nil || !bytes.Equal(got, []byte("["+old+"]")) {
		t.Fatalf("upstream retained output was inherited as client evidence: %s, %v", got, err)
	}
	parent.PrefixItems = 0
	if got, err := recoveryCarryPrefix(body, []recoverystore.Record{parent}); err != nil || got != nil {
		t.Fatalf("unproven upstream output became client evidence: %s, %v", got, err)
	}
	parent.PrefixItems = 1
	if _, err := recoveryCarryPrefix(body, []recoverystore.Record{parent, parent}); err == nil {
		t.Fatal("multiple inherited prefixes were combined without a unique mapping")
	}
}

func TestNativeRecoveryClientPrefixHonorsOutputLimit(t *testing.T) {
	old := fmt.Sprintf(`{"type":"message","id":"old-client","role":"user","content":%q}`, strings.Repeat("x", recoverystore.MaxOutputBytes))
	const recent = `{"type":"message","id":"recent-client","role":"user","content":"recent requirement"}`
	const seal = `{"type":"compaction","encrypted_content":"client-seal"}`
	x := &nativeRecovery{
		branch:         "client-limit",
		original:       []byte(`{"input":[` + old + "," + seal + `]}`),
		views:          []recoverystore.Record{{Input: json.RawMessage("[" + old + "," + recent + "]"), Output: json.RawMessage("[" + seal + "]")}},
		excerptInput:   json.RawMessage("[" + recent + "]"),
		excerptOmitted: 1,
	}
	if err := x.excerpt(); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("oversized matching evidence was accepted: %v", err)
	}
}

func TestNativeRecoveryClientDisabledCopyKeepsNativeContinuation(t *testing.T) {
	writer, _ := recoverySetup(t)
	if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "client-disabled"); w.Code != 200 {
		t.Fatalf("capture: %d %s", w.Code, w.Body)
	}
	if err := recoverystore.SaveConfig(recoverystore.Config{Providers: []string{"other"}}); err != nil {
		t.Fatal(err)
	}
	writer.reply = recoveryDone
	w := recoveryPost(t, recoveryNext("fake/m1", "local-C1", "continue natively"), "client-disabled")
	if w.Code != 200 || !bytes.Contains(writer.got, []byte("local-C1")) {
		t.Fatalf("disabled local copy blocked native continuation: status=%d, error=%s", w.Code, w.Body)
	}
}
