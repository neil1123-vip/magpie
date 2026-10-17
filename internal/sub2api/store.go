package sub2api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	"github.com/yetone/magpie/internal/steady"
)

type pendingRotation struct {
	Version    uint64 `json:"version"`
	Generation uint64 `json:"generation,omitempty"`
	Hash       string `json:"hash"`
	Attempt    string `json:"attempt"`
}

type accountRecord struct {
	FormatVersion   uint64           `json:"formatVersion"`
	View            AccountView      `json:"account"`
	Pending         *pendingRotation `json:"pending,omitempty"`
	AutoDone        uint64           `json:"autoDone,omitempty"`
	AutoRecoveredAt time.Time        `json:"autoRecoveredAt,omitempty"`
	Removed         bool             `json:"removed,omitempty"`
}

type sessionSecret struct {
	FormatVersion uint64    `json:"formatVersion"`
	ID            string    `json:"id"`
	Base          string    `json:"base"`
	Subject       string    `json:"subject"`
	Email         string    `json:"email"`
	Proxy         string    `json:"proxy,omitempty"`
	UserAgent     string    `json:"userAgent,omitempty"`
	Version       uint64    `json:"version"`
	Generation    uint64    `json:"generation"`
	Attempt       string    `json:"attempt"`
	Tokens        tokenPair `json:"tokens"`
}

type savedPassword struct {
	FormatVersion uint64 `json:"formatVersion"`
	ID            string `json:"id"`
	Email         string `json:"email"`
	Password      string `json:"password"`
}

const sessionFormatVersion uint64 = 2

type unsavedSession struct {
	Before accountRecord
	Record accountRecord
	Secret sessionSecret
}

func accountID(base, subject string) string {
	sum := sha256.Sum256([]byte(base + "\x00" + subject))
	return hex.EncodeToString(sum[:])
}

func validID(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == sha256.Size
}

func (m *Manager) recordPath(id string) string { return filepath.Join(m.root, "accounts", id+".json") }
func (m *Manager) secretPath(id string) string { return filepath.Join(m.root, "secrets", id+".dpapi") }
func (m *Manager) passwordPath(id string) string {
	return filepath.Join(m.root, "secrets", id+"-password.dpapi")
}

func storageError() error {
	return &Error{Kind: KindStorage, Message: "Sub2API local credentials could not be stored or read"}
}

func checkLocalFormat(data []byte) error {
	var header *struct {
		FormatVersion uint64 `json:"formatVersion"`
	}
	if json.Unmarshal(data, &header) != nil || header == nil {
		return storageError()
	}
	if header.FormatVersion > sessionFormatVersion {
		return &Error{Kind: KindStorage, Code: "UNSUPPORTED_LOCAL_FORMAT", Message: "Sub2API local data needs a newer version of Magpie"}
	}
	return nil
}

func (m *Manager) writeRecord(record accountRecord) error {
	path := m.recordPath(record.View.ID)
	if err := checkRecordFormat(path); err != nil {
		return err
	}
	record.FormatVersion = sessionFormatVersion
	data, err := json.Marshal(record)
	if err != nil {
		return storageError()
	}
	return m.write(path, data)
}

func checkRecordFormat(path string) error {
	raw, err := steady.ReadThere(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return storageError()
	}
	if err == nil {
		if err := checkLocalFormat(raw); err != nil {
			return err
		}
	}
	return nil
}

func atomicFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return steady.WriteFile(path, data, 0o600)
}

func (m *Manager) saveSecret(path string, value any) error {
	if err := m.checkSecretFormat(path); err != nil {
		return err
	}
	plain, err := json.Marshal(value)
	if err != nil {
		return storageError()
	}
	if err := checkLocalFormat(plain); err != nil {
		clear(plain)
		return err
	}
	cipher, err := m.protect(plain)
	clear(plain)
	if err != nil {
		return storageError()
	}
	if m.write(path, cipher) != nil {
		return storageError()
	}
	return nil
}

func (m *Manager) checkSecretFormat(path string) error {
	cipher, err := steady.ReadThere(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storageError()
	}
	plain, err := m.unprotect(cipher)
	if err != nil {
		return storageError()
	}
	defer clear(plain)
	return checkLocalFormat(plain)
}

