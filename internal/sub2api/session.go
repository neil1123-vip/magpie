package sub2api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/netproxy"
	"golang.org/x/net/http/httpguts"
)

type loginAttempt struct {
	Request     LoginRequest
	Generations map[string]uint64
	Password    []byte
	Temporary   string
	Expires     time.Time
	Canceled    bool
	Busy        bool
	Cancel      context.CancelFunc
	Context     context.Context
}

type Manager struct {
	root      string
	mu        sync.Mutex
	attempts  map[string]*loginAttempt
	latest    map[string]string
	locks     map[string]*sync.Mutex
	unsaved   map[string]unsavedSession
	cleanups  map[string]uint64
	transport http.RoundTripper
	protect   func([]byte) ([]byte, error)
	unprotect func([]byte) ([]byte, error)
	write     func(string, []byte) error
	fileLock  func(string, time.Duration) (func(), bool)
	now       func() time.Time
}

func New(root string) *Manager {
	transport := http.DefaultTransport
	if base, ok := transport.(*http.Transport); ok {
		transport = netproxy.Dispatch(base)
	}
	return &Manager{
		root: root, attempts: make(map[string]*loginAttempt), latest: make(map[string]string),
		locks: make(map[string]*sync.Mutex), unsaved: make(map[string]unsavedSession), cleanups: make(map[string]uint64),
		transport: transport, protect: protectSecret, unprotect: unprotectSecret,
		write: atomicFile, fileLock: filelock.Lock, now: time.Now,
	}
}

func randomID() (string, error) {
	var data [24]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", &Error{Kind: KindTemporary, Message: "A secure login attempt could not be created"}
	}
	return hex.EncodeToString(data[:]), nil
}

func (m *Manager) Login(ctx context.Context, input LoginRequest) (LoginResult, error) {
	var generations map[string]uint64
	if input.SessionID != "" {
		if input.Target != "" || input.Revision != "" || input.Base != "" || input.Proxy != "" || input.UserAgent != "" {
			return LoginResult{}, &Error{Kind: KindInvalid, Message: "Account sign-in uses its saved network settings"}
		}
		var err error
		input, err = m.accountLoginInput(input)
		if err != nil {
			return LoginResult{}, err
		}
		generations = map[string]uint64{input.SessionID: input.ExpectedGeneration}
	} else if input.ExpectedGeneration != 0 {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "An account is required for the expected session generation"}
	}
	base, err := NormalizeBase(input.Base)
	if err != nil {
		return LoginResult{}, err
	}
	input.Base, input.Email = base, strings.TrimSpace(input.Email)
	if input.Email == "" || input.Password == "" || input.SessionID == "" && (input.Target == "" || input.Revision == "") {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Panel, account, password and current source are required"}
	}
	if strings.ContainsAny(input.UserAgent, "\r\n") {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Invalid panel user agent"}
	}
	if input.UserAgent == "" {
		input.UserAgent = defaultUserAgent
	}
	input.Proxy, err = effectiveProxy(base, input.Proxy)
	if err != nil {
		return LoginResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return LoginResult{}, err
	}
	m.cancelBrowserTarget(input)
	unlockAuth, err := m.lockAuth(base)
	if err != nil {
		return LoginResult{}, err
	}
	defer unlockAuth()
	// Fail before sending credentials when this platform cannot preserve a session.
	probe, err := m.protect([]byte("Sub2API"))
	if err != nil || len(probe) == 0 {
		return LoginResult{}, storageError()
	}
	id, err := randomID()
	if err != nil {
		return LoginResult{}, err
	}
	if input.SessionID == "" {
		generations, err = m.loginGenerations(base)
		if err != nil {
			return LoginResult{}, err
		}
	}
	attempt := &loginAttempt{Request: input, Generations: generations, Expires: m.now().Add(5 * time.Minute), Busy: true}
	attempt.Request.Password = ""
	if input.Remember {
		attempt.Password, err = m.protect([]byte(input.Password))
		if err != nil {
			return LoginResult{}, storageError()
		}
	}
	m.mu.Lock()
	m.pruneAttemptsLocked()
	target := loginTarget(input)
	if old := m.attempts[m.latest[target]]; old != nil {
		cancelAttempt(old)
	}
	m.attempts[id], m.latest[target] = attempt, id
	m.mu.Unlock()

	if input.SessionID != "" {
		if err := m.WithGeneration(input.SessionID, input.ExpectedGeneration, nil); err != nil {
			m.Cancel(id)
			return LoginResult{}, err
		}
	}
	owner, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer func() {
		if owner.Err() != nil {
			m.Cancel(id)
		}
		cancel()
	}()
	secret := sessionSecret{Base: base, Email: input.Email, Proxy: input.Proxy, UserAgent: input.UserAgent}
	raw, err := m.request(owner, secret, http.MethodPost, base+"/api/v1/auth/login", nil,
		map[string]string{"email": input.Email, "password": input.Password}, "")
	if err != nil {
		m.Cancel(id)
		return LoginResult{}, err
	}
	tokens, err := parseTokens(raw, m.now())
	if err != nil {
		m.Cancel(id)
		return LoginResult{}, err
	}
	m.mu.Lock()
	attempt.Busy = false
	if tokens.TwoFA {
		attempt.Temporary = tokens.Temporary
	}
	active := m.attemptActiveLocked(id)
	m.mu.Unlock()
	if !active {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "This Sub2API login was canceled or replaced"}
	}
	if tokens.TwoFA {
		return LoginResult{State: StateTwoFA, Attempt: id, Target: input.Target, Revision: input.Revision, SessionID: input.SessionID}, nil
	}
	return m.finishLogin(owner, id, attempt, tokens, input.SessionID)
}

