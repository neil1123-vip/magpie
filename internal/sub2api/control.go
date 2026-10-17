package sub2api

import (
	"path/filepath"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

var controlLocks sync.Map

// WithControl serializes short local control commits for one configuration directory.
// Callbacks must not request a session lock, perform HTTP, or wait for UI callbacks.
func WithControl(dir string, commit func() error) error {
	return withControl(dir, filelock.Lock, commit)
}

func withControl(dir string, lockFile func(string, time.Duration) (func(), bool), commit func() error) error {
	if dir == "" {
		return storageError()
	}
	path, err := filepath.Abs(filepath.Join(dir, "locks", "control.lock"))
	if err != nil {
		return storageError()
	}
	value, _ := controlLocks.LoadOrStore(path, new(sync.Mutex))
	lock := value.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	unlock, held := lockFile(path, filelock.Wait)
	if !held {
		return &Error{Kind: KindTemporary, Message: "Another Magpie process owns Sub2API control state"}
	}
	defer unlock()
	return commit()
}

func (m *Manager) withControl(commit func() error) error {
	return withControl(m.root, m.fileLock, commit)
}

func staleSession() error {
	return &Error{Kind: KindConflict, Code: "STALE_SESSION", Message: "Sub2API account changed; reload its current state"}
}

func nextGeneration(current uint64) (uint64, error) {
	// Generations cross JSON into JavaScript, so every accepted value stays exact.
	if current >= 1<<53-1 {
		return 0, storageError()
	}
	return current + 1, nil
}

// WithGeneration checks durable account control under the shared short lock.
// A provider may take its configuration commit lock inside commit, never a session lock.
func (m *Manager) WithGeneration(id string, expected uint64, commit func() error) error {
	return m.withControl(func() error {
		record, err := m.record(id)
		if err != nil {
			return err
		}
		if expected == 0 || record.View.Generation != expected || record.View.State == StateLoggedOut || record.Removed {
			return staleSession()
		}
		if commit != nil {
			return commit()
		}
		return nil
	})
}

func pendingAttempt(record accountRecord) string {
	if record.Pending != nil {
		return record.Pending.Attempt
	}
	return ""
}

func sameAccountStamp(left, right accountRecord) bool {
	return left.View.Generation == right.View.Generation && left.View.Version == right.View.Version &&
		pendingAttempt(left) == pendingAttempt(right) && left.Removed == right.Removed
}

func (m *Manager) checkCurrent(expected accountRecord) error {
	current, err := m.record(expected.View.ID)
	if err != nil {
		return err
	}
	if !sameAccountStamp(current, expected) {
		return staleSession()
	}
	return nil
}

func (m *Manager) commitRecord(before, after accountRecord) error {
	return m.withControl(func() error {
		if err := m.checkCurrent(before); err != nil {
			return err
		}
		return m.writeRecord(after)
	})
}
