package gateway

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/recoverystore"
)

const recoveryChoiceTTL = 30 * time.Minute
const maxRecoveryChoices = 256
const maxRecoveryChoicesBytes = 256 << 10

// RecoveryChoice contains no request body, credential, or raw caller identity.
// Branch is a short fingerprint for identifying the pending operation.
type RecoveryChoice struct {
	ID      string    `json:"id"`
	Model   string    `json:"model"`
	Branch  string    `json:"branch"`
	Reason  string    `json:"reason"`
	Actions []string  `json:"actions"`
	Created time.Time `json:"created"`
}

type pendingRecoveryChoice struct {
	RecoveryChoice
	Scope     string    `json:"scope"`
	Input     string    `json:"input"`
	BranchKey string    `json:"branchKey"`
	Expires   time.Time `json:"expires"`
	Allowed   string    `json:"allowed,omitempty"`
}

type recoveryChoiceQueue struct {
	Generation uint64                  `json:"generation"`
	Entries    []pendingRecoveryChoice `json:"entries"`
}

func recoveryFingerprint(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func withRecoveryChoices(change func(*recoveryChoiceQueue) (bool, error)) error {
	dir := recoverystore.Directory()
	// Stats creates/verifies the private directory; never weaken its permissions.
	if _, err := recoverystore.Stats(); err != nil {
		return err
	}
	unlock, held := filelock.Lock(filepath.Join(dir, "choices.lock"), filelock.Wait)
	if !held {
		return errors.New("cannot lock pending recovery choices")
	}
	defer unlock()
	status, err := recoverystore.Stats()
	if err != nil {
		return err
	}
	name := filepath.Join(dir, "choices.json")
	q := recoveryChoiceQueue{Generation: status.Generation}
	f, err := os.Open(name)
	if err == nil {
		b, readErr := io.ReadAll(io.LimitReader(f, maxRecoveryChoicesBytes+1))
		closeErr := f.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		if len(b) > maxRecoveryChoicesBytes {
			return errors.New("pending recovery choices exceed their size limit")
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&q); err != nil {
			return fmt.Errorf("pending recovery choices are damaged: %w", err)
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return errors.New("pending recovery choices are damaged")
		}
		if len(q.Entries) > maxRecoveryChoices {
			return errors.New("too many pending recovery choices")
		}
		for _, e := range q.Entries {
			requestOnly := e.BranchKey == "" && e.Branch == "request" && slices.Equal(e.Actions, []string{"native-only"})
			if e.ID == "" || len(e.ID) > 64 || len(e.Scope) != 64 || len(e.Input) != 64 || (len(e.BranchKey) != 64 && !requestOnly) ||
				len(e.Actions) == 0 || e.Created.IsZero() || !e.Expires.After(e.Created) ||
				e.Expires.Sub(e.Created) > recoveryChoiceTTL {
				return errors.New("pending recovery choices are damaged")
			}
			for _, action := range e.Actions {
				if action != "native-only" && action != "excerpt" {
					return errors.New("pending recovery choices contain an unknown action")
				}
			}
			if e.Allowed != "" && !slices.Contains(e.Actions, e.Allowed) {
				return errors.New("pending recovery choices contain an invalid permission")
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if q.Generation != status.Generation {
		q = recoveryChoiceQueue{Generation: status.Generation}
	}
	q.Entries = slices.DeleteFunc(q.Entries, func(e pendingRecoveryChoice) bool { return !e.Expires.After(time.Now()) })
	changed, err := change(&q)
	if err != nil || !changed {
		return err
	}
	b, err := json.Marshal(q)
	if err != nil {
		return err
	}
	if len(b) > maxRecoveryChoicesBytes {
		return errors.New("pending recovery choices exceed their size limit")
	}
	tmp, err := os.CreateTemp(dir, ".choices-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), name)
}

// RecoveryChoicesChecked reports read errors to management UIs and the CLI.
func RecoveryChoicesChecked() ([]RecoveryChoice, error) {
	choices := []RecoveryChoice{}
	err := withRecoveryChoices(func(q *recoveryChoiceQueue) (bool, error) {
		for _, e := range q.Entries {
			if e.Allowed == "" {
				c := e.RecoveryChoice
				c.Actions = slices.Clone(c.Actions)
				choices = append(choices, c)
			}
		}
		return false, nil
	})
	return choices, err
}

func RecoveryChoices() []RecoveryChoice {
	choices, _ := RecoveryChoicesChecked()
	return choices
}

func AllowRecovery(id, action string) error {
	if action != "native-only" && action != "excerpt" {
		return errors.New("recovery choice must be native-only or excerpt")
	}
	return withRecoveryChoices(func(q *recoveryChoiceQueue) (bool, error) {
		for i := range q.Entries {
			e := &q.Entries[i]
			if e.ID != id {
				continue
			}
			if !slices.Contains(e.Actions, action) {
				return false, errors.New("that recovery choice is not offered for this request")
			}
			e.Allowed = action
			return true, nil
		}
		return false, errors.New("this recovery choice expired or is no longer available")
	})
}

func offerRecoveryChoice(scope, branch string, body []byte, reason string, actions ...string) (string, error) {
	if scope == "" || branch == "" {
		return "", errors.New("a reliable caller and branch identity are required for a saved recovery choice")
	}
	return offerRecovery(scope, branch, body, reason, actions...)
}

func offerRecoveryRequest(scope string, body []byte, reason string) (string, error) {
	if scope == "" {
		return "", errors.New("a reliable caller identity is required for a recovery request")
	}
	return offerRecovery(scope, "", body, reason, "native-only")
}

func offerRecovery(scope, branch string, body []byte, reason string, actions ...string) (string, error) {
	if len(reason) > 1024 || len(actions) == 0 || len(actions) > 2 {
		return "", errors.New("invalid recovery choice")
	}
	for _, action := range actions {
		if action != "native-only" && action != "excerpt" {
			return "", errors.New("invalid recovery choice action")
		}
	}
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Model) > 1024 {
		return "", errors.New("invalid recovery request")
	}
	input, caller := recoveryFingerprint(string(body)), recoveryFingerprint(scope)
	branchKey, branchLabel := "", "request"
	if branch != "" {
		branchKey = recoveryFingerprint(branch)
		branchLabel = branchKey[:12]
	}
	var id string
	err := withRecoveryChoices(func(q *recoveryChoiceQueue) (bool, error) {
		for _, e := range q.Entries {
			if e.Scope == caller && e.Input == input && e.BranchKey == branchKey &&
				e.Reason == reason && slices.Equal(e.Actions, actions) {
				id = e.ID
				return false, nil
			}
		}
		if len(q.Entries) >= maxRecoveryChoices {
			return false, errors.New("too many requests are awaiting a recovery choice")
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return false, err
		}
		id = hex.EncodeToString(random[:])
		now := time.Now()
		q.Entries = append(q.Entries, pendingRecoveryChoice{
			RecoveryChoice: RecoveryChoice{
				ID: id, Model: request.Model, Branch: branchLabel, Reason: reason,
				Actions: slices.Clone(actions), Created: now,
			},
			Scope: caller, Input: input, BranchKey: branchKey, Expires: now.Add(recoveryChoiceTTL),
		})
		return true, nil
	})
	return id, err
}

func consumeRecoveryChoice(scope, branch string, body []byte, action string) (bool, error) {
	ok, _, err := consumeRecoveryChoiceAt(scope, branch, body, action)
	return ok, err
}

func consumeRecoveryChoiceAt(scope, branch string, body []byte, action string) (bool, uint64, error) {
	if scope == "" || branch == "" {
		return false, 0, nil
	}
	caller, input, branchKey := recoveryFingerprint(scope), recoveryFingerprint(string(body)), recoveryFingerprint(branch)
	consumed := false
	var generation uint64
	err := withRecoveryChoices(func(q *recoveryChoiceQueue) (bool, error) {
		for i, e := range q.Entries {
			if e.Scope == caller && e.Input == input && e.BranchKey == branchKey && e.Allowed == action && action != "" {
				q.Entries = slices.Delete(q.Entries, i, i+1)
				consumed = true
				generation = q.Generation
				return true, nil
			}
		}
		return false, nil
	})
	return consumed && err == nil, generation, err
}

func consumeRecoveryRequest(scope string, body []byte, id string) (bool, error) {
	ok, _, err := consumeRecoveryRequestAt(scope, body, id)
	return ok, err
}

func consumeRecoveryRequestAt(scope string, body []byte, id string) (bool, uint64, error) {
	if scope == "" || id == "" {
		return false, 0, nil
	}
	caller, input := recoveryFingerprint(scope), recoveryFingerprint(string(body))
	consumed := false
	var generation uint64
	err := withRecoveryChoices(func(q *recoveryChoiceQueue) (bool, error) {
		for i, e := range q.Entries {
			if e.ID == id && e.Scope == caller && e.Input == input && e.BranchKey == "" && e.Allowed == "native-only" {
				q.Entries = slices.Delete(q.Entries, i, i+1)
				consumed = true
				generation = q.Generation
				return true, nil
			}
		}
		return false, nil
	})
	return consumed && err == nil, generation, err
}