func (m *Manager) accountLoginInput(input LoginRequest) (LoginRequest, error) {
	unlock, err := m.lockAccount(input.SessionID)
	if err != nil {
		return input, err
	}
	defer unlock()
	record, err := m.record(input.SessionID)
	if err != nil {
		return input, err
	}
	if input.ExpectedGeneration == 0 || record.View.Generation != input.ExpectedGeneration {
		return input, staleSession()
	}
	if record.Removed || record.View.State == StateLoggedOut {
		return input, &Error{Kind: KindConflict, Code: "ACCOUNT_CONTEXT_REQUIRED", Message: "Sign in from a source to restore this account's network settings"}
	}
	record, secret, err := m.load(input.SessionID)
	if err != nil {
		return input, err
	}
	if record.View.Generation != input.ExpectedGeneration {
		return input, staleSession()
	}
	input.Base, input.Proxy, input.UserAgent = secret.Base, secret.Proxy, secret.UserAgent
	return input, nil
}

func (m *Manager) loginGenerations(base string) (map[string]uint64, error) {
	generations := make(map[string]uint64)
	err := m.withControl(func() error {
		entries, err := os.ReadDir(filepath.Join(m.root, "accounts"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return storageError()
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			record, err := m.record(strings.TrimSuffix(entry.Name(), ".json"))
			if err != nil {
				return err
			}
			if record.View.Base != base {
				continue
			}
			if err := m.checkAccountSecrets(record.View.ID); err != nil {
				return err
			}
			generations[record.View.ID] = record.View.Generation
		}
		return nil
	})
	return generations, err
}

func loginTarget(input LoginRequest) string {
	if input.SessionID != "" {
		return "account:" + input.SessionID
	}
	return "source:" + input.Target
}

func (m *Manager) attemptActiveLocked(id string) bool {
	a := m.attempts[id]
	return a != nil && !a.Canceled && (a.Context == nil || a.Context.Err() == nil) &&
		a.Expires.After(m.now()) && m.latest[loginTarget(a.Request)] == id
}

func (m *Manager) ValidateAttempt(id, target, revision string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneAttemptsLocked()
	a := m.attempts[id]
	if !m.attemptActiveLocked(id) || a.Request.SessionID != "" || a.Request.Target != target || a.Request.Revision != revision {
		return &Error{Kind: KindConflict, Message: "Sub2API login settings changed or expired; sign in again"}
	}
	return nil
}

func (m *Manager) ValidateAccountAttempt(id, session string, expected uint64) error {
	m.mu.Lock()
	m.pruneAttemptsLocked()
	a := m.attempts[id]
	active := m.attemptActiveLocked(id) && session != "" && a.Request.SessionID == session && a.Request.ExpectedGeneration == expected
	m.mu.Unlock()
	if !active {
		return &Error{Kind: KindConflict, Message: "Sub2API account sign-in changed or expired; sign in again"}
	}
	return m.WithGeneration(session, expected, nil)
}

func (m *Manager) Cancel(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.attempts[id]; a != nil {
		cancelAttempt(a)
	}
}

func (m *Manager) pruneAttempts() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneAttemptsLocked()
}

