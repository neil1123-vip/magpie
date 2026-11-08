package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

func recoveryTestImage(t *testing.T, width, height int) string {
	t.Helper()
	im := image.NewNRGBA(image.Rect(0, 0, width, height))
	seed := uint32(1)
	for i := range im.Pix {
		seed = seed*1664525 + 1013904223
		im.Pix[i] = byte(seed >> 24)
	}
	var out bytes.Buffer
	if err := png.Encode(&out, im); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes())
}

func TestNativeRecoveryImageBudgetExcludesOnlyImagePayload(t *testing.T) {
	url := recoveryTestImage(t, 128, 128)
	for _, kind := range []string{"message", "function_call", "custom_tool_call"} {
		t.Run(kind, func(t *testing.T) {
			part := fmt.Sprintf(`{"type":"input_image","image_url":%q,"detail":"high"}`, url)
			input := `[{"role":"user","content":[{"type":"input_text","text":"inspect"},` + part + `]}]`
			if kind != "message" {
				field := "arguments"
				if kind == "custom_tool_call" {
					field = "input"
				}
				input = fmt.Sprintf(`[{"type":%q,"call_id":"a","name":"screen",%q:"{}"},{"type":%q,"call_id":"a","output":[{"type":"input_text","text":"captured"},%s]}]`, kind, field, kind+"_output", part)
			}
			body := []byte(`{"model":"gpt-5.5","max_output_tokens":256,"input":` + input + `}`)
			got, err := recoveryBudget(body, "gpt-5.5")
			// 16 patches * 1.2 rounded up, plus one rounding token.
			want := len(body) - len(url) + 21 + 256 + 4096
			if err != nil || got != want || got >= len(body) {
				t.Fatalf("budget = %d, %v; want %d, body bytes %d", got, err, want, len(body))
			}
			for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
				if got, err := recoveryBudget(body, model); err != nil || got != want+1 {
					t.Fatalf("%s budget = %d, %v; want %d", model, got, err, want+1)
				}
			}
			if _, err := recoveryBudget(body, "gpt-6.1-sol-2099-01-01"); err == nil {
				t.Fatal("unknown model accepted an image budget")
			}
		})
	}
	text := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":%q}]}`, url))
	if n, err := recoveryBudget(text, "unknown"); err != nil || n != len(text)+8192+4096 {
		t.Fatalf("ordinary text discounted as an image: %d, %v", n, err)
	}
}

func TestNativeRecoveryImageBase64(t *testing.T) {
	otherImage := strings.TrimPrefix(recoveryTestImage(t, 33, 32), "data:image/png;base64,")
	for _, tt := range []struct {
		name, data string
	}{
		{"URL-safe", whitePNGURLSafe},
		{"URL-safe unpadded", strings.TrimRight(whitePNGURLSafe, "=")},
		{"standard unpadded", strings.TrimRight(whitePNGStandard, "=")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_output_tokens":256,"input":[{"type":"function_call","call_id":"a","name":"screen","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":"data:image/png;base64,%s","detail":"high"}]}]}`, tt.data))
			original := bytes.Clone(body)
			wire := stdBase64Body(provider.Responses, body)
			if bytes.Equal(body, wire) || !bytes.Contains(wire, []byte(whitePNGStandard)) {
				t.Fatal("fixture was not normalized")
			}
			if _, err := recoveryInput(body); err != nil {
				t.Errorf("upstream-supported image cannot be saved: %v", err)
			}
			if _, err := recoveryBudget(body, "gpt-5.5"); err != nil {
				t.Errorf("upstream-supported image cannot be budgeted: %v", err)
			}
			check := func(source, candidate []byte) error {
				x := &nativeRecovery{restored: true, body: source, currentWindow: 200000, currentOutput: 8192, currentModel: "gpt-5.5"}
				ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
				return recoveryWireCheck(ctx, provider.Responses, candidate)
			}
			if err := check(body, wire); err != nil {
				t.Errorf("encoding-only normalization changed restored history: %v", err)
			}
			for _, changed := range []struct {
				name string
				wire []byte
			}{
				{"pixels", bytes.Replace(wire, []byte(whitePNGStandard), []byte(otherImage), 1)},
				{"detail", bytes.Replace(wire, []byte(`"detail":"high"`), []byte(`"detail":"low"`), 1)},
				{"tool association", bytes.ReplaceAll(wire, []byte(`"call_id":"a"`), []byte(`"call_id":"b"`))},
				{"part position", bytes.Replace(wire, []byte(`"output":[`), []byte(`"output":[{"type":"input_text","text":"inserted"},`), 1)},
				{"message position", bytes.Replace(wire, []byte(`"input":[`), []byte(`"input":[{"role":"user","content":"inserted"},`), 1)},
			} {
				if bytes.Equal(wire, changed.wire) {
					t.Fatalf("%s mutation did not change fixture", changed.name)
				}
				if err := check(body, changed.wire); err == nil {
					t.Errorf("changed %s was accepted", changed.name)
				}
			}
			nested := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_output_tokens":256,"input":[{"role":"user","content":[{"type":"input_image","image_url":{"url":"data:image/png;base64,%s","detail":"high"}}]}]}`, tt.data))
			if _, err := recoveryBudget(nested, "gpt-5.5"); err != nil {
				t.Fatalf("nested image cannot be budgeted: %v", err)
			}
			lostDetail := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_output_tokens":256,"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,%s"}]}]}`, whitePNGStandard))
			if err := check(nested, lostDetail); err == nil {
				t.Error("lost nested image detail was accepted")
			}
			if !bytes.Equal(body, original) {
				t.Fatal("recovery validation changed the saved request")
			}
		})
	}
}

