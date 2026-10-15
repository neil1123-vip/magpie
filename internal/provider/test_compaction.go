package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// TestCompaction asks the same Responses trigger the native-compaction
// option passes through. A /responses/compact probe would test another path.
func (p Provider) TestCompaction(ctx context.Context, picked []string) ([]Result, error) {
	if p.Account != nil {
		return nil, errors.New("native compaction detection is for API-key providers")
	}
	if strings.TrimSpace(p.Responses) == "" {
		return nil, errors.New("configure a Responses URL first")
	}
	q, err := p.detecting("")
	if err != nil {
		return nil, err
	}
	ctx = q.Via(ctx)
	if len(picked) > 0 {
		q.Models = picked
	}
	key, ok := q.keyFor(Responses)
	if ok && len(q.Exposed()) == 0 && len(q.Available()) == 0 {
		list := key.WithKey(KeyAccount{Key: key.Key, Protocol: Responses})
		list.ID, list.Gemini = "", "" // Discover only this API, without saving auth.
		q.Models = list.detectModels(list.Via(ctx))
	}
	r := Result{Protocol: Responses, Model: q.testModel(key, Responses)}
	if !ok {
		r.Error = "no key is on for this endpoint"
	} else if r.Model == "" {
		r.Error = "no model to try: expose one, or refresh the model list"
	} else {
		r = key.testCompaction(ctx, r.Model)
	}
	return []Result{r}, nil
}

func (p Provider) testCompaction(ctx context.Context, model string) (r Result) {
	r = Result{Protocol: Responses, Model: model}
	ctx, cancel := context.WithTimeout(p.Via(ctx), 45*time.Second)
	defer cancel()
	start := time.Now()
	defer func() { r.Millis = time.Since(start).Milliseconds() }()
	b, err := json.Marshal(map[string]any{
		"model": UpstreamName(p, model), "stream": true, "store": false,
		"instructions": "You are Codex, a coding assistant.",
		"include":      []string{"reasoning.encrypted_content"},
		"tools":        []any{},
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Remember: the project is named Orchard and written in Go."}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "I will keep those project details for the next task."}}},
			map[string]any{"type": "compaction_trigger"},
		},
	})
	if err != nil {
		r.Error = err.Error()
		return
	}
	body := p.tested(string(b))
	url := strings.TrimRight(p.Base(Responses), "/") + "/responses"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		r.Error = err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if err = p.Sign(ctx, req, Responses, body); err != nil {
		r.Error = err.Error()
		return
	}
	res, err := p.Do(http.DefaultClient, req)
	if err != nil {
		r.Error = strings.TrimPrefix(err.Error(), "Post \""+url+"\": ")
		return
	}
	defer res.Body.Close()
	r.Status = res.StatusCode
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		r.Error = APIError(b, res.Status)
		return
	}
	r.OK, err = nativeCompactionAnswer(res.Body, strings.Contains(res.Header.Get("Content-Type"), "text/event-stream"))
	if err != nil {
		r.Error = err.Error()
	}
	return
}

func nativeCompactionAnswer(r io.Reader, stream bool) (bool, error) {
	const limit = 2 << 20
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return false, err
	}
	if len(b) > limit {
		return false, errors.New("compaction response is too large")
	}
	found, completed := false, !stream
	item := func(v gjson.Result) {
		enc := v.Get("encrypted_content")
		typ := v.Get("type").String()
		if (typ == "compaction" || typ == "compaction_summary") && enc.Type == gjson.String && strings.TrimSpace(enc.Str) != "" && !strings.HasPrefix(strings.TrimSpace(enc.Str), "magpie1:") {
			found = true
		}
	}
	read := func(b []byte) error {
		if !gjson.ValidBytes(b) {
			return errors.New("invalid JSON in the compaction response")
		}
		v := gjson.ParseBytes(b)
		if e := v.Get("error"); e.Exists() && e.Type != gjson.Null {
			return errors.New(APIError(b, "compaction request failed"))
		}
		switch v.Get("type").String() {
		case "error":
			return errors.New(APIError(b, "compaction request failed"))
		case "response.failed", "response.incomplete":
			return errors.New(APIError([]byte(v.Get("response").Raw), "compaction request failed"))
		case "response.completed":
			completed = v.Get("response.status").String() == "completed"
		case "response.output_item.done":
			item(v.Get("item"))
		}
		for _, path := range []string{"", "response"} {
			reply := v
			if path != "" {
				reply = v.Get(path)
			}
			if e := reply.Get("error"); e.Exists() && e.Type != gjson.Null {
				return errors.New(APIError([]byte(reply.Raw), "compaction request failed"))
			}
			if status := reply.Get("status").String(); status == "failed" || status == "incomplete" {
				return errors.New(APIError([]byte(reply.Raw), "compaction request failed"))
			} else if status != "" && status != "completed" {
				continue
			}
			for _, it := range reply.Get("output").Array() {
				item(it)
			}
		}
		return nil
	}
	if stream {
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 64*1024), limit+1)
		var data bytes.Buffer
		dispatch := func() error {
			defer data.Reset()
			if data.Len() == 0 || strings.TrimSpace(data.String()) == "[DONE]" {
				return nil
			}
			return read(data.Bytes())
		}
		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				if err := dispatch(); err != nil {
					return false, err
				}
				continue
			}
			if field, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
				if data.Len() != 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(field, " "))
			}
		}
		if err := sc.Err(); err != nil {
			return false, err
		}
		if err := dispatch(); err != nil {
			return false, err
		}
		if !completed {
			return false, errors.New("compaction response ended before completion")
		}
	} else {
		if err := read(b); err != nil {
			return false, err
		}
	}
	return found, nil
}
