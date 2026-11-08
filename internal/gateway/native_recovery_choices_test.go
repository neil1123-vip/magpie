package gateway

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/recoverystore"
)

func recoveryChoiceHome(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
}

func TestNativeRecoveryChoiceBoundAndOnce(t *testing.T) {
	recoveryChoiceHome(t)
	body := []byte(`{"model":"relay/m","input":[{"role":"user","content":"private history"}]}`)
	id, err := offerRecoveryChoice("caller-secret", "branch-secret", body, "storage_full", "native-only")
	if err != nil {
		t.Fatal(err)
	}
	same, err := offerRecoveryChoice("caller-secret", "branch-secret", body, "storage_full", "native-only")
	if err != nil || same != id {
		t.Fatalf("automatic retry changed the pending choice: %q %v", same, err)
	}
	if err := AllowRecovery(id, "excerpt"); err == nil {
		t.Fatal("an action not offered was authorized")
	}
	if ok, err := consumeRecoveryChoice("caller-secret", "branch-secret", body, "native-only"); ok || err != nil {
		t.Fatalf("an unconfirmed request was authorized: %v %v", ok, err)
	}
	if err := AllowRecovery(id, "native-only"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		scope, branch string
		body          []byte
	}{
		{"other-caller", "branch-secret", body},
		{"caller-secret", "other-branch", body},
		{"caller-secret", "branch-secret", append(append([]byte{}, body...), ' ')},
	} {
		if ok, err := consumeRecoveryChoice(c.scope, c.branch, c.body, "native-only"); ok || err != nil {
			t.Fatalf("permission escaped its exact request: %v %v", ok, err)
		}
	}
	b, err := os.ReadFile(filepath.Join(recoverystore.Directory(), "choices.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private history", "caller-secret", "branch-secret"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("queue stored private request data: %s", secret)
		}
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			ok, err := consumeRecoveryChoice("caller-secret", "branch-secret", body, "native-only")
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if n := accepted.Load(); n != 1 {
		t.Fatalf("one-time permission consumed %d times", n)
	}
}

func TestNativeRecoveryChoiceClearedExpiredAndDamaged(t *testing.T) {
	recoveryChoiceHome(t)
	body := []byte(`{"model":"m","input":[]}`)
	offer := func() string {
		t.Helper()
		id, err := offerRecoveryChoice("s", "b", body, "too_large", "excerpt")
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	id := offer()
	if err := AllowRecovery(id, "excerpt"); err != nil {
		t.Fatal(err)
	}
	if err := recoverystore.Clear(); err != nil {
		t.Fatal(err)
	}
	if ok, err := consumeRecoveryChoice("s", "b", body, "excerpt"); ok || err != nil {
		t.Fatalf("clear left old permission usable: %v %v", ok, err)
	}
	id = offer()
	name := filepath.Join(recoverystore.Directory(), "choices.json")
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var q recoveryChoiceQueue
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	q.Entries[0].Created = time.Now().Add(-2 * recoveryChoiceTTL)
	q.Entries[0].Expires = q.Entries[0].Created.Add(recoveryChoiceTTL)
	b, _ = json.Marshal(q)
	if err := os.WriteFile(name, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AllowRecovery(id, "excerpt"); err == nil {
		t.Fatal("an expired choice was authorized")
	}
	if err := os.WriteFile(name, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecoveryChoicesChecked(); err == nil {
		t.Fatal("a damaged queue was displayed as empty")
	}
	if _, err := consumeRecoveryChoice("s", "b", body, "excerpt"); err == nil {
		t.Fatal("a damaged queue was treated as a normal retry")
	}
	if _, err := offerRecoveryChoice("s", "", body, "too_large", "excerpt"); err == nil {
		t.Fatal("a request without a reliable branch got a shared authorization")
	}
}

func TestNativeRecoveryRequestNeedsExactToken(t *testing.T) {
	recoveryChoiceHome(t)
	body := []byte(`{"model":"m","input":["private"]}`)
	id, err := offerRecoveryRequest("caller", body, "storage_full")
	if err != nil {
		t.Fatal(err)
	}
	if err := AllowRecovery(id, "excerpt"); err == nil {
		t.Fatal("a request without branch identity allowed an excerpt")
	}
	if err := AllowRecovery(id, "native-only"); err != nil {
		t.Fatal(err)
	}
	if ok, err := consumeRecoveryChoice("caller", "", body, "native-only"); ok || err != nil {
		t.Fatalf("an automatic retry consumed request-only permission: %v %v", ok, err)
	}
	for _, c := range []struct {
		caller, token string
		body          []byte
	}{
		{"caller", "", body},
		{"caller", "other-token", body},
		{"other-caller", id, body},
		{"caller", id, []byte(`{"model":"m","input":["changed"]}`)},
	} {
		if ok, err := consumeRecoveryRequest(c.caller, c.body, c.token); ok || err != nil {
			t.Fatalf("token escaped its exact request: %v %v", ok, err)
		}
	}
	status, err := recoverystore.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if ok, generation, err := consumeRecoveryRequestAt("caller", body, id); !ok || err != nil || generation != status.Generation {
		t.Fatalf("exact request lost its authorization generation: %v %d, want %d: %v", ok, generation, status.Generation, err)
	}
	if ok, err := consumeRecoveryRequest("caller", body, id); ok || err != nil {
		t.Fatalf("request token reused: %v %v", ok, err)
	}
}
