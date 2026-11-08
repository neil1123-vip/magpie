package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

func TestRecoveryInputKeepsReadableHistory(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":"request a summary"},{"type":"reasoning","encrypted_content":"private","summary":[]},{"type":"reasoning","encrypted_content":"private","summary":[{"type":"summary_text","text":"checked files"}]},{"type":"function_call","name":"read","call_id":"a","arguments":"{\"file_id\":\"ordinary argument\"}","encrypted_function_args":[]},{"type":"function_call_output","call_id":"a","output":[{"type":"input_text","text":"result"}]},{"type":"compaction_trigger"}]}`)
	input, err := recoveryInput(body)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := recoveryItems(input)
	if len(items) != 4 || bytes.Contains(input, []byte("encrypted_content")) || bytes.Contains(input, []byte("compaction_trigger")) || !bytes.Contains(input, []byte("request a summary")) || !bytes.Contains(input, []byte("checked files")) || !bytes.Contains(input, []byte("ordinary argument")) {
		t.Fatalf("history: %s", input)
	}
}

func TestRecoveryInputRealCodexCompaction(t *testing.T) {
	body, err := os.ReadFile("testdata/codex-26.1007-compaction.json")
	if err != nil {
		t.Fatal(err)
	}
	input, err := recoveryInput(body)
	if err != nil {
		t.Fatal(err)
	}
	items, _ := recoveryItems(input)
	if len(items) != 4 || bytes.Contains(input, []byte(`"type":"compaction_trigger"`)) || !bytes.Contains(input, []byte("call_0xsoGrJ6UxCP0f5bSGlP7hrG")) {
		t.Fatalf("unexpected saved fixture: %s", input)
	}
}

func TestRecoveryInputRefusesIncompleteHistory(t *testing.T) {
	for name, body := range map[string]string{
		"remote response": `{"previous_response_id":"resp_old","input":[]}`,
		"file":            `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file_remote"}]}]}`,
		"image":           `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.test/photo.png"}]}]}`,
		"unknown":         `{"input":[{"type":"new_private_item"}]}`,
		"null text":       `{"input":[{"role":"user","content":[{"type":"input_text","text":null}]}]}`,
		"missing result":  `{"input":[{"type":"function_call","call_id":"a","name":"read","arguments":"{}"}]}`,
		"missing call":    `{"input":[{"type":"function_call_output","call_id":"a","output":"result"}]}`,
		"duplicate call":  `{"input":[{"type":"function_call","call_id":"a","arguments":"{}"},{"type":"function_call","call_id":"a","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":"result"}]}`,
		"wrong tool type": `{"input":[{"type":"function_call","call_id":"a","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"a","output":"result"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if input, err := recoveryInput([]byte(body)); err == nil {
				t.Fatalf("accepted incomplete history: %s", input)
			}
		})
	}
	input, err := recoveryInput([]byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]},{"type":"compaction","encrypted_content":"parent"}]}`))
	if err != nil || !bytes.Contains(input, []byte(`"encrypted_content":"parent"`)) {
		t.Fatalf("inline image and parent: %s, %v", input, err)
	}
}

func TestRecoveryInputKeepsToolSearchDefinitions(t *testing.T) {
	input, err := recoveryInput([]byte(`{"input":[{"type":"tool_search_call","call_id":"s","arguments":{"query":"files"},"encrypted_function_args":[ ]},{"type":"tool_search_output","call_id":"s","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]}]}`))
	if err != nil || !bytes.Contains(input, []byte(`"name":"read"`)) || bytes.Contains(input, []byte("encrypted_function_args")) {
		t.Fatalf("tool search history: %s, %v", input, err)
	}
}