func (m *Manager) pruneAttemptsLocked() {
	for id, a := range m.attempts {
		if !a.Canceled && a.Expires.After(m.now()) {
			continue
		}
		cancelAttempt(a)
		delete(m.attempts, id)
		target := loginTarget(a.Request)
		if m.latest[target] == id {
			delete(m.latest, target)
		}
	}
}

func (m *Manager) Complete2FA(ctx context.Context, id, code string) (LoginResult, error) {
	code = strings.TrimSpace(code)
	if len(code) != 6 || strings.IndexFunc(code, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Enter the six digit verification code"}
	}
	m.mu.Lock()
	m.pruneAttemptsLocked()
	a := m.attempts[id]
	if a == nil || !m.attemptActiveLocked(id) || a.Busy || a.Temporary == "" {
		m.mu.Unlock()
		return LoginResult{}, &Error{Kind: KindConflict, Message: "Sub2API verification expired or is already in progress"}
	}
	a.Busy = true
	request, temporary := a.Request, a.Temporary
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		a.Busy = false
		m.mu.Unlock()
	}()
	if err := ctx.Err(); err != nil {
		return LoginResult{}, err
	}
	unlockAuth, err := m.lockAuth(request.Base)
	if err != nil {
		return LoginResult{}, err
	}
	defer unlockAuth()
	if request.SessionID != "" {
		if err := m.WithGeneration(request.SessionID, request.ExpectedGeneration, nil); err != nil {
			m.Cancel(id)
			return LoginResult{}, err
		}
	}
	owner, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer func() {
		if owner.Err() != nil {
			m.Cancel(id)
		}
		cancel()
	}()
	secret := sessionSecret{Base: request.Base, Email: request.Email, Proxy: request.Proxy, UserAgent: request.UserAgent}
	raw, err := m.request(owner, secret, http.MethodPost, request.Base+"/api/v1/auth/login/2fa", nil,
		map[string]string{"temp_token": temporary, "totp_code": code}, "")
	if err != nil {
		return LoginResult{}, err
	}
	tokens, err := parseTokens(raw, m.now())
	if err != nil || tokens.TwoFA {
		if err == nil {
			err = &Error{Kind: KindAuth, Message: "Sub2API verification did not complete"}
		}
		return LoginResult{}, err
	}
	return m.finishLogin(owner, id, a, tokens, request.SessionID)
}

func (m *Manager) finishLogin(ctx context.Context, attemptID string, attempt *loginAttempt, tokens tokenPair, expectedID string) (LoginResult, error) {
	m.mu.Lock()
	input := attempt.Request
	passwordCipher := append([]byte(nil), attempt.Password...)
	active := m.attemptActiveLocked(attemptID)
	m.mu.Unlock()
	if !active {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "This Sub2API login was canceled or replaced"}
	}
	if input.Remember {
		plain, err := m.unprotect(passwordCipher)
		if err != nil {
			return LoginResult{}, storageError()
		}
		input.Password = string(plain)
		clear(plain)
	}
	clear(passwordCipher)
	subject, _, err := m.loginIdentity(ctx, input, tokens)
	if err != nil {
		return LoginResult{}, err
	}
	id := accountID(input.Base, subject)
	if expectedID != "" && expectedID != id {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "Sub2API re-login returned a different account"}
	}
	unlock, err := m.lockAccount(id)
	if err != nil {
		return LoginResult{}, err
	}
	defer unlock()
	return m.commitLogin(ctx, id, subject, attemptID, input, tokens, attempt.Generations)
}