func (m *Manager) checkAccountSecrets(id string) error {
	for _, path := range []string{m.secretPath(id), m.passwordPath(id)} {
		if err := m.checkSecretFormat(path); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) removeSecret(path string) error {
	if err := m.checkSecretFormat(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return storageError()
	}
	return nil
}

// Only generation-checked logout cleanup may move unreadable ciphertext aside.
func (m *Manager) removeLoggedOutSecret(path string) error {
	cipher, err := steady.ReadThere(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return storageError()
	}
	plain, err := m.unprotect(cipher)
	if err == nil {
		clear(plain)
		return m.removeSecret(path)
	}
	dir, err := os.MkdirTemp(filepath.Dir(path), ".quarantine-")
	if err != nil {
		return storageError()
	}
	if err := steady.Rename(path, filepath.Join(dir, filepath.Base(path))); err != nil {
		_ = os.Remove(dir)
		return storageError()
	}
	return nil
}

func (m *Manager) readSecret(path string, value any) error {
	cipher, err := steady.ReadFile(path)
	if err != nil {
		return storageError()
	}
	plain, err := m.unprotect(cipher)
	if err != nil {
		return storageError()
	}
	defer clear(plain)
	if err := checkLocalFormat(plain); err != nil {
		return err
	}
	if json.Unmarshal(plain, value) != nil {
		return storageError()
	}
	return nil
}

func (m *Manager) record(id string) (accountRecord, error) {
	var record accountRecord
	if !validID(id) {
		return record, &Error{Kind: KindInvalid, Message: "Invalid Sub2API account"}
	}
	raw, err := steady.ReadThere(m.recordPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return record, &Error{Kind: KindAuth, Message: "Sign in to this Sub2API account"}
	}
	if err != nil {
		return record, storageError()
	}
	if err := checkLocalFormat(raw); err != nil {
		return record, err
	}
	if json.Unmarshal(raw, &record) != nil || record.View.ID != id {
		return record, storageError()
	}
	if record.FormatVersion < 2 && record.View.Generation == 0 && record.View.Version > 0 {
		record.View.Generation = 1
	}
	if record.View.Version > 0 && record.View.Generation == 0 {
		return record, storageError()
	}
	return record, nil
}

// load runs under the account lock; pending is cleared only after a new pair is durable.
func (m *Manager) load(id string) (accountRecord, sessionSecret, error) {
	record, err := m.record(id)
	if err != nil {
		return record, sessionSecret{}, err
	}
	if record.Removed || record.View.State == StateLoggedOut && record.Pending == nil {
		return record, sessionSecret{}, &Error{Kind: KindAuth, Message: "This Sub2API account is signed out"}
	}
	if err := m.checkAccountSecrets(id); err != nil {
		return record, sessionSecret{}, err
	}
	m.mu.Lock()
	unsaved, ok := m.unsaved[id]
	m.mu.Unlock()
	if ok {
		// A later login or logout always wins over an unsaved old rotation.
		if record.Pending != nil && sameAccountStamp(record, unsaved.Before) {
			if err := m.withControl(func() error {
				if err := m.checkCurrent(unsaved.Before); err != nil {
					return err
				}
				return m.persistLocked(unsaved.Before, unsaved.Record, unsaved.Secret)
			}); err != nil {
				return unsaved.Record, unsaved.Secret, err
			}
			record = unsaved.Record
		} else {
			m.mu.Lock()
			delete(m.unsaved, id)
			m.mu.Unlock()
		}
	}
	var secret sessionSecret
	if err := m.readSecret(m.secretPath(id), &secret); err != nil {
		return record, secret, err
	}
	if secret.FormatVersion < 2 && secret.Generation == 0 {
		secret.Generation = record.View.Generation
	}
	if secret.ID != id || secret.Base != record.View.Base || secret.Subject != record.View.Subject {
		return record, secret, storageError()
	}
	if p := record.Pending; p != nil {
		generation := p.Generation
		if generation == 0 && record.FormatVersion < 2 {
			generation = record.View.Generation
		}
		if secret.Version == p.Version+1 && secret.Attempt == p.Attempt && secret.Generation == generation {
			before := record
			if secret.Generation != record.View.Generation {
				record.AutoDone, record.AutoRecoveredAt = 0, time.Time{}
				record.View.RetryAt = time.Time{}
				record.View.Context = panelNetworkContext(secret.Proxy, secret.UserAgent)
			}
			record.View.Generation, record.View.CleanupPending, record.Removed = secret.Generation, false, false
			record.View.Version = secret.Version
			record.View.State, record.View.LastError = StateReady, ""
			record.View.UpdatedAt, record.View.LastSuccess = m.now(), m.now()
			record.View.Email, record.View.Proxy, record.View.UserAgent = secret.Email, netproxy.Redact(secret.Proxy), secret.UserAgent
			record.Pending = nil
			if err := m.commitRecord(before, record); err != nil {
				return record, secret, err
			}
		} else {
			record.View.State = StateUnknown
			record.View.LastError = "A previous refresh may have consumed its token; sign in again"
		}
	}
	if secret.Version != record.View.Version || secret.Generation != record.View.Generation {
		return record, secret, storageError()
	}
	return record, secret, nil
}

func (m *Manager) persist(record accountRecord, secret sessionSecret) error {
	return m.withControl(func() error {
		before, err := m.record(record.View.ID)
		if err != nil {
			return err
		}
		if before.View.Generation != record.View.Generation || before.Removed || before.View.State == StateLoggedOut {
			return staleSession()
		}
		if secret.Generation == 0 {
			secret.Generation = record.View.Generation
		}
		if record.View.Generation != secret.Generation || record.View.Version != secret.Version {
			return staleSession()
		}
		if pending := before.Pending; pending != nil {
			if pending.Attempt != secret.Attempt || pending.Version+1 != secret.Version || pending.Generation != secret.Generation {
				return staleSession()
			}
		} else if before.View.Version != secret.Version && before.View.Version+1 != secret.Version {
			return staleSession()
		}
		return m.persistLocked(before, record, secret)
	})
}

// persistLocked runs with the account lock followed by the control lock.
func (m *Manager) persistLocked(before, record accountRecord, secret sessionSecret) error {
	id := record.View.ID
	if err := checkRecordFormat(m.recordPath(id)); err != nil {
		return err
	}
	if err := m.checkAccountSecrets(id); err != nil {
		return err
	}
	secret.FormatVersion = sessionFormatVersion
	m.mu.Lock()
	m.unsaved[id] = unsavedSession{Before: before, Record: record, Secret: secret}
	m.mu.Unlock()
	if err := m.saveSecret(m.secretPath(id), secret); err != nil {
		return err
	}
	if err := m.writeRecord(record); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.unsaved, id)
	m.mu.Unlock()
	return nil
}