func TestNativeRecoveryImageRefusalScope(t *testing.T) {
	const failure = `{"type":"invalid_request_error","code":"invalid_request","message":"The request could not be processed. Please check the request parameters.","param":null}`
	const input = `{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,fixture"}]}]}`
	for _, tt := range []struct {
		name, model, request, failure string
		status                        int
		restored, want                bool
	}{
		{"http", "gpt-6-sol", input, `{"error":` + failure + `}`, 400, true, true},
		{"stream", "gpt-6.1-sol", input, failure, 400, true, true},
		{"ordinary", "gpt-6-sol", input, failure, 400, false, false},
		{"text", "gpt-6-sol", `{"input":[]}`, failure, 400, true, false},
		{"other model", "gpt-6-astra", input, failure, 400, true, false},
		{"auth", "gpt-6-sol", input, failure, 401, true, false},
		{"field error", "gpt-6-sol", input, strings.Replace(failure, `"param":null`, `"param":"input"`, 1), 400, true, false},
		{"other code", "gpt-6-sol", input, strings.Replace(failure, `"code":"invalid_request"`, `"code":"content_policy_violation"`, 1), 400, true, false},
		{"other message", "gpt-6-sol", input, strings.Replace(failure, "could not be processed", "was refused by policy", 1), 400, true, false},
		{"malformed", "gpt-6-sol", input, failure + `tail`, 400, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x := &nativeRecovery{restored: tt.restored, currentModel: tt.model, body: []byte(tt.request)}
			ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
			if got := recoveryImageRefused(ctx, tt.status, []byte(tt.failure)); got != tt.want {
				t.Fatalf("refusal matched = %t, want %t", got, tt.want)
			}
		})
	}
	if recoveryImageRefused(context.Background(), 400, []byte(failure)) {
		t.Fatal("matched without recovery context")
	}
}

