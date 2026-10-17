package sub2api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type BrowserRequest struct {
	Attempt   string
	Base      string
	Proxy     string
	UserAgent string
}

// BrowserCandidate is an in-memory handoff, never an API response or saved session.
type BrowserCandidate struct {
	Origin     string
	Access     string
	Refresh    string
	UserAgent  string
	ExpiresAt  int64
	HostExited bool
}

type BrowserRead func(context.Context, string, string, http.Header) ([]byte, error)

// BrowserValidator checks a source's actual balance using the final candidate.
type BrowserValidator func(context.Context, AccountView, BrowserRead) error

var authLocks sync.Map

func authBusy() error {
	return &Error{Kind: KindTemporary, Code: "AUTH_BUSY", Message: "Another Sub2API sign-in or refresh owns this site; try again"}
}

func canonicalOrigin(raw string) (string, error) {
	base, err := NormalizeBase(raw)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", &Error{Kind: KindInvalid, Message: "Invalid Sub2API panel origin"}
	}
	host := strings.ToLower(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	}
	port := u.Port()
	if port == "" {
		port = "443"
		if u.Scheme == "http" {
			port = "80"
		}
	}
	number, err := strconv.Atoi(port)
	if err != nil || number <= 0 || number > 65535 {
		return "", &Error{Kind: KindInvalid, Message: "Invalid Sub2API panel port"}
	}
	return u.Scheme + "://" + net.JoinHostPort(host, strconv.Itoa(number)), nil
}

// lockAuth precedes every session lock and also covers an as-yet unknown browser account.
func (m *Manager) lockAuth(base string) (func(), error) {
	origin, err := canonicalOrigin(base)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(origin))
	path, err := filepath.Abs(filepath.Join(m.root, "locks", "auth-"+hex.EncodeToString(sum[:])+".lock"))
	if err != nil {
		return nil, storageError()
	}
	value, _ := authLocks.LoadOrStore(path, new(sync.Mutex))
	lock := value.(*sync.Mutex)
	if !lock.TryLock() {
		return nil, authBusy()
	}
	unlock, held := m.fileLock(path, 0)
	if !held {
		lock.Unlock()
		return nil, authBusy()
	}
	return func() { unlock(); lock.Unlock() }, nil
}

func cancelAttempt(attempt *loginAttempt) {
	attempt.Canceled = true
	attempt.Request.Password, attempt.Temporary = "", ""
	clear(attempt.Password)
	attempt.Password = nil
	if attempt.Cancel != nil {
		attempt.Cancel()
	}
}

func (m *Manager) cancelBrowserTarget(input LoginRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old := m.attempts[m.latest[loginTarget(input)]]; old != nil && old.Request.browser {
		cancelAttempt(old)
	}
}