func (m *Manager) lockAccount(id string) (func(), error) {
	return m.lockAccountWait(id, filelock.Wait)
}

func (m *Manager) lockAccountWait(id string, wait time.Duration) (func(), error) {
	if !validID(id) {
		return nil, &Error{Kind: KindInvalid, Message: "Invalid Sub2API account"}
	}
	m.mu.Lock()
	lock := m.locks[id]
	if lock == nil {
		lock = new(sync.Mutex)
		m.locks[id] = lock
	}
	m.mu.Unlock()
	start := time.Now()
	for !lock.TryLock() {
		if time.Since(start) >= wait {
			return nil, &Error{Kind: KindTemporary, Message: "Another operation owns this Sub2API session"}
		}
		time.Sleep(5 * time.Millisecond)
	}
	remaining := max(wait-time.Since(start), 0)
	unlock, held := m.fileLock(filepath.Join(m.root, "locks", id+".lock"), remaining)
	if !held {
		lock.Unlock()
		return nil, &Error{Kind: KindTemporary, Message: "Another Magpie process owns this Sub2API session"}
	}
	return func() { unlock(); lock.Unlock() }, nil
}

func safeView(record accountRecord) AccountView {
	view := record.View
	view.Proxy = netproxy.Redact(view.Proxy)
	return view
}

func (m *Manager) Accounts() ([]AccountView, error) {
	m.pruneAttempts()
	dir := filepath.Join(m.root, "accounts")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []AccountView{}, nil
	}
	if err != nil {
		return nil, storageError()
	}
	out := make([]AccountView, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		view, err := m.Info(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		record, err := m.record(view.ID)
		if err != nil {
			return nil, err
		}
		if record.View.CleanupPending {
			m.startCleanup(record.View.ID, record.View.Generation)
		}
		if !record.Removed {
			out = append(out, view)
		}
	}
	return out, nil
}

func (m *Manager) Info(id string) (AccountView, error) {
	m.pruneAttempts()
	record, err := m.record(id)
	if err != nil {
		return AccountView{}, err
	}
	m.mu.Lock()
	unsaved, ok := m.unsaved[id]
	m.mu.Unlock()
	if ok && record.Pending != nil && sameAccountStamp(record, unsaved.Before) {
		generation := record.View.Generation
		record = unsaved.Record
		record.View.Generation = generation
		record.View.State = StateTemporary
		record.View.LastError = "New Sub2API credentials are waiting for local storage"
	} else if record.Pending != nil {
		record.View.State = StateUnknown
		record.View.LastError = "A previous refresh result is not yet confirmed"
	}
	return safeView(record), nil
}

func (m *Manager) saveFailure(record *accountRecord, state string, err error, keepPending bool) error {
	return m.saveFailureWithDelay(record, state, err, keepPending, 0)
}

func (m *Manager) saveFailureWithDelay(record *accountRecord, state string, err error, keepPending bool, delay time.Duration) error {
	before := *record
	if state == StateUnknown {
		detail := &Error{Kind: KindUnknown, Message: "The previous refresh result is uncertain; sign in again"}
		var original *Error
		if errors.As(err, &original) {
			copied := *original
			detail = &copied
		}
		detail.Code, err = "REFRESH_UNCERTAIN", detail
	}
	record.View.State, record.View.LastError = state, err.Error()
	record.View.UpdatedAt = m.now()
	record.View.RetryAt = time.Time{}
	var detail *Error
	if errors.As(err, &detail) && detail.RetryAfter > 0 {
		delay = detail.RetryAfter
	}
	if delay > 0 {
		record.View.RetryAt = m.now().Add(delay)
	}
	if !keepPending {
		record.Pending = nil
	}
	if err := m.commitRecord(before, *record); err != nil {
		return err
	}
	return err
}
