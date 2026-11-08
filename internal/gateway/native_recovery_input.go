package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// recoveryInput keeps only history the gateway can replay without the
// generating account. Compaction items remain references for the caller to
// resolve; the expanded history must pass this check again.
func recoveryInput(body []byte) (json.RawMessage, error) {
	if err := recoveryMediaBounds(body); err != nil {
		return nil, err
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, fmt.Errorf("local recovery requires a Responses request object")
	}
	if previous := bytes.TrimSpace(request["previous_response_id"]); len(previous) > 0 && !bytes.Equal(previous, []byte("null")) && !bytes.Equal(previous, []byte(`""`)) {
		return nil, fmt.Errorf("local recovery requires previous_response_id to be expanded first")
	}
	items, err := recoveryItems(request["input"])
	if err != nil {
		return nil, err
	}
	out := make([]json.RawMessage, 0, len(items))
	pending, seen := map[string]string{}, map[string]bool{}
	for i, raw := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(raw, &item); err != nil || item == nil {
			return nil, fmt.Errorf("local recovery input[%d] is not an item object", i)
		}
		kind := recoveryString(item["type"])
		if kind == "" && recoveryString(item["role"]) != "" {
			kind = "message"
		}
		fail := func(err error) (json.RawMessage, error) {
			return nil, fmt.Errorf("local recovery input[%d]: %w", i, err)
		}
		switch kind {
		case "compaction_trigger":
			continue
		case "additional_tools":
			tools := gjson.ParseBytes(item["tools"])
			if !tools.IsArray() || len(item["role"]) != 0 && recoveryString(item["role"]) != "developer" {
				return fail(fmt.Errorf("additional tools require an array and developer role"))
			}
			for _, tool := range tools.Array() {
				if !tool.IsObject() {
					return fail(fmt.Errorf("additional tool is not an object"))
				}
			}
		case "compaction", "compaction_summary", "context_compaction":
			if recoveryString(item["encrypted_content"]) == "" {
				return fail(fmt.Errorf("compaction has no checkpoint content"))
			}
			out = append(out, raw)
			continue
		case "message":
			switch recoveryString(item["role"]) {
			case "system", "developer", "user", "assistant":
			default:
				return fail(fmt.Errorf("message has an unsupported role"))
			}
			if err := recoveryContent(item["content"]); err != nil {
				return fail(err)
			}
		case "reasoning":
			readable := false
			for _, field := range []string{"summary", "content"} {
				parts := bytes.TrimSpace(item[field])
				if len(parts) == 0 || bytes.Equal(parts, []byte("null")) {
					continue
				}
				var texts []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(parts, &texts); err != nil {
					return fail(fmt.Errorf("reasoning has unsupported readable content"))
				}
				for _, text := range texts {
					if text.Type != "summary_text" && text.Type != "reasoning_text" {
						return fail(fmt.Errorf("reasoning has unsupported part %q", text.Type))
					}
					readable = readable || text.Text != ""
				}
			}
			if !readable {
				continue // private reasoning is not portable history
			}
		case "function_call", "custom_tool_call", "tool_search_call":
			id := recoveryString(item["call_id"])
			if id == "" || seen[id] {
				return fail(fmt.Errorf("tool call has a missing or duplicate call_id"))
			}
			field := "arguments"
			if kind == "custom_tool_call" {
				field = "input"
			}
			if value := bytes.TrimSpace(item[field]); len(value) == 0 || bytes.Equal(value, []byte("null")) {
				return fail(fmt.Errorf("tool call %q has no readable %s", id, field))
			}
			pending[id], seen[id] = kind, true
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			id := recoveryString(item["call_id"])
			want := strings.TrimSuffix(kind, "_output")
			if kind == "tool_search_output" {
				want = "tool_search_call"
			}
			if id == "" || pending[id] != want {
				return fail(fmt.Errorf("tool result %q has no matching earlier call", id))
			}
			if kind == "tool_search_output" {
				var found []json.RawMessage
				if err := json.Unmarshal(item["tools"], &found); err != nil || found == nil {
					return fail(fmt.Errorf("tool search result %q has no readable tools", id))
				}
			} else if err := recoveryContent(item["output"]); err != nil {
				return fail(err)
			}
			delete(pending, id)
		default:
			return fail(fmt.Errorf("unsupported history item type %q", kind))
		}
		// Some Responses vendors attach private replay seals to otherwise
		// readable messages and calls as well as reasoning items.
		delete(item, "encrypted_content")
		if sealed := bytes.TrimSpace(item["encrypted_function_args"]); len(sealed) > 0 {
			var args []json.RawMessage
			if err := json.Unmarshal(sealed, &args); err != nil || len(args) != 0 {
				return fail(fmt.Errorf("encrypted tool arguments are not replayable"))
			}
			delete(item, "encrypted_function_args")
		}
		clean, err := json.Marshal(item)
		if err != nil {
			return fail(err)
		}
		out = append(out, clean)
	}
	if len(pending) > 0 {
		return nil, fmt.Errorf("local recovery history has %d unfinished tool call(s)", len(pending))
	}
	return json.Marshal(out)
}