func TestNativeRecoveryImageSolOriginalAbovePatchCap(t *testing.T) {
	// Gray pixels fit the decoded-memory limit even above 30000 raw patches.
	var encoded bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&encoded, image.NewGray(image.Rect(0, 0, 6401, 4800))); err != nil {
		t.Fatal(err)
	}
	url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded.Bytes())
	body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q,"detail":"original"}]}]}`, url))
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		got, err := recoveryRequestCost(body, model)
		if want := len(body) - len(url) + 36002; err != nil || got != want {
			t.Fatalf("%s original budget = %d, %v; want %d", model, got, err, want)
		}
	}
}

func TestNativeRecoveryImageTransparentGrayLimit(t *testing.T) {
	for _, tt := range []struct {
		w, h  uint32
		depth byte
	}{{5000, 4000, 8}, {3000, 3000, 16}} {
		t.Run(fmt.Sprint(tt.depth), func(t *testing.T) {
			raw := pngHeader(tt.w, tt.h, tt.depth, 0)
			// No IDAT: even a broken guard cannot allocate the oversized pixels.
			raw = append(raw[:33:33], raw[len(raw)-12:]...)
			raw = pngWithGrayTransparency(raw)
			url := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
			if _, err := recoveryImage(url); err == nil || !strings.Contains(err.Error(), "dimensions exceed the validation limit") {
				t.Fatalf("transparent gray image reached decoding: %v", err)
			}
		})
	}
}

func TestNativeRecoveryRejectsDamagedAndUnsupportedImages(t *testing.T) {
	url := recoveryTestImage(t, 32, 32)
	_, payload, _ := strings.Cut(url, ",")
	raw, _ := base64.StdEncoding.DecodeString(payload)
	for name, bad := range map[string]string{
		"base64":           "data:image/png;base64,????",
		"not image":        "data:image/png;base64,aGVsbG8=",
		"wrong mime":       strings.Replace(url, "image/png", "image/jpeg", 1),
		"truncated pixels": "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw[:len(raw)/2]),
		"remote":           "https://example.test/image.png",
		"tail":             url + "!",
		"gif":              "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw==",
	} {
		t.Run(name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, bad))
			if _, err := recoveryInput(body); err == nil {
				t.Fatal("invalid image accepted as a recoverable source")
			}
		})
	}
	for _, part := range []string{
		`{"type":"input_audio","input_audio":{"data":"aGVsbG8="}}`,
		`{"type":"input_file","file_data":"data:application/pdf;base64,aGVsbG8="}`,
		fmt.Sprintf(`{"type":"input_image","image_url":%q,"detail":42}`, url),
		fmt.Sprintf(`{"type":"input_image","image_url":%q,"detail":"future"}`, url),
	} {
		if _, err := recoveryBudget([]byte(`{"input":[{"role":"user","content":[`+part+`]}]}`), "gpt-5.5"); err == nil {
			t.Fatalf("unbudgeted media accepted: %s", part[:min(len(part), 80)])
		}
	}
}

func TestNativeRecoveryImageWireRechecksHistoryAndCapacity(t *testing.T) {
	url := recoveryTestImage(t, 128, 128)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_output_tokens":256,"input":[{"type":"custom_tool_call","call_id":"a","name":"screen","input":"{}"},{"type":"custom_tool_call_output","call_id":"a","output":[{"type":"input_image","image_url":%q,"detail":"original"}]}]}`, url))
	needed, err := recoveryBudget(body, "gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		body     []byte
		protocol provider.Protocol
		window   int
		ok       bool
	}{
		{"fits despite base64", body, provider.Responses, needed, true},
		{"one token over", body, provider.Responses, needed - 1, false},
		{"larger output", bytes.Replace(body, []byte(`:256`), []byte(`:257`), 1), provider.Responses, needed, false},
		{"detail reduced", bytes.Replace(body, []byte(`original`), []byte(`high`), 1), provider.Responses, needed, false},
		{"call changed", bytes.ReplaceAll(body, []byte(`"a"`), []byte(`"b"`)), provider.Responses, needed, false},
		{"image dropped", []byte(`{"model":"gpt-5.5","max_output_tokens":256,"input":[]}`), provider.Responses, needed, false},
		{"unknown model", bytes.Replace(body, []byte(`gpt-5.5`), []byte(`gpt-6.1-sol-2099-01-01`), 1), provider.Responses, 200000, false},
		{"known wire model changed", bytes.Replace(body, []byte(`gpt-5.5`), []byte(`gpt-6.1-sol`), 1), provider.Responses, 200000, false},
		{"chat conversion", []byte(`{"model":"gpt-5.5","max_tokens":256,"messages":[]}`), provider.Chat, 200000, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			x := &nativeRecovery{restored: true, body: body, currentWindow: tt.window, currentModel: "gpt-5.5"}
			ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
			err := recoveryWireCheck(ctx, tt.protocol, tt.body)
			if (err == nil) != tt.ok || !tt.ok && !errors.Is(err, errRecoveryWindow) {
				t.Fatalf("wire check = %v, want allowed %v", err, tt.ok)
			}
		})
	}
}

func TestNativeRecoveryImageExcerptKeepsWholeInteraction(t *testing.T) {
	url := recoveryTestImage(t, 128, 128)
	last := fmt.Sprintf(`[{"role":"user","content":"recent screenshot"},{"type":"function_call","call_id":"a","name":"screen","arguments":"{}"},{"type":"function_call_output","call_id":"a","output":[{"type":"input_image","image_url":%q}]},{"role":"assistant","content":"answer"}]`, url)
	clean, err := recoveryInput([]byte(`{"input":` + last + `}`))
	if err != nil {
		t.Fatal(err)
	}
	cost, _, err := recoveryInputCost(gjson.ParseBytes(clean), "gpt-5.5")
	if err != nil || cost >= len(clean) {
		t.Fatalf("image fixture budget = %d, %v", cost, err)
	}
	whole := json.RawMessage(`[{"role":"user","content":"old"},{"role":"assistant","content":"old reply"},` + last[1:])
	got, omitted, err := recoveryExcerpt(whole, cost, "gpt-5.5")
	if err != nil || omitted != 2 || !bytes.Equal(got, clean) {
		t.Fatalf("excerpt omitted %d, err %v, same input %v", omitted, err, bytes.Equal(got, clean))
	}
	if _, _, err := recoveryExcerpt(whole, cost-1, "gpt-5.5"); err == nil {
		t.Fatal("latest tool interaction split to remove its image")
	}
}

