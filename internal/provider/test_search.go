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

// TestSearch asks only the configured native search APIs, without the
// gateway's search replacement. A normal answer alone proves nothing.
func (p Provider) TestSearch(ctx context.Context, picked []string) ([]Result, error) {
	if p.Account != nil {
		return nil, errors.New("native search detection is for API-key providers")
	}
	if p.Anthropic == "" && p.Responses == "" {
		return nil, errors.New("configure an Anthropic or Responses URL first")
	}
	q, err := p.detecting("")
	if err != nil {
		return nil, err
	}
	ctx = q.Via(ctx)
	if len(picked) > 0 {
		q.Models = picked
	}
	var out []Result
	for _, proto := range []Protocol{Anthropic, Responses} {
		if q.Base(proto) == "" {
			continue
		}
		key, ok := q.keyFor(proto)
		trial := q
		if ok && len(trial.Exposed()) == 0 && len(trial.Available()) == 0 {
			list := key.WithKey(KeyAccount{Key: key.Key, Protocol: proto})
			list.ID, list.Gemini = "", "" // Discover only this API, without saving auth.
			trial.Models = list.detectModels(list.Via(ctx))
		}
		model := trial.testModel(key, proto)
		if !ok {
			out = append(out, Result{Protocol: proto, Model: model, Error: "no key is on for this endpoint"})
			continue
		}
		out = append(out, key.testSearch(ctx, proto, model))
	}
	return out, nil
}

func (p Provider) testSearch(ctx context.Context, proto Protocol, model string) (r Result) {
	r = Result{Protocol: proto, Model: model}
	if model == "" {
		r.Error = "no model to try: expose one, or refresh the model list"
		return
	}
	ctx, cancel := context.WithTimeout(p.Via(ctx), 45*time.Second)
	defer cancel()
	start := time.Now()
	defer func() { r.Millis = time.Since(start).Milliseconds() }()
	prompt := "Use web_search to search the web for the official Go programming language website. Then answer with its URL in one sentence."
	body := map[string]any{"model": UpstreamName(p, model), "stream": true}
	url := strings.TrimRight(p.Base(proto), "/")
	if proto == Anthropic {
		url += "/v1/messages"
		body["max_tokens"] = 2048
		body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		body["tools"] = []any{map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 1}}
	} else {
		url += "/responses"
		body["input"] = []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt}}}}
		body["tools"] = []any{map[string]any{"type": "web_search"}}
		body["tool_choice"] = "required"
		body["max_output_tokens"] = 2048
	}
	b, err := json.Marshal(body)
	if err != nil {
		r.Error = err.Error()
		return
	}
	b = p.tested(string(b))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		r.Error = err.Error()
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if proto == Anthropic {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if err = p.Sign(ctx, req, proto, b); err != nil {
		r.Error = err.Error()
		return
	}
	res, err := p.Do(http.DefaultClient, req)
	if err == nil && proto == Anthropic {
		transient := p
		transient.ID = "" // A probe may retry auth, but must not save it.
		res, err = RetryOneAuth(transient, req, res, func(r *http.Request) (*http.Response, error) { return p.Do(http.DefaultClient, r) })
	}
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
	r.OK, err = nativeSearchAnswer(res.Body, proto, strings.Contains(res.Header.Get("Content-Type"), "text/event-stream"))
	if err != nil {
		r.Error = err.Error()
	}
	return
}

// Read through the result, rather than stopping at a tool's announcement:
// an Anthropic server tool can fail inside an otherwise successful 200.
func nativeSearchAnswer(r io.Reader, proto Protocol, stream bool) (bool, error) {
	const limit = 2 << 20
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return false, err
	}
	if len(b) > limit {
		return false, errors.New("search response is too large")
	}
	found, completed := false, !stream
	calls := map[string]bool{}
	var failure error
	block := func(v gjson.Result) {
		switch v.Get("type").String() {
		case "web_search_call":
			if proto == Responses && v.Get("status").String() == "completed" && v.Get("action.type").String() == "search" {
				found = true
			}
		case "server_tool_use":
			if proto == Anthropic && v.Get("name").String() == "web_search" && v.Get("id").String() != "" {
				calls[v.Get("id").String()] = true
			}
		case "web_search_tool_result":
			if proto != Anthropic {
				return
			}
			content := v.Get("content")
			if content.Get("type").String() == "web_search_tool_result_error" {
				failure = errors.New("web_search: " + content.Get("error_code").String())
			}
			if calls[v.Get("tool_use_id").String()] {
				for _, hit := range content.Array() {
					if hit.Get("type").String() == "web_search_result" && hit.Get("url").String() != "" {
						found = true
					}
				}
			}
		}
	}
	read := func(b []byte) error {
		if !gjson.ValidBytes(b) {
			return errors.New("invalid JSON in the search response")
		}
		v := gjson.ParseBytes(b)
		switch v.Get("type").String() {
		case "error":
			return errors.New(APIError(b, "search request failed"))
		case "response.failed", "response.incomplete":
			return errors.New(APIError([]byte(v.Get("response").Raw), "search request failed"))
		case "response.completed":
			if proto == Responses {
				completed = v.Get("response.status").String() == "completed"
			}
		case "message_stop":
			if proto == Anthropic {
				completed = true
			}
		}
		for _, path := range []string{"", "response"} {
			reply := v
			if path != "" {
				reply = v.Get(path)
			}
			if e := reply.Get("error"); e.Exists() && e.Type != gjson.Null {
				return errors.New(APIError([]byte(reply.Raw), "search request failed"))
			}
			if status := reply.Get("status").String(); status == "failed" || status == "incomplete" {
				return errors.New(APIError([]byte(reply.Raw), "search request failed"))
			} else if status != "" && status != "completed" {
				continue
			}
			block(reply.Get("item"))
			block(reply.Get("content_block"))
			for _, list := range []string{"output", "content"} {
				for _, item := range reply.Get(list).Array() {
					block(item)
				}
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
			return false, errors.New("search response ended before completion")
		}
	} else {
		if err := read(b); err != nil {
			return false, err
		}
	}
	if failure != nil {
		return false, failure
	}
	return found, nil
}
