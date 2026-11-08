package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
	"github.com/yetone/magpie/internal/settings"
)

func recoveryMediaRouteSetup(t *testing.T) (*fake, *fake, string) {
	t.Helper()
	a, b := recoverySetup(t)
	for _, id := range []string{"fake", "other"} {
		p, err := provider.Find(id)
		if err != nil {
			t.Fatal(err)
		}
		p.Models = []string{"gpt-5.5"}
		p.Contexts = map[string]int{"gpt-5.5": 200000}
		if err := provider.Save(*p); err != nil {
			t.Fatal(err)
		}
		if err := provider.SetModelImage(id+"/gpt-5.5", imageInputBool(true)); err != nil {
			t.Fatal(err)
		}
	}
	image := recoveryTestImage(t, 512, 512)
	history := fmt.Sprintf(`{"model":"fake/gpt-5.5","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"first screenshot"},{"type":"input_image","image_url":%q,"detail":"original"}]},{"type":"function_call","call_id":"screen-call","name":"screenshot","arguments":"{}"},{"type":"function_call_output","call_id":"screen-call","output":[{"type":"input_text","text":"tool screenshot"},{"type":"input_image","image_url":%q,"detail":"high"}]},{"role":"assistant","content":"checked both"},{"type":"compaction_trigger"}]}`, image, image)
	if w := recoveryPost(t, history, "media-branch"); w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "native-backed" {
		t.Fatalf("capture: %d %s", w.Code, w.Body)
	}
	return a, b, image
}

func assertRecoveryMedia(t *testing.T, body []byte, image string) {
	t.Helper()
	for field, want := range map[string]string{
		"input.0.content.0.text":      "first screenshot",
		"input.0.content.1.image_url": image,
		"input.0.content.1.detail":    "original",
		"input.1.call_id":             "screen-call",
		"input.2.call_id":             "screen-call",
		"input.2.output.0.text":       "tool screenshot",
		"input.2.output.1.image_url":  image,
		"input.2.output.1.detail":     "high",
	} {
		if got := gjson.GetBytes(body, field).Str; got != want {
			t.Fatalf("restored %s changed (got %d bytes, want %d)", field, len(got), len(want))
		}
	}
	if strings.Contains(string(body), imageLeftOut) || strings.Contains(string(body), "local-C1") {
		t.Fatal("replay omitted images or kept the old checkpoint")
	}
}

func TestNativeRecoveryMediaRoundTripAndRecompact(t *testing.T) {
	a, b, image := recoveryMediaRouteSetup(t)
	next := recoveryNext("other/gpt-5.5", "local-C1", "continue")
	w := recoveryPost(t, next, "media-branch")
	if w.Code != 200 || b.calls != 1 {
		t.Fatalf("restore: %d %s, calls=%d", w.Code, w.Body, b.calls)
	}
	assertRecoveryMedia(t, b.got, image)

	// Another compaction must save the images of the adopted view, too.
	var request map[string]json.RawMessage
	if err := json.Unmarshal([]byte(next), &request); err != nil {
		t.Fatal(err)
	}
	items, err := recoveryItems(request["input"])
	if err != nil {
		t.Fatal(err)
	}
	request["input"], err = json.Marshal(append(items, json.RawMessage(`{"type":"compaction_trigger"}`)))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	b.reply = nativeSealedReply("local-C2")
	if w = recoveryPost(t, string(body), "media-branch"); w.Code != 200 {
		t.Fatalf("recompact: %d %s", w.Code, w.Body)
	}
	record, err := recoverystore.Load("local", checkpointKey("local-C2"))
	if err != nil {
		t.Fatal(err)
	}
	assertRecoveryMedia(t, withFields([]byte(`{}`), map[string]any{"input": record.Input}), image)
	a.reply = recoveryDone
	w = recoveryPost(t, recoveryNext("fake/gpt-5.5", "local-C2", "after restart"), "media-branch")
	if w.Code != 200 || a.calls != 2 {
		t.Fatalf("new server restoring C2: %d %s, calls=%d", w.Code, w.Body, a.calls)
	}
	assertRecoveryMedia(t, a.got, image)
}