func TestNativeRecoveryImageCountAndRequestBounds(t *testing.T) {
	part := fmt.Sprintf(`{"type":"input_image","image_url":%q}`, recoveryTestImage(t, 1, 1))
	for _, n := range []int{recoveryImageCount, recoveryImageCount + 1} {
		body := []byte(`{"input":[{"role":"user","content":[` + strings.TrimSuffix(strings.Repeat(part+",", n), ",") + `]}]}`)
		_, err := recoveryRequestCost(body, "gpt-5.5")
		if (err == nil) != (n == recoveryImageCount) {
			t.Fatalf("count %d: %v", n, err)
		}
	}
	if _, err := recoveryRequestCost(make([]byte, (32<<20)+1), "gpt-5.5"); err == nil {
		t.Fatal("oversized request accepted")
	}
	bad := `{"type":"input_image","image_url":"data:image/png;base64,broken"}`
	body := []byte(`{"input":[{"role":"user","content":[` + strings.TrimSuffix(strings.Repeat(bad+",", recoveryImageCount+1), ",") + `]}]}`)
	if _, err := recoveryInput(body); err == nil || !strings.Contains(err.Error(), "128 images") {
		t.Fatalf("image count must be checked before decoding: %v", err)
	}
}

func TestNativeRecoveryImageHistoryAllowsTransportNormalization(t *testing.T) {
	url := recoveryTestImage(t, 32, 32)
	body := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"checked"}]},{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, url))
	normalized := []byte(fmt.Sprintf(`{"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":%q,"detail":"auto"}]}]}`, url))
	if recoveryImageHistory(body) != recoveryImageHistory(normalized) {
		t.Fatal("equivalent message or removed bare reasoning changed image identity")
	}
	x := &nativeRecovery{restored: true, body: []byte(`{"input":[]}`), currentWindow: 200000, currentModel: "gpt-5.5", currentOutput: 8192}
	ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
	if err := recoveryWireCheck(ctx, provider.Responses, normalized); !errors.Is(err, errRecoveryWindow) {
		t.Fatalf("image injected into text-only history was accepted: %v", err)
	}
}

func TestNativeRecoveryImageHistoryAllowsCurrentTools(t *testing.T) {
	url := recoveryTestImage(t, 32, 32)
	body := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"current_tool","parameters":{"type":"object"}}]},{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, url))
	normalized := fromLite(body)
	if bytes.Contains(normalized, []byte("additional_tools")) || !bytes.Contains(normalized, []byte("current_tool")) {
		t.Fatal("fixture did not lift the current tool declaration")
	}
	x := &nativeRecovery{restored: true, body: body, currentWindow: 200000, currentModel: "gpt-6.1-sol", currentOutput: 8192}
	ctx := context.WithValue(context.Background(), nativeRecoveryKey{}, x)
	if err := recoveryWireCheck(ctx, provider.Responses, normalized); err != nil {
		t.Fatalf("lifting current tools changed image identity: %v", err)
	}
	moved := bytes.Replace(normalized, []byte(`"input":[`), []byte(`"input":[{"role":"user","content":"inserted message"},`), 1)
	if err := recoveryWireCheck(ctx, provider.Responses, moved); !errors.Is(err, errRecoveryWindow) {
		t.Fatalf("image moved to a different message position was accepted: %v", err)
	}
}