func TestRecoveryReplaceRetainedMessagesOnce(t *testing.T) {
	output := json.RawMessage(`[{"type":"message","id":"old-id","status":"completed","role":"user","content":[{"type":"output_text","text":"original"}]},{"type":"compaction","encrypted_content":"sealed"}]`)
	input := json.RawMessage(`[{"role":"user","content":"original"},{"role":"assistant","content":"previous answer"}]`)
	body := []byte(`{"model":"new/model","instructions":"current instructions","tools":[{"type":"function","name":"current"}],"input":[{"role":"developer","content":"prefix"},{"role":"user","id":"new-id","content":[{"type":"input_text","text":"original"}]},{"type":"compaction","encrypted_content":"sealed"},{"role":"user","content":"continue"}]}`)
	restored, err := recoveryReplace(body, output, input)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model        string            `json:"model"`
		Instructions string            `json:"instructions"`
		Input        []json.RawMessage `json:"input"`
		Tools        []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(restored, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "new/model" || got.Instructions != "current instructions" || len(got.Tools) != 1 || len(got.Input) != 4 || strings.Count(string(restored), "original") != 1 || bytes.Contains(restored, []byte("sealed")) || !bytes.Contains(got.Input[0], []byte("prefix")) || !bytes.Contains(got.Input[3], []byte("continue")) {
		t.Fatalf("restored: %s", restored)
	}
}

func TestRecoveryReplaceRefusesAmbiguityAndChangedTools(t *testing.T) {
	seal := json.RawMessage(`[{"type":"compaction","encrypted_content":"sealed"}]`)
	if _, err := recoveryReplace([]byte(`{"input":[{"type":"compaction","encrypted_content":"sealed"},{"type":"compaction","encrypted_content":"sealed"}]}`), seal, json.RawMessage(`[]`)); err == nil {
		t.Fatal("accepted ambiguous output window")
	}
	for _, changed := range []string{
		`{"type":"function_call","call_id":"different","arguments":"{}"}`,
		`{"type":"function_call","call_id":"a","arguments":"{\"changed\":true}"}`,
	} {
		window := json.RawMessage(`[{"type":"function_call","call_id":"a","arguments":"{}"},{"type":"compaction","encrypted_content":"sealed"}]`)
		body := []byte(`{"input":[` + changed + `,{"type":"compaction","encrypted_content":"sealed"}]}`)
		if _, err := recoveryReplace(body, window, json.RawMessage(`[]`)); err == nil {
			t.Fatalf("accepted changed tool call: %s", body)
		}
	}
}

func TestNativeRecoveryRetainedVendorFields(t *testing.T) {
	a, b := recoverySetup(t)
	retained := `{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}],"internal_chat_message_metadata_passthrough":{"turn_id":"original-turn"},"encrypted_content":"message-replay"}`
	output := `[` + retained + `,{"type":"compaction","encrypted_content":"local-C1"}]`
	a.reply = sse(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","output":` + output + `}}`)
	if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "branch-a"); w.Code != 200 {
		t.Fatalf("compact: %d %s", w.Code, w.Body)
	}
	next := []byte(`{"model":"other/m1","stream":true,"input":` + strings.TrimSuffix(output, "]") + `,{"role":"user","content":"new suffix"}]}`)
	next = forVendor(provider.Provider{Responses: "https://other.test/v1"}, next, false)
	w := recoveryPost(t, string(next), "branch-a")
	if w.Code != 200 || strings.Count(string(b.got), "fix the bug") != 1 || !bytes.Contains(b.got, []byte("new suffix")) || bytes.Contains(b.got, []byte("local-C1")) {
		t.Fatalf("normalized retained window failed: %d %s; sent %s", w.Code, w.Body, b.got)
	}
}

func TestRecoveryWindowTransportFieldsKeepSemantics(t *testing.T) {
	for _, plain := range []string{
		`{"type":"message","role":"assistant","content":"answer"}`,
		`{"type":"function_call","call_id":"a","name":"read","arguments":"{}"}`,
		`{"type":"function_call_output","call_id":"a","output":"result"}`,
	} {
		transport := strings.TrimSuffix(plain, "}") + `,"internal_chat_message_metadata_passthrough":{"turn_id":"original"},"encrypted_content":"private-replay","encrypted_function_args":[]}`
		a, err := recoveryItemKey(json.RawMessage(plain))
		if err != nil {
			t.Fatal(err)
		}
		b, err := recoveryItemKey(json.RawMessage(transport))
		if err != nil || !bytes.Equal(a, b) {
			t.Fatalf("transport changed the window key: %s != %s, %v", a, b, err)
		}
		for _, field := range []string{"content", "call_id", "arguments", "output", "unknown_field"} {
			var changed map[string]any
			_ = json.Unmarshal([]byte(transport), &changed)
			changed[field] = "changed"
			raw, _ := json.Marshal(changed)
			key, err := recoveryItemKey(raw)
			if err != nil || bytes.Equal(a, key) {
				t.Fatalf("ignored semantic field %s in %s: %v", field, plain, err)
			}
		}
	}
	for _, kind := range []string{"reasoning", "compaction", "compaction_summary", "context_compaction"} {
		a, _ := recoveryItemKey(json.RawMessage(fmt.Sprintf(`{"type":%q,"encrypted_content":"original"}`, kind)))
		b, _ := recoveryItemKey(json.RawMessage(fmt.Sprintf(`{"type":%q,"encrypted_content":"changed"}`, kind)))
		if bytes.Equal(a, b) {
			t.Fatalf("ignored %s checkpoint content", kind)
		}
	}
}

func TestRecoveryExcerptKeepsCompleteToolExchange(t *testing.T) {
	last := `[ {"role":"user","content":"recent"},{"type":"function_call","call_id":"a","name":"read","arguments":"{}"},{"role":"user","content":"image resize notice"},{"type":"function_call_output","call_id":"a","output":"result"},{"role":"assistant","content":"answer"}]`
	clean, err := recoveryInput([]byte(`{"input":` + last + `}`))
	if err != nil {
		t.Fatal(err)
	}
	whole := json.RawMessage(`[{"role":"user","content":"old"},{"role":"assistant","content":"old answer"},` + last[2:])
	excerpt, omitted, err := recoveryExcerpt(whole, len(clean))
	if err != nil || omitted != 2 || !bytes.Equal(excerpt, clean) {
		t.Fatalf("excerpt: %s, omitted %d, err %v", excerpt, omitted, err)
	}
	if _, _, err := recoveryExcerpt(whole, len(clean)-1); err == nil {
		t.Fatal("split the latest tool exchange to satisfy the budget")
	}
	if _, _, err := recoveryExcerpt(json.RawMessage(`[{"type":"compaction","encrypted_content":"parent"}]`), 1000); err == nil {
		t.Fatal("excerpted an unresolved parent checkpoint")
	}
}
