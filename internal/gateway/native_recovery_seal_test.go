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
)

func TestNativeRecoveryKeepsSealWithoutLegacyWriter(t *testing.T) {
	for _, knownRefusal := range []bool{false, true} {
		t.Run(fmt.Sprintf("known custom refusal %t", knownRefusal), func(t *testing.T) {
			a, b := recoverySetup(t)
			if w := recoveryPost(t, fmt.Sprintf(compactTurn, "fake/m1"), "seal-branch"); w.Code != 200 || w.Header().Get("X-Magpie-Recovery-State") != "native-backed" {
				t.Fatalf("capture: %d %s", w.Code, w.Body)
			}
			if _, err := recoverystore.Load("local", checkpointKey("local-C1")); err != nil {
				t.Fatal(err)
			}
			if err := provider.SetNativeCompaction("fake", false); err != nil {
				t.Fatal(err)
			}

			// The bounded writer cache can expire before the local copy does.
			checkpoints.Lock()
			delete(checkpoints.m, checkpointKey("local-C1"))
			checkpoints.Unlock()
			saveCheckpoints()
			next := withFields([]byte(recoveryNext("fake/m1", "local-C1", "continue")), map[string]any{
				"tools": json.RawMessage(`[{"type":"custom","name":"exec","description":"Execute source text.","format":{"type":"text"}}]`),
			})
			if _, ok := checkpointOf(next); ok {
				t.Fatal("legacy writer cache still contains the checkpoint")
			}
			a.reply = recoveryDone
			a.refuse = func(body []byte) (int, string) {
				if hasCustomTool(body) {
					return http.StatusBadRequest, deepSeekCustomRefusal
				}
				return 0, ""
			}
			s := New()
			if knownRefusal {
				s.markUnfit("fake", customToolRefused("m1"), provider.Responses)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(next)))
			r.RemoteAddr = "127.0.0.1:12345"
			r.Header.Set("Authorization", "Bearer magpie")
			r.Header.Set(SessionHeader, "seal-branch")
			w := httptest.NewRecorder()
			s.Front().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || a.calls != 2 || b.calls != 0 {
				t.Fatalf("custom-tool refusal must not retry without the seal: status=%d writer calls=%d other calls=%d body=%s", w.Code, a.calls, b.calls, w.Body)
			}
			if gjson.GetBytes(a.got, "input.0.encrypted_content").Str != "local-C1" || !hasCustomTool(a.got) {
				t.Fatalf("writer did not receive the sealed request unchanged: %s", a.got)
			}
		})
	}
}