func TestNativeRecoveryImageHistoryAllowsOnlyTheTargetModelPrompt(t *testing.T) {
	p := provider.Provider{ID: "fake", Name: "Fake", Key: "k", Models: []string{"gpt-5.5"}, Contexts: map[string]int{"gpt-5.5": 200000}, Responses: "http://127.0.0.1/v1"}
	s := prevSandbox(t, p)
	if err := provider.SetModelSystemPrompt("fake/gpt-5.5", "TARGET_SYSTEM"); err != nil {
		t.Fatal(err)
	}
	url := recoveryTestImage(t, 32, 32)
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.5","max_output_tokens":256,"input":[{"role":"user","content":[{"type":"input_image","image_url":%q}]}]}`, url))
	x := &nativeRecovery{restored: true, body: body}
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r = r.WithContext(context.WithValue(r.Context(), nativeRecoveryKey{}, x))
	wire := withModelSystem(r, p, "gpt-5.5", provider.Responses, body)
	if bytes.Equal(body, wire) {
		t.Fatal("fixture did not insert the target model prompt")
	}
	if err := s.prepareNativeRecovery(r, candidate{p: p, model: "gpt-5.5"}, wire); err != nil {
		t.Fatal(err)
	}
	if err := recoveryWireCheck(r.Context(), provider.Responses, wire); err != nil {
		t.Fatalf("target model prompt changed image identity: %v", err)
	}
	extra := bytes.Replace(wire, []byte(`"input":[`), []byte(`"input":[{"role":"developer","content":"TARGET_SYSTEM"},`), 1)
	if err := recoveryWireCheck(r.Context(), provider.Responses, extra); !errors.Is(err, errRecoveryWindow) {
		t.Fatalf("an extra developer item changed image positions without refusal: %v", err)
	}
	if bytes.Contains(x.body, []byte("TARGET_SYSTEM")) {
		t.Fatal("target model prompt was written into canonical history")
	}
}

func TestNativeRecoveryImageWireFailureDoesNotAdopt(t *testing.T) {
	a, b, _ := recoveryMediaRouteSetup(t)
	p, err := provider.Find("other")
	if err != nil {
		t.Fatal(err)
	}
	// This override applies after the candidate's 200000-token window check.
	p.ExtraBody = provider.ExtraBody{"!max_output_tokens": json.RawMessage(`300000`)}
	if err := provider.Save(*p); err != nil {
		t.Fatal(err)
	}
	w := recoveryPost(t, recoveryNext("other/gpt-5.5", "local-C1", "continue"), "media-branch")
	if w.Code != 413 || b.calls != 0 {
		t.Fatalf("oversized final output reservation reached upstream: status=%d state=%q calls=%d model=%q input=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), b.calls, modelOf(b.got), recoveryMediaShape(b.got))
	}
	if _, err := recoverystore.View("local", checkpointKey("media-branch"), checkpointKey("local-C1")); !errors.Is(err, recoverystore.ErrNotFound) {
		t.Fatalf("unsent request adopted a restored view: %v", err)
	}
	a.reply = recoveryDone
	w = recoveryPost(t, recoveryNext("fake/gpt-5.5", "local-C1", "continue natively"), "media-branch")
	if w.Code != 200 || !bytes.Contains(a.got, []byte("local-C1")) || recoveryHasImages(a.got) {
		t.Fatalf("failed recovery blocked native continuation: %d %s", w.Code, w.Body)
	}
}

func TestNativeRecoveryImageExcerptChoosesUsableModelBudget(t *testing.T) {
	url := recoveryTestImage(t, 1024, 1024)
	input := json.RawMessage(fmt.Sprintf(`[{"role":"user","content":%q},{"role":"assistant","content":"old reply"},{"role":"user","content":[{"type":"input_image","image_url":%q,"detail":"high"}]}]`, strings.Repeat("old ", 12000), url))
	x := &nativeRecovery{branch: "branch", views: []recoverystore.Record{{Input: input}}, body: append(append([]byte(`{"max_output_tokens":256,"input":`), input...), '}')}
	x.considerExcerpt("gpt-4o-mini", 30000)
	x.considerExcerpt("gpt-5.5", 18000)
	if x.excerptInput == nil || x.excerptOmitted != 2 || x.window != 18000 {
		t.Fatalf("lower-cost model ignored: omitted %d, window %d", x.excerptOmitted, x.window)
	}
	x.considerExcerpt("gpt-4o-mini", 30000)
	if x.window != 18000 {
		t.Fatal("larger unusable window displaced the valid excerpt")
	}
}

func TestNativeRecoveryKeepsCommentsBeforeCaptureReady(t *testing.T) {
	w := httptest.NewRecorder()
	x := &nativeRecovery{compact: true}
	capture := &recoveryWriter{w: w, header: w.Header().Clone(), compact: true, recovery: x}
	for _, ping := range []string{": keepalive\n\n", ": queued\r\n\r\n", "\n: heartbeat\n"} {
		if _, err := capture.Write([]byte(ping)); err != nil || capture.passed || w.Body.Len() != 0 {
			t.Fatalf("preparation heartbeat published an uncommitted response: %v", err)
		}
	}
	if recoveryKeepalive([]byte(": comment\ndata: {\"answer\":true}\n\n")) {
		t.Fatal("real response was classified as a comment")
	}
}