// BrowserLogin accepts a candidate only after open has confirmed the host exited.
// The opener must return no candidate until its child and profile cleanup have finished.
func (m *Manager) BrowserLogin(ctx context.Context, input LoginRequest, open func(context.Context, BrowserRequest) (BrowserCandidate, error), validate ...BrowserValidator) (LoginResult, error) {
	if open == nil || input.Password != "" || input.Remember {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Browser sign-in does not accept a password or remember option"}
	}
	if len(validate) > 1 || input.SessionID == "" && (len(validate) != 1 || validate[0] == nil) {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Browser sign-in requires verification of the source balance"}
	}
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
	} else if input.ExpectedGeneration != 0 || input.Target == "" || input.Revision == "" {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Browser sign-in requires the current account or source"}
	}
	base, err := NormalizeBase(input.Base)
	if err != nil {
		return LoginResult{}, err
	}
	input.Base, input.Email, input.browser = base, strings.TrimSpace(input.Email), true
	if strings.ContainsAny(input.UserAgent, "\r\n") {
		return LoginResult{}, &Error{Kind: KindInvalid, Message: "Invalid panel user agent"}
	}
	input.Proxy, err = effectiveProxy(base, input.Proxy)
	if err != nil {
		return LoginResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return LoginResult{}, err
	}
	probe, err := m.protect([]byte("Sub2API"))
	protected := len(probe) > 0
	clear(probe)
	if err != nil || !protected {
		return LoginResult{}, storageError()
	}
	m.cancelBrowserTarget(input)
	unlock, err := m.lockAuth(base)
	if err != nil {
		return LoginResult{}, err
	}
	defer unlock()
	if input.SessionID == "" {
		generations, err = m.loginGenerations(base)
		if err != nil {
			return LoginResult{}, err
		}
	} else if err := m.WithGeneration(input.SessionID, input.ExpectedGeneration, nil); err != nil {
		return LoginResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return LoginResult{}, err
	}
	owner, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	attempt := &loginAttempt{Request: input, Generations: generations, Expires: m.now().Add(5 * time.Minute), Busy: true, Cancel: cancel, Context: owner}
	m.mu.Lock()
	m.pruneAttemptsLocked()
	target := loginTarget(input)
	if old := m.attempts[m.latest[target]]; old != nil {
		cancelAttempt(old)
	}
	m.attempts[id], m.latest[target] = attempt, id
	m.mu.Unlock()
	defer m.Cancel(id)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-owner.Done():
				return
			case <-ticker.C:
				if m.checkBrowserAttempt(owner, id) != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-watched }()

	candidate, err := open(owner, BrowserRequest{Attempt: id, Base: base, Proxy: input.Proxy, UserAgent: input.UserAgent})
	if err != nil {
		if owner.Err() != nil {
			return LoginResult{}, &Error{Kind: KindConflict, Code: "BROWSER_CANCELED", Message: "Sub2API browser sign-in was canceled or expired"}
		}
		var detail *Error
		if errors.As(err, &detail) {
			return LoginResult{}, err
		}
		return LoginResult{}, &Error{Kind: KindTemporary, Code: "BROWSER_FAILED", Message: "Sub2API browser sign-in did not complete"}
	}
	if err := m.checkBrowserAttempt(owner, id); err != nil {
		return LoginResult{}, err
	}
	if !candidate.HostExited {
		return LoginResult{}, &Error{Kind: KindConflict, Code: "BROWSER_ACTIVE", Message: "Sub2API browser host has not confirmed its exit"}
	}
	if strings.TrimSpace(candidate.UserAgent) == "" || len(candidate.UserAgent) > 4096 ||
		strings.IndexFunc(candidate.UserAgent, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return LoginResult{}, &Error{Kind: KindInvalid, Code: "BROWSER_CONTEXT", Message: "Sub2API browser did not return a valid user agent"}
	}
	if input.UserAgent != "" && candidate.UserAgent != input.UserAgent {
		return LoginResult{}, &Error{Kind: KindConflict, Code: "BROWSER_CONTEXT", Message: "Sub2API browser used a different user agent"}
	}
	input.UserAgent = candidate.UserAgent
	origin, err := canonicalOrigin(candidate.Origin)
	expectedOrigin, _ := canonicalOrigin(base)
	if err != nil || origin != expectedOrigin {
		return LoginResult{}, &Error{Kind: KindConflict, Code: "BROWSER_ORIGIN", Message: "Sub2API browser returned a different site"}
	}
	if strings.TrimSpace(candidate.Access) == "" || strings.TrimSpace(candidate.Refresh) == "" ||
		len(candidate.Access) > 64<<10 || len(candidate.Refresh) > 64<<10 ||
		strings.ContainsAny(candidate.Access+candidate.Refresh, "\r\n") {
		return LoginResult{}, &Error{Kind: KindInvalid, Code: "BROWSER_TOKENS", Message: "Sub2API browser did not return a complete token pair"}
	}
	tokens := tokenPair{Access: candidate.Access, Refresh: candidate.Refresh, ExpiresAt: candidate.ExpiresAt}
	subject, email, err := m.loginIdentity(owner, input, tokens)
	if err != nil {
		return LoginResult{}, err
	}
	account := accountID(base, subject)
	if input.SessionID != "" && input.SessionID != account {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "Sub2API re-login returned a different account"}
	}
	if err := m.checkBrowserAttempt(owner, id); err != nil {
		return LoginResult{}, err
	}
	// This candidate was never saved. Its refresh is consumed once, then discarded on any failure.
	refresh := tokens.Refresh
	tokens.Refresh, candidate.Refresh = "", ""
	secret := sessionSecret{Base: base, Proxy: input.Proxy, UserAgent: input.UserAgent}
	raw, err := m.request(owner, secret, http.MethodPost, base+"/api/v1/auth/refresh", nil, map[string]string{"refresh_token": refresh}, "")
	refresh = ""
	if err != nil {
		var detail *Error
		if errors.As(err, &detail) && (detail.Challenge || detail.Status >= 400 && detail.Status < 500) {
			return LoginResult{}, err
		}
		return LoginResult{}, &Error{Kind: KindUnknown, Code: "REFRESH_UNCERTAIN", Message: "Browser sign-in refresh is uncertain; start a new sign-in"}
	}
	tokens, err = parseTokens(raw, m.now())
	if err != nil || tokens.TwoFA {
		return LoginResult{}, &Error{Kind: KindUnknown, Code: "REFRESH_UNCERTAIN", Message: "Browser sign-in refresh is uncertain; start a new sign-in"}
	}
	finalSubject, finalEmail, err := m.loginIdentity(owner, input, tokens)
	if err != nil {
		return LoginResult{}, err
	}
	if finalSubject != subject {
		return LoginResult{}, &Error{Kind: KindConflict, Message: "Sub2API browser refresh returned a different account"}
	}
	if err := m.checkBrowserAttempt(owner, id); err != nil {
		return LoginResult{}, err
	}
	if input.SessionID != "" {
		raw, err := m.request(owner, secret, http.MethodGet, base+"/api/v1/groups/available", nil, nil, tokens.Access)
		if err != nil {
			return LoginResult{}, err
		}
		if _, _, err := parseGroups(raw); err != nil {
			return LoginResult{}, &Error{Kind: KindUnknown, Message: "Sub2API account groups could not be confirmed"}
		}
	}
	if err := m.checkBrowserAttempt(owner, id); err != nil {
		return LoginResult{}, err
	}
	if finalEmail != "" {
		email = finalEmail
	}
	if email != "" {
		input.Email = email
	}
	if input.SessionID == "" {
		view := AccountView{ID: account, Base: base, Subject: subject, Email: input.Email,
			UserAgent: input.UserAgent, Context: panelNetworkContext(input.Proxy, input.UserAgent)}
		read := func(ctx context.Context, method, rawURL string, headers http.Header) ([]byte, error) {
			if err := m.checkBrowserAttempt(owner, id); err != nil {
				return nil, err
			}
			return m.request(ctx, secret, method, rawURL, headers, nil, tokens.Access)
		}
		if err := validate[0](owner, view, read); err != nil {
			return LoginResult{}, err
		}
	}
	accountUnlock, err := m.lockAccount(account)
	if err != nil {
		return LoginResult{}, err
	}
	defer accountUnlock()
	if err := m.checkBrowserAttempt(owner, id); err != nil {
		return LoginResult{}, err
	}
	return m.commitLogin(owner, account, subject, id, input, tokens, generations)
}

func (m *Manager) checkBrowserAttempt(ctx context.Context, id string) error {
	m.mu.Lock()
	active := m.attemptActiveLocked(id)
	attempt := m.attempts[id]
	m.mu.Unlock()
	if ctx.Err() != nil || !active {
		return &Error{Kind: KindConflict, Code: "BROWSER_CANCELED", Message: "Sub2API browser sign-in was canceled or expired"}
	}
	return m.withControl(func() error {
		for account, generation := range attempt.Generations {
			record, err := m.record(account)
			if err != nil {
				return err
			}
			if record.View.Generation != generation {
				return staleSession()
			}
		}
		return nil
	})
}