func TestNativeRecoveryMediaSolModels(t *testing.T) {
	for _, model := range []string{"gpt-6-sol", "gpt-6.1-sol"} {
		t.Run(model, func(t *testing.T) {
			_, b, image := recoveryMediaRouteSetup(t)
			p, err := provider.Find("other")
			if err != nil {
				t.Fatal(err)
			}
			p.Models = []string{model}
			p.Contexts = map[string]int{model: 500000}
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetModelImage("other/"+model, imageInputBool(true)); err != nil {
				t.Fatal(err)
			}
			w := recoveryPost(t, recoveryNext("other/"+model, "local-C1", "continue"), "media-branch")
			if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "restored" || b.calls != 1 {
				t.Fatalf("Sol restore: status=%d state=%q calls=%d body=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), b.calls, w.Body)
			}
			if got := gjson.GetBytes(b.got, "model").Str; got != model {
				t.Fatalf("wire model=%q, want %q", got, model)
			}
			assertRecoveryMedia(t, b.got, image)
		})
	}
}

func TestNativeRecoveryMediaGenericRefusalKeepsImages(t *testing.T) {
	const message = "The request could not be processed. Please check the request parameters."
	for _, streamed := range []bool{false, true} {
		t.Run(fmt.Sprintf("streamed %t", streamed), func(t *testing.T) {
			a, b, image := recoveryMediaRouteSetup(t)
			failure := fmt.Sprintf(`{"error":{"code":"invalid_request","message":%q}}`, message)
			b.code, b.ctype, b.reply = http.StatusBadRequest, "application/json", failure
			if streamed {
				b.code, b.ctype = http.StatusOK, "text/event-stream"
				b.reply = sse(fmt.Sprintf(`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_request","message":%q}}}`, message))
			}
			s := New()
			post := func(model string) *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(recoveryNext(model, "local-C1", "continue")))
				r.RemoteAddr = "127.0.0.1:12345"
				r.Header.Set("Authorization", "Bearer magpie")
				r.Header.Set(SessionHeader, "media-branch")
				w := httptest.NewRecorder()
				s.Front().ServeHTTP(w, r)
				return w
			}
			w := post("other/gpt-5.5")
			if b.calls != 1 || !strings.Contains(w.Body.String(), message) || strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("generic refusal: status=%d calls=%d body=%s", w.Code, b.calls, w.Body)
			}
			assertRecoveryMedia(t, b.got, image)
			if s.sightRefused("other", "gpt-5.5") {
				t.Fatal("generic refusal marked the model as unable to take images")
			}
			a.reply = recoveryDone
			w = post("fake/gpt-5.5")
			if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "restored" || a.calls != 2 || b.calls != 1 {
				t.Fatalf("continuation after refusal: status=%d state=%q writer calls=%d refused calls=%d body=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), a.calls, b.calls, w.Body)
			}
			assertRecoveryMedia(t, a.got, image)
		})
	}
}