func recoveryString(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}

func recoveryItems(raw json.RawMessage) ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || items == nil {
		return nil, fmt.Errorf("local recovery requires an input array")
	}
	return items, nil
}

// Check media only in protocol content parts. A tool argument or a user's
// text containing a URL or a field named file_id is ordinary history.
func recoveryContent(raw json.RawMessage) error {
	var text string
	if json.Unmarshal(raw, &text) == nil && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil || parts == nil {
		return fmt.Errorf("history content is missing or unsupported")
	}
	for _, part := range parts {
		kind := recoveryString(part["type"])
		if recoveryString(part["file_id"]) != "" {
			return fmt.Errorf("external file_id content is not saved locally")
		}
		switch kind {
		case "input_text", "output_text", "text":
			var value string
			if err := json.Unmarshal(part["text"], &value); err != nil || bytes.Equal(bytes.TrimSpace(part["text"]), []byte("null")) {
				return fmt.Errorf("text part has no readable text")
			}
		case "refusal":
			if recoveryString(part["refusal"]) == "" {
				return fmt.Errorf("refusal part has no readable text")
			}
		case "input_image", "image_url":
			url := recoveryString(part["image_url"])
			if url == "" {
				var image struct {
					URL string `json:"url"`
				}
				_ = json.Unmarshal(part["image_url"], &image)
				url = image.URL
			}
			if !strings.HasPrefix(url, "data:") || !strings.Contains(url, ",") || strings.HasSuffix(url, ",") {
				return fmt.Errorf("remote or missing image content is not saved locally")
			}
			if _, err := recoveryImage(url); err != nil {
				return err
			}
		case "input_file":
			if recoveryString(part["file_url"]) != "" || recoveryString(part["file_data"]) == "" {
				return fmt.Errorf("external or missing file content is not saved locally")
			}
		case "input_audio":
			var audio struct {
				Data string `json:"data"`
			}
			_ = json.Unmarshal(part["input_audio"], &audio)
			if audio.Data == "" {
				return fmt.Errorf("external or missing audio content is not saved locally")
			}
		default:
			return fmt.Errorf("unsupported history content part %q", kind)
		}
	}
	return nil
}

// recoveryReplace replaces the entire output window once. Keeping messages
// that the compactor retained would replay them twice after expansion.
func recoveryReplace(body []byte, output, input json.RawMessage) ([]byte, error) {
	replaced, _, err := recoveryReplacement(body, output, input, input)
	return replaced, err
}

// source proves which client prefix belongs to the window; input may be an excerpt.
// The returned window keeps that proof after the omitted source has been discarded.
func recoveryReplacement(body []byte, output, source, input json.RawMessage) ([]byte, json.RawMessage, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return nil, nil, fmt.Errorf("local recovery requires a Responses request object")
	}
	current, err := recoveryItems(request["input"])
	if err != nil {
		return nil, nil, err
	}
	window, err := recoveryItems(output)
	if err != nil || len(window) == 0 {
		return nil, nil, fmt.Errorf("local recovery has no complete output window")
	}
	original, err := recoveryItems(source)
	if err != nil {
		return nil, nil, err
	}
	restored, err := recoveryItems(input)
	if err != nil {
		return nil, nil, err
	}
	keys := make([][]byte, len(current))
	for i, item := range current {
		if keys[i], err = recoveryItemKey(item); err != nil {
			return nil, nil, err
		}
	}
	wanted := make([][]byte, len(window))
	for i, item := range window {
		if wanted[i], err = recoveryItemKey(item); err != nil {
			return nil, nil, err
		}
	}
	at := -1
	for i := 0; i+len(window) <= len(current); i++ {
		match := true
		for j := range wanted {
			if !bytes.Equal(keys[i+j], wanted[j]) {
				match = false
				break
			}
		}
		if match {
			if at >= 0 {
				return nil, nil, fmt.Errorf("local recovery output window is ambiguous")
			}
			at = i
		}
	}
	if at < 0 {
		return nil, nil, fmt.Errorf("local recovery output window does not match the current history")
	}
	// Codex can retain a source prefix outside the compactor's output window.
	// Shared message IDs must prove one alignment before consuming that prefix.
	byID := map[string]int{}
	messageID := func(raw json.RawMessage) string {
		item := gjson.ParseBytes(raw)
		kind, role := item.Get("type").Str, item.Get("role").Str
		if (kind == "" || kind == "message") && (role == "user" || role == "assistant") {
			return item.Get("id").Str
		}
		return ""
	}
	for i, raw := range original {
		if id := messageID(raw); id != "" {
			if _, exists := byID[id]; exists {
				byID[id] = -1
			} else {
				byID[id] = i
			}
		}
	}
	start := -1
	for i, raw := range current[:at] {
		if j, shared := byID[messageID(raw)]; shared {
			if j < 0 || i < j || start >= 0 && start != i-j {
				return nil, nil, fmt.Errorf("local recovery retained prefix has ambiguous message IDs")
			}
			start = i - j
		}
	}
	if start < 0 && at > 0 {
		// A retained source message can be inside the vendor's matched window.
		for i, raw := range current[at : at+len(window)] {
			if j, shared := byID[messageID(raw)]; shared {
				if j >= 0 && j <= i {
					continue // this alignment cannot prove a prefix before the window
				}
				if j < 0 || at+i < j || start >= 0 && start != at+i-j {
					return nil, nil, fmt.Errorf("local recovery retained prefix has ambiguous message IDs")
				}
				start = at + i - j
			}
		}
	}
	from := at
	if start >= 0 {
		if start > at || at-start > len(original) {
			return nil, nil, fmt.Errorf("local recovery retained prefix exceeds its source")
		}
		for j, raw := range original[:at-start] {
			key, err := recoveryItemKey(raw)
			if err != nil || !bytes.Equal(keys[start+j], key) {
				return nil, nil, fmt.Errorf("local recovery retained prefix does not match its source")
			}
		}
		from = start
	}
	if from < at {
		output, err = json.Marshal(current[from : at+len(window)])
		if err != nil {
			return nil, nil, err
		}
	}
	joined := make([]json.RawMessage, 0, len(current)-len(window)+len(restored))
	joined = append(joined, current[:from]...)
	joined = append(joined, restored...)
	joined = append(joined, current[at+len(window):]...)
	request["input"], err = json.Marshal(joined)
	if err != nil {
		return nil, nil, err
	}
	replaced, err := json.Marshal(request)
	return replaced, output, err
}