func (m *Manager) loginIdentity(ctx context.Context, input LoginRequest, tokens tokenPair) (string, string, error) {
	secret := sessionSecret{Base: input.Base, Email: input.Email, Proxy: input.Proxy, UserAgent: input.UserAgent}
	raw, err := m.request(ctx, secret, http.MethodGet, input.Base+"/api/v1/auth/me", nil, nil, tokens.Access)
	if err != nil {
		return "", "", err
	}
	var identity struct {
		Data struct {
			ID    json.RawMessage `json:"id"`
			Email string          `json:"email"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &identity) != nil {
		return "", "", &Error{Kind: KindUnknown, Message: "Sub2API account identity could not be confirmed"}
	}
	var subject string
	if json.Unmarshal(identity.Data.ID, &subject) != nil {
		var number json.Number
		if json.Unmarshal(identity.Data.ID, &number) != nil {
			return "", "", &Error{Kind: KindUnknown, Message: "Sub2API account identity is missing"}
		}
		subject = number.String()
	}
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "", "", &Error{Kind: KindUnknown, Message: "Sub2API account identity is missing"}
	}
	return subject, strings.TrimSpace(identity.Data.Email), nil
}

func (m *Manager) commitLogin(ctx context.Context, id, subject, attemptID string, input LoginRequest, tokens tokenPair, generations map[string]uint64) (LoginResult, error) {
	var result LoginResult
	err := m.withControl(func() error {
		var err error
		result, err = m.commitLoginLocked(ctx, id, subject, attemptID, input, tokens, generations)
		return err
	})
	return result, err
}

func (m *Manager) commitLoginLocked(ctx context.Context, id, subject, attemptID string, input LoginRequest, tokens tokenPair, generations map[string]uint64) (LoginResult, error) {
	m.mu.Lock()
	active := m.attemptActiveLocked(attemptID)
	m.mu.Unlock()
	if !active {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "This Sub2API login was canceled or replaced"}
	}
	record, err := m.record(id)
	if err != nil {
		var detail *Error
		if !errors.As(err, &detail) || detail.Kind != KindAuth {
			return LoginResult{}, err
		}
		record.View = AccountView{ID: id, Base: input.Base, Subject: subject}
	}
	if generations[id] != record.View.Generation {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "Sub2API account changed during sign-in; start again"}
	}
	if err := m.checkAccountSecrets(id); err != nil {
		return LoginResult{}, err
	}
	if record.View.State != StateLoggedOut && record.View.Version > 0 {
		var old sessionSecret
		if err := m.readSecret(m.secretPath(id), &old); err != nil {
			return LoginResult{}, err
		}
		if old.Proxy != input.Proxy || old.UserAgent != input.UserAgent {
			return LoginResult{}, &Error{Kind: KindConflict, Message: "Shared Sub2API accounts must use the same proxy and user agent"}
		}
	}
	generation, err := nextGeneration(record.View.Generation)
	if err != nil || record.View.Version == ^uint64(0) {
		return LoginResult{}, storageError()
	}
	secret := sessionSecret{ID: id, Base: input.Base, Subject: subject, Email: input.Email, Proxy: input.Proxy, UserAgent: input.UserAgent,
		Version: record.View.Version + 1, Generation: generation, Attempt: attemptID, Tokens: tokens}
	if input.browser {
		input.Remember = record.View.Remember && record.View.Email == input.Email
	}
	preservePassword := input.browser && input.Remember
	record.View.Remember = input.Remember
	record.Pending = &pendingRotation{Version: record.View.Version, Generation: generation, Attempt: attemptID}
	// Serialize transaction acceptance with Cancel; later cancellation does not roll it back.
	m.mu.Lock()
	active, err = m.attemptActiveLocked(attemptID), ctx.Err()
	m.mu.Unlock()
	if err != nil {
		return LoginResult{}, err
	}
	if !active {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "This Sub2API login was canceled or replaced"}
	}
	if err := m.writeRecord(record); err != nil {
		return LoginResult{}, err
	}
	before := record
	if input.Remember && !preservePassword {
		if err := m.saveSecret(m.passwordPath(id), savedPassword{FormatVersion: sessionFormatVersion, ID: id, Email: input.Email, Password: input.Password}); err != nil {
			return LoginResult{}, err
		}
	} else if !preservePassword {
		if err := m.removeSecret(m.passwordPath(id)); err != nil {
			return LoginResult{}, err
		}
	}
	record.View.Email, record.View.Version = input.Email, secret.Version
	record.View.Generation, record.View.CleanupPending, record.Removed = generation, false, false
	record.View.Proxy, record.View.UserAgent = netproxy.Redact(input.Proxy), input.UserAgent
	record.View.Context = panelNetworkContext(input.Proxy, input.UserAgent)
	record.View.Remember, record.View.State, record.View.LastError = input.Remember, StateReady, ""
	record.View.UpdatedAt, record.View.LastSuccess = m.now(), m.now()
	record.View.RetryAt = time.Time{}
	record.AutoDone, record.Pending = 0, nil
	record.AutoRecoveredAt = time.Time{}
	if err := m.persistLocked(before, record, secret); err != nil {
		return LoginResult{}, err
	}
	m.Cancel(attemptID)
	view := safeView(record)
	return LoginResult{State: StateReady, Account: &view, Target: input.Target, Revision: input.Revision, SessionID: input.SessionID}, nil
}

func (m *Manager) Logout(ctx context.Context, id string) error {
	view, err := m.Info(id)
	if err != nil {
		return err
	}
	_, err = m.LogoutExpected(ctx, id, view.Generation)
	return err
}

// LogoutExpected invalidates local work before waiting for an old network owner.
func (m *Manager) LogoutExpected(ctx context.Context, id string, expected uint64) (LogoutResult, error) {
	if err := ctx.Err(); err != nil {
		return LogoutResult{}, err
	}
	var result LogoutResult
	err := m.withControl(func() error {
		record, err := m.record(id)
		if err != nil {
			return err
		}
		if record.View.Generation != expected {
			return staleSession()
		}
		generation, err := nextGeneration(record.View.Generation)
		if err != nil {
			return err
		}
		record.View.Generation = generation
		record.View.State, record.View.Remember, record.View.LastError = StateLoggedOut, false, ""
		record.View.UpdatedAt, record.View.RetryAt, record.Pending = m.now(), time.Time{}, nil
		record.View.CleanupPending = true
		record.AutoDone, record.AutoRecoveredAt = 0, time.Time{}
		if err := m.writeRecord(record); err != nil {
			return err
		}
		result.Generation, result.CleanupPending = record.View.Generation, record.View.CleanupPending
		m.mu.Lock()
		delete(m.unsaved, id)
		for _, attempt := range m.attempts {
			if attempt.Request.Base == record.View.Base && (attempt.Request.browser || attempt.Request.Email == record.View.Email) &&
				attempt.Generations[id] < record.View.Generation {
				cancelAttempt(attempt)
			}
		}
		m.mu.Unlock()
		return nil
	})
	if err != nil {
		return result, err
	}
	if !result.CleanupPending {
		return result, nil
	}
	unlock, err := m.lockAccountWait(id, 0)
	if err != nil {
		m.startCleanup(id, result.Generation)
		return result, nil
	}
	result.CleanupPending, err = m.cleanupLocked(id, result.Generation)
	unlock()
	if err != nil {
		result.CleanupError = err.Error()
	}
	return result, nil
}

func (m *Manager) startCleanup(id string, generation uint64) {
	m.mu.Lock()
	if current, exists := m.cleanups[id]; exists {
		m.cleanups[id] = max(current, generation)
		m.mu.Unlock()
		return
	}
	m.cleanups[id] = generation
	m.mu.Unlock()
	go func() {
		for {
			unlock, err := m.lockAccount(id)
			if err == nil {
				m.cleanupLocked(id, generation)
				unlock()
			}
			m.mu.Lock()
			latest := m.cleanups[id]
			if latest <= generation {
				delete(m.cleanups, id)
				m.mu.Unlock()
				return
			}
			generation = latest
			m.mu.Unlock()
		}
	}()
}

// cleanupLocked takes control only after the session owner has released its lock.
func (m *Manager) cleanupLocked(id string, generation uint64) (bool, error) {
	pending := true
	err := m.withControl(func() error {
		record, err := m.record(id)
		if err != nil {
			return err
		}
		if record.View.Generation != generation || record.View.State != StateLoggedOut || !record.View.CleanupPending {
			pending = false
			return nil
		}
		var cleanupErr error
		for _, path := range []string{m.secretPath(id), m.passwordPath(id)} {
			if err := m.removeLoggedOutSecret(path); err != nil && cleanupErr == nil {
				cleanupErr = err
			}
		}
		if cleanupErr != nil {
			return cleanupErr
		}
		record.View.CleanupPending = false
		if err := m.writeRecord(record); err != nil {
			return err
		}
		pending = false
		return nil
	})
	return pending, err
}

func (m *Manager) Read(ctx context.Context, id, method, rawURL string, headers http.Header) ([]byte, error) {
	return m.read(ctx, id, method, rawURL, headers, true)
}

func (m *Manager) ReadRaw(ctx context.Context, id, method, rawURL string, headers http.Header) ([]byte, error) {
	return m.read(ctx, id, method, rawURL, headers, false)
}

// CheckContext compares the full encrypted request settings, never their redacted DTO.
func (m *Manager) CheckContext(id, proxy, userAgent string) error {
	unlock, err := m.lockAccount(id)
	if err != nil {
		return err
	}
	defer unlock()
	_, secret, err := m.load(id)
	if err != nil {
		return err
	}
	proxy, err = effectiveProxy(secret.Base, proxy)
	if err != nil {
		return err
	}
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	if proxy != secret.Proxy || userAgent != secret.UserAgent {
		return &Error{Kind: KindConflict, Message: "Shared Sub2API accounts must use the same proxy and user agent"}
	}
	return nil
}

// NetworkContext identifies panel request settings without exposing them.
func (m *Manager) NetworkContext(id string) (string, error) {
	unlock, err := m.lockAccount(id)
	if err != nil {
		return "", err
	}
	defer unlock()
	_, secret, err := m.load(id)
	if err != nil {
		return "", err
	}
	return panelNetworkContext(secret.Proxy, secret.UserAgent), nil
}

func panelNetworkContext(proxy, userAgent string) string {
	sum := sha256.Sum256([]byte(proxy + "\x00" + userAgent))
	return hex.EncodeToString(sum[:])
}

// Refresh explicitly renews a session, for a caller that must not replay a POST.
func (m *Manager) Refresh(ctx context.Context, id string) error {
	view, err := m.Info(id)
	if err != nil {
		return err
	}
	return m.refresh(ctx, id, view.Version)
}

func (m *Manager) read(ctx context.Context, id, method, rawURL string, headers http.Header, renew bool) ([]byte, error) {
	method = strings.ToUpper(strings.TrimSpace(method))
	if method == "" {
		method = http.MethodGet
	}
	if method != http.MethodGet && method != http.MethodPost && method != http.MethodHead {
		return nil, &Error{Kind: KindInvalid, Message: "Unsupported panel read method"}
	}
	unlock, err := m.lockAccount(id)
	if err != nil {
		return nil, err
	}
	record, secret, err := m.load(id)
	unlock()
	if err != nil {
		return nil, err
	}
	raw, err := m.request(ctx, secret, method, rawURL, headers, nil, secret.Tokens.Access)
	if err == nil || !renew || method != http.MethodGet && method != http.MethodPost || !accessRejected(err) {
		return raw, err
	}
	if record.View.State == StateUnknown {
		return nil, &Error{Kind: KindUnknown, Code: "REFRESH_UNCERTAIN", Message: "The previous refresh result is uncertain; sign in again"}
	}
	if err := m.refresh(ctx, id, secret.Version); err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		return nil, &Error{Kind: KindRenewed, Message: "Sub2API session renewed; query again"}
	}
	return m.read(ctx, id, method, rawURL, headers, false)
}

func refreshFingerprint(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

type refreshKey struct {
	root, id            string
	generation, version uint64
}

type refreshCall struct {
	done chan struct{}
	err  error
}

var refreshCalls sync.Map

func (m *Manager) refresh(ctx context.Context, id string, version uint64) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	initial, err := m.record(id)
	if err != nil {
		return err
	}
	if initial.View.Version != version {
		return nil
	}
	root, err := filepath.Abs(m.root)
	if err != nil {
		return storageError()
	}
	key := refreshKey{root: root, id: id, generation: initial.View.Generation, version: version}
	call := &refreshCall{done: make(chan struct{})}
	shared, loaded := refreshCalls.LoadOrStore(key, call)
	if loaded {
		call = shared.(*refreshCall)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-call.done:
			return call.err
		}
	}
	defer func() {
		call.err = err
		close(call.done)
		refreshCalls.Delete(key)
	}()
	return m.refreshOwned(ctx, id, version, initial)
}

func (m *Manager) refreshOwned(ctx context.Context, id string, version uint64, initial accountRecord) error {
	unlockAuth, err := m.lockAuth(initial.View.Base)
	if err != nil {
		latest, readErr := m.record(id)
		if readErr == nil && latest.View.Generation == initial.View.Generation &&
			latest.View.Version != version && latest.View.State == StateReady {
			return nil
		}
		return err
	}
	defer unlockAuth()
	unlock, err := m.lockAccount(id)
	if err != nil {
		return err
	}
	defer unlock()
	record, secret, err := m.load(id)
	if err != nil {
		return err
	}
	if secret.Version != version {
		return nil
	}
	switch record.View.State {
	case StateUnknown:
		return &Error{Kind: KindUnknown, Code: "REFRESH_UNCERTAIN", Message: "The previous refresh result is uncertain; sign in again"}
	case StateLogin:
		if record.View.Remember && record.AutoDone != version {
			return m.reloginLocked(ctx, &record, secret)
		}
		return &Error{Kind: KindAuth, Message: "Sign in to this Sub2API account again"}
	case StateTwoFA, StateChallenge:
		return &Error{Kind: KindAuth, Message: "Sign in to this Sub2API account again"}
	}
	if record.View.RetryAt.After(m.now()) {
		return &Error{Kind: KindTemporary, RetryAfter: record.View.RetryAt.Sub(m.now()), Message: "Sub2API refresh is cooling down"}
	}
	attempt, err := randomID()
	if err != nil {
		return err
	}
	before := record
	record.Pending = &pendingRotation{Version: version, Generation: record.View.Generation, Hash: refreshFingerprint(secret.Tokens.Refresh), Attempt: attempt}
	if err := m.commitRecord(before, record); err != nil {
		return err
	}
	owner, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	raw, err := m.request(owner, secret, http.MethodPost, secret.Base+"/api/v1/auth/refresh", nil,
		map[string]string{"refresh_token": secret.Tokens.Refresh}, "")
	if err != nil {
		if refreshRejected(err) {
			if savedErr := m.saveFailure(&record, StateLogin, err, false); savedErr != err {
				return savedErr
			}
			if record.View.Remember && record.AutoDone != version {
				return m.reloginLocked(owner, &record, secret)
			}
			return err
		}
		var detail *Error
		if errors.As(err, &detail) && (detail.Challenge || detail.Status >= 400 && detail.Status < 500) {
			state := StateTemporary
			if detail.Challenge {
				state = StateChallenge
			}
			return m.saveFailureWithDelay(&record, state, err, false, time.Minute)
		}
		return m.saveFailure(&record, StateUnknown, err, true)
	}
	tokens, err := parseTokens(raw, m.now())
	if err != nil || tokens.TwoFA {
		if err == nil {
			err = &Error{Kind: KindUnknown, Message: "Invalid Sub2API refresh result"}
		}
		return m.saveFailure(&record, StateUnknown, err, true)
	}
	secret.Version, secret.Attempt, secret.Tokens = version+1, attempt, tokens
	record.View.Version, record.View.State, record.View.LastError = secret.Version, StateReady, ""
	record.View.UpdatedAt, record.View.LastSuccess, record.View.RetryAt = m.now(), m.now(), time.Time{}
	record.Pending = nil
	return m.persist(record, secret)
}

func (m *Manager) reloginLocked(ctx context.Context, record *accountRecord, secret sessionSecret) error {
	before := *record
	record.AutoDone = secret.Version
	if err := m.commitRecord(before, *record); err != nil {
		return err
	}
	// A new pair revoked soon after recovery must not start a password loop.
	if !record.AutoRecoveredAt.IsZero() && m.now().Sub(record.AutoRecoveredAt) < 15*time.Minute {
		return &Error{Kind: KindAuth, Message: "Sub2API revoked the recovered session; sign in again"}
	}
	var saved savedPassword
	if err := m.readSecret(m.passwordPath(secret.ID), &saved); err != nil {
		return err
	}
	if saved.ID != secret.ID || saved.Email != secret.Email || saved.Password == "" {
		return storageError()
	}
	if err := m.WithGeneration(secret.ID, secret.Generation, nil); err != nil {
		return err
	}
	raw, err := m.request(ctx, secret, http.MethodPost, secret.Base+"/api/v1/auth/login", nil,
		map[string]string{"email": saved.Email, "password": saved.Password}, "")
	if err != nil {
		state := StateLogin
		var detail *Error
		if errors.As(err, &detail) && detail.Challenge {
			state = StateChallenge
		}
		return m.saveFailure(record, state, err, false)
	}
	tokens, err := parseTokens(raw, m.now())
	if err != nil {
		return m.saveFailure(record, StateLogin, err, false)
	}
	if tokens.TwoFA {
		err = &Error{Kind: KindAuth, Message: "Sub2API requires a new two factor sign-in"}
		return m.saveFailure(record, StateTwoFA, err, false)
	}
	// Validate the stable account before replacing any shared account's credentials.
	if err := m.WithGeneration(secret.ID, secret.Generation, nil); err != nil {
		return err
	}
	me, err := m.request(ctx, secret, http.MethodGet, secret.Base+"/api/v1/auth/me", nil, nil, tokens.Access)
	if err != nil {
		return m.saveFailure(record, StateLogin, err, false)
	}
	var identity struct {
		Data struct {
			ID json.RawMessage `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(me, &identity) != nil {
		return m.saveFailure(record, StateLogin, &Error{Kind: KindUnknown, Message: "Sub2API re-login identity could not be confirmed"}, false)
	}
	subject := strings.Trim(string(identity.Data.ID), "\"")
	if subject != secret.Subject {
		return m.saveFailure(record, StateLogin, &Error{Kind: KindConflict, Message: "Sub2API re-login returned a different account"}, false)
	}
	attempt, err := randomID()
	if err != nil {
		return err
	}
	before = *record
	record.Pending = &pendingRotation{Version: secret.Version, Generation: secret.Generation, Attempt: attempt}
	record.AutoRecoveredAt = m.now()
	if err := m.commitRecord(before, *record); err != nil {
		return err
	}
	secret.Version, secret.Attempt, secret.Tokens = secret.Version+1, attempt, tokens
	record.View.Version, record.View.State, record.View.LastError = secret.Version, StateReady, ""
	record.View.UpdatedAt, record.View.LastSuccess, record.View.RetryAt = m.now(), m.now(), time.Time{}
	record.Pending = nil
	return m.persist(*record, secret)
}

// FetchKey makes a bounded API-key request without storing or mixing panel credentials.
func FetchKey(ctx context.Context, method, rawURL, key string, headers http.Header) ([]byte, error) {
	if strings.TrimSpace(key) == "" {
		return nil, &Error{Kind: KindInvalid, Message: "A Sub2API API key is required"}
	}
	s, effective, err := keyNetworkSettings(ctx, rawURL, headers)
	if err != nil {
		return nil, err
	}
	return New("").request(ctx, s, method, rawURL, effective, nil, key)
}

// KeyNetworkContext uses the same effective settings as the API-key request.
func KeyNetworkContext(ctx context.Context, rawURL string, headers http.Header) (string, error) {
	s, effective, err := keyNetworkSettings(ctx, rawURL, headers)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(struct {
		Proxy, UserAgent string
		Headers          http.Header
	}{s.Proxy, s.UserAgent, effective})
	if err != nil {
		return "", &Error{Kind: KindInvalid, Message: "Invalid API key network settings"}
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func keyNetworkSettings(ctx context.Context, rawURL string, headers http.Header) (sessionSecret, http.Header, error) {
	base, err := NormalizeBase(rawURL)
	if err != nil {
		return sessionSecret{}, nil, err
	}
	if _, err := panelURL(base, rawURL); err != nil {
		return sessionSecret{}, nil, err
	}
	if err := CheckPanelHeaders(headers); err != nil {
		return sessionSecret{}, nil, err
	}
	effective := make(http.Header, len(headers)+2)
	for key, values := range headers {
		canonical := http.CanonicalHeaderKey(key)
		_, duplicate := effective[canonical]
		if !httpguts.ValidHeaderFieldName(key) || duplicate {
			return sessionSecret{}, nil, &Error{Kind: KindInvalid, Message: "Invalid or duplicate API key header"}
		}
		effective[canonical] = make([]string, len(values))
		for i, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return sessionSecret{}, nil, &Error{Kind: KindInvalid, Message: "Invalid API key header value"}
			}
			effective[canonical][i] = value
		}
	}
	userAgent := effective.Get("User-Agent")
	if userAgent == "" {
		userAgent = defaultUserAgent
	}
	effective.Set("Accept", "application/json")
	effective.Set("User-Agent", userAgent)
	proxy, err := effectiveProxy(rawURL, netproxy.Choice(ctx))
	if err != nil {
		return sessionSecret{}, nil, err
	}
	return sessionSecret{Base: base, Proxy: proxy, UserAgent: userAgent}, effective, nil
}