func TestNativeRecoveryMediaSolRefusalFallback(t *testing.T) {
	const message = "The request could not be processed. Please check the request parameters."
	const refusal = `{"type":"invalid_request_error","code":"invalid_request","message":"The request could not be processed. Please check the request parameters.","param":null}`
	for _, tt := range []struct {
		name         string
		model        string
		streamed     bool
		pass         bool
		published    bool
		last         bool
		system       bool
		wantFallback int
	}{
		{name: "6 Sol HTTP", model: "gpt-6-sol", wantFallback: 1},
		{name: "6 Sol HTTP with model prompts", model: "gpt-6-sol", system: true, wantFallback: 1},
		{name: "6 Sol stream", model: "gpt-6-sol", streamed: true, wantFallback: 1},
		{name: "6.1 Sol HTTP", model: "gpt-6.1-sol", wantFallback: 1},
		{name: "6.1 Sol stream", model: "gpt-6.1-sol", streamed: true, wantFallback: 1},
		{name: "pass HTTP", model: "gpt-6.1-sol", pass: true},
		{name: "pass stream", model: "gpt-6.1-sol", streamed: true, pass: true},
		{name: "published stream", model: "gpt-6.1-sol", streamed: true, published: true},
		{name: "other model HTTP", model: "gpt-5.5"},
		{name: "other model stream", model: "gpt-5.5", streamed: true},
		{name: "last HTTP", model: "gpt-6.1-sol", last: true},
		{name: "last stream", model: "gpt-6.1-sol", streamed: true, last: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, b, image := recoveryMediaRouteSetup(t)
			c := &fake{t: t, reply: recoveryDone}
			up := httptest.NewServer(c)
			t.Cleanup(up.Close)
			if err := provider.Save(provider.Provider{ID: "reserve", Name: "Reserve", Key: "reserve-key", Models: []string{"gpt-5.5"}, Contexts: map[string]int{"gpt-5.5": 200000}, Responses: up.URL + "/v1"}); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetModelImage("reserve/gpt-5.5", imageInputBool(true)); err != nil {
				t.Fatal(err)
			}
			p, err := provider.Find("other")
			if err != nil {
				t.Fatal(err)
			}
			p.Models = []string{tt.model}
			p.Contexts = map[string]int{tt.model: 500000}
			p.Fallback = []string{"reserve/gpt-5.5"}
			if tt.last {
				p.Fallback = nil
			}
			p.PassErrors = tt.pass
			if err := provider.Save(*p); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetModelImage("other/"+tt.model, imageInputBool(true)); err != nil {
				t.Fatal(err)
			}
			if tt.system {
				for ref, prompt := range map[string]string{"other/" + tt.model: "TARGET_SYSTEM", "reserve/gpt-5.5": "RESERVE_SYSTEM"} {
					if err := provider.SetModelSystemPrompt(ref, prompt); err != nil {
						t.Fatal(err)
					}
				}
			}
			checkMedia := func(body []byte, prompt, other string) {
				t.Helper()
				if tt.system {
					if gjson.GetBytes(body, "input.0.role").Str != "developer" || strings.Count(string(body), prompt) != 1 || strings.Contains(string(body), other) {
						t.Fatalf("target model prompt was missing, repeated or carried from another model: %s", recoveryMediaShape(body))
					}
					items, err := recoveryItems([]byte(gjson.GetBytes(body, "input").Raw))
					if err != nil {
						t.Fatal(err)
					}
					input, err := json.Marshal(items[1:])
					if err != nil {
						t.Fatal(err)
					}
					body = withTopField(body, "input", input)
				}
				assertRecoveryMedia(t, body, image)
			}
			b.code, b.ctype, b.reply = http.StatusBadRequest, "application/json", `{"error":`+refusal+`}`
			if tt.streamed {
				b.code, b.ctype = http.StatusOK, "text/event-stream"
				b.reply = sse(`data: {"type":"response.failed","response":{"status":"failed","error":` + refusal + `}}`)
				if tt.published {
					b.reply = sse(`data: {"type":"response.output_text.delta","delta":"already visible"}`) + b.reply
				}
			}
			s := New()
			post := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(recoveryNext("other/"+tt.model, "local-C1", "continue")))
				r.RemoteAddr = "127.0.0.1:12345"
				r.Header.Set("Authorization", "Bearer magpie")
				r.Header.Set(SessionHeader, "media-branch")
				w := httptest.NewRecorder()
				s.Front().ServeHTTP(w, r)
				return w
			}
			w := post()
			if b.calls != 1 || c.calls != tt.wantFallback || a.calls != 1 {
				t.Fatalf("refusal routing: status=%d writer=%d target=%d reserve=%d want=%d body=%s", w.Code, a.calls, b.calls, c.calls, tt.wantFallback, w.Body)
			}
			checkMedia(b.got, "TARGET_SYSTEM", "RESERVE_SYSTEM")
			if got := gjson.GetBytes(b.got, "model").Str; got != tt.model {
				t.Fatalf("refused wire model=%q, want %q", got, tt.model)
			}
			if tt.wantFallback == 1 {
				if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "restored" || !strings.Contains(w.Body.String(), "response.completed") || strings.Contains(w.Body.String(), "invalid_request") {
					t.Fatalf("image fallback: status=%d state=%q body=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), w.Body)
				}
				checkMedia(c.got, "RESERVE_SYSTEM", "TARGET_SYSTEM")
			} else if !strings.Contains(w.Body.String(), message) || strings.Contains(w.Body.String(), "response.completed") {
				t.Fatalf("refusal was not passed through: status=%d body=%s", w.Code, w.Body)
			}
			if tt.published && !strings.Contains(w.Body.String(), "already visible") {
				t.Fatal("published output disappeared after the refusal")
			}
			if s.sightRefused("other", tt.model) {
				t.Fatal("generic refusal changed image capability")
			}
			if tt.wantFallback == 0 {
				return
			}
			if restedAny("other") {
				t.Fatal("image refusal fallback rested the provider")
			}
			b.code, b.ctype, b.reply = http.StatusOK, "text/event-stream", recoveryDone
			w = post()
			if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "restored" || b.calls != 2 || c.calls != tt.wantFallback || a.calls != 1 {
				t.Fatalf("same provider after refusal: status=%d state=%q writer=%d target=%d reserve=%d body=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), a.calls, b.calls, c.calls, w.Body)
			}
			checkMedia(b.got, "TARGET_SYSTEM", "RESERVE_SYSTEM")
		})
	}
}