func recoveryItemKey(raw json.RawMessage) ([]byte, error) {
	var item map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&item); err != nil || item == nil {
		return nil, fmt.Errorf("local recovery window contains an invalid item")
	}
	kind, _ := item["type"].(string)
	if kind == "compaction" || kind == "compaction_summary" || kind == "context_compaction" {
		// Codex retains the checkpoint but drops its originating turn metadata.
		if metadata, ok := item["metadata"].(map[string]any); ok {
			delete(metadata, "turn_id")
			if len(metadata) == 0 {
				delete(item, "metadata")
			}
		}
	}
	// Codex drops these transport fields when changing providers (forVendor).
	delete(item, "internal_chat_message_metadata_passthrough")
	delete(item, "encrypted_function_args")
	if !keepsSeal[`"`+kind+`"`] {
		delete(item, "encrypted_content")
	}
	if kind == "message" || kind == "" && item["role"] != nil {
		item["type"] = "message"
		delete(item, "id")
		delete(item, "status")
		if parts, ok := item["content"].([]any); ok {
			for _, value := range parts {
				if part, ok := value.(map[string]any); ok && (part["type"] == "input_text" || part["type"] == "output_text") {
					part["type"] = "input_text"
				}
			}
		}
	}
	return json.Marshal(item)
}

// recoveryExcerpt keeps a suffix of complete user turns, including their images.
func recoveryExcerpt(input json.RawMessage, maxTokens int, model string) (json.RawMessage, int, error) {
	clean, err := recoveryInput(append(append([]byte(`{"input":`), input...), '}'))
	if err != nil {
		return nil, 0, err
	}
	items, err := recoveryItems(clean)
	if err != nil {
		return nil, 0, err
	}
	starts := []int{0}
	pending := 0
	for i, raw := range items {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &item)
		switch item.Type {
		case "compaction", "compaction_summary", "context_compaction":
			return nil, 0, fmt.Errorf("local excerpt requires all parent checkpoints to be expanded")
		case "function_call", "custom_tool_call", "tool_search_call":
			pending++
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			pending--
		}
		if i > 0 && pending == 0 && (item.Type == "message" || item.Type == "") && item.Role == "user" {
			starts = append(starts, i)
		}
	}
	// Array punctuation is counted, including the two brackets for [] and
	// one comma between items. Each item has already been marshalled compactly.
	sizes := make([]int, len(items)+1)
	for i := len(items) - 1; i >= 0; i-- {
		delta, _, err := recoveryItemMedia(gjson.ParseBytes(items[i]), model)
		if err != nil {
			return nil, 0, err
		}
		sizes[i] = sizes[i+1] + len(items[i]) + delta
		if i+1 < len(items) {
			sizes[i]++
		}
	}
	for _, start := range starts {
		if sizes[start]+2 <= maxTokens {
			excerpt, err := json.Marshal(items[start:])
			return excerpt, start, err
		}
	}
	return nil, 0, fmt.Errorf("latest complete interaction exceeds the local excerpt budget")
}