func TestNativeRecoveryMediaSkipsIncompatibleCandidates(t *testing.T) {
	for _, tt := range []struct {
		name          string
		chat          bool
		group         bool
		unknownBudget bool
	}{
		{name: "text-only"},
		{name: "group text-only", group: true},
		{name: "chat endpoint", chat: true},
		{name: "unverified dated Sol alias", unknownBudget: true},
		{name: "group unverified dated Sol alias", group: true, unknownBudget: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, b, image := recoveryMediaRouteSetup(t)
			blocked := &fake{t: t, reply: recoveryDone}
			up := httptest.NewServer(blocked)
			t.Cleanup(up.Close)
			blockedModel, window := "gpt-5.5", 200000
			if tt.unknownBudget {
				blockedModel, window = "gpt-6.1-sol-2099-01-01", 500000
			}
			p := provider.Provider{ID: "blocked", Name: "Blocked", Key: "blocked-key", Models: []string{blockedModel}, Contexts: map[string]int{blockedModel: window}, Fallback: []string{"other/gpt-5.5"}}
			if tt.chat {
				p.Chat = up.URL + "/v1"
			} else {
				p.Responses = up.URL + "/v1"
			}
			if err := provider.Save(p); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetModelImage("blocked/"+blockedModel, imageInputBool(tt.chat || tt.unknownBudget)); err != nil {
				t.Fatal(err)
			}
			st := settings.Load()
			st.Vision = "other/gpt-5.5"
			if err := settings.Save(st); err != nil {
				t.Fatal(err)
			}
			model := "blocked/" + blockedModel
			if tt.group {
				if err := provider.SaveGroup(provider.Group{Name: "Media", Members: []string{model, "other/gpt-5.5"}, Routing: provider.Ordered}); err != nil {
					t.Fatal(err)
				}
				model = "group/media"
			}
			w := recoveryPost(t, recoveryNext(model, "local-C1", "continue"), "media-branch")
			if w.Code != 200 || blocked.calls != 0 || b.calls != 1 {
				t.Fatalf("fallback: %d %s, incompatible calls=%d, image/describer calls=%d", w.Code, w.Body, blocked.calls, b.calls)
			}
			assertRecoveryMedia(t, b.got, image)
		})
	}
}

func TestNativeRecoveryMediaDoesNotFitByDeletingImages(t *testing.T) {
	for _, adoptedView := range []bool{false, true} {
		for _, cap := range []struct {
			name                string
			known, images, user bool
		}{
			{name: "known byte cap", known: true},
			{name: "unknown byte cap"},
			{name: "known image cap", known: true, images: true},
			{name: "unknown image cap", images: true},
			{name: "user image cap", known: true, images: true, user: true},
		} {
			t.Run(fmt.Sprintf("adopted %t/%s", adoptedView, cap.name), func(t *testing.T) {
				_, b, image := recoveryMediaRouteSetup(t)
				next := recoveryNext("other/gpt-5.5", "local-C1", "continue")
				warmupCalls := 0
				if adoptedView {
					// Adopt the view before testing a later request's candidate caps.
					adopted := recoveryPost(t, next, "media-branch")
					if adopted.Code != 200 || adopted.Header().Get("X-Magpie-Recovery-State") != "restored" || b.calls != 1 {
						t.Fatalf("adopted image view: status=%d state=%q calls=%d input=%s", adopted.Code, adopted.Header().Get("X-Magpie-Recovery-State"), b.calls, recoveryMediaShape(b.got))
					}
					assertRecoveryMedia(t, b.got, image)
					warmupCalls = 1
				}
				c := &fake{t: t, reply: recoveryDone}
				up := httptest.NewServer(c)
				t.Cleanup(up.Close)
				if err := provider.Save(provider.Provider{ID: "reserve", Name: "Reserve", Key: "reserve-key", Models: []string{"gpt-5.5"}, Contexts: map[string]int{"gpt-5.5": 200000}, Responses: up.URL + "/v1", NativeCompaction: adoptedView}); err != nil {
					t.Fatal(err)
				}
				if err := provider.SetModelImage("reserve/gpt-5.5", imageInputBool(true)); err != nil {
					t.Fatal(err)
				}
				p, err := provider.Find("other")
				if err != nil {
					t.Fatal(err)
				}
				p.Fallback = []string{"reserve/gpt-5.5"}
				if cap.user {
					p.MaxImages = map[string]int{"gpt-5.5": 1}
				}
				if err := provider.Save(*p); err != nil {
					t.Fatal(err)
				}
				b.refuse = func(body []byte) (int, string) {
					if recoveryHasImages(body) {
						if cap.images {
							return 400, `{"error":{"message":"Coarena: A request may carry at most 1 images."}}`
						}
						return 400, `{"error":{"message":"request body too large"}}`
					}
					return 0, ""
				}
				s := New()
				if cap.known && !cap.user {
					if cap.images {
						s.imageCaps.learn("other\x00gpt-5.5", 1)
					} else {
						s.bodyCaps.learn("other", 1)
					}
				}
				r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(next))
				r.RemoteAddr = "127.0.0.1:12345"
				r.Header.Set("Authorization", "Bearer magpie")
				r.Header.Set(SessionHeader, "media-branch")
				w := httptest.NewRecorder()
				s.Front().ServeHTTP(w, r)
				wantCalls := warmupCalls
				if !cap.known {
					wantCalls++
				}
				t.Logf("status=%d state=%q capped input=%s fallback input=%s", w.Code, w.Header().Get("X-Magpie-Recovery-State"), recoveryMediaShape(b.got), recoveryMediaShape(c.got))
				if w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "restored" || b.calls != wantCalls || c.calls != 1 {
					t.Fatalf("cap fallback: capped calls=%d want=%d, fallback calls=%d", b.calls, wantCalls, c.calls)
				}
				assertRecoveryMedia(t, c.got, image)
				if !cap.known {
					assertRecoveryMedia(t, b.got, image)
				}
			})
		}
	}
}

func recoveryMediaShape(body []byte) string {
	var items []string
	for _, item := range gjson.GetBytes(body, "input").Array() {
		images := 0
		for _, field := range []string{"content", "output"} {
			for _, part := range item.Get(field).Array() {
				if kind := part.Get("type").Str; kind == "input_image" || kind == "image_url" {
					images++
				}
			}
		}
		items = append(items, fmt.Sprintf("{type:%q role:%q encrypted:%t images:%d}", item.Get("type").Str, item.Get("role").Str, item.Get("encrypted_content").Exists(), images))
	}
	return "[" + strings.Join(items, ", ") + "]"
}
