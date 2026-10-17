package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/netproxy"
)

type panelFixture struct {
	server        *httptest.Server
	logins        atomic.Int64
	refreshes     atomic.Int64
	reads         atomic.Int64
	requireTwoFA  atomic.Bool
	rejectAccess  atomic.Bool
	refreshStatus atomic.Int64
	badPassword   atomic.Bool
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixture(t *testing.T) *panelFixture {
	t.Helper()
	p := new(panelFixture)
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			count := p.logins.Add(1)
			if p.badPassword.Load() {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":"INVALID_CREDENTIALS"}`)
				return
			}
			if p.requireTwoFA.Load() {
				fmt.Fprint(w, `{"code":0,"data":{"requires_2fa":true,"temp_token":"temporary-secret"}}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":"access-%d","refresh_token":"refresh-%d","expires_in":3600}}`, count, count)
		case "/api/v1/auth/login/2fa":
			var request map[string]string
			json.NewDecoder(r.Body).Decode(&request)
			if request["temp_token"] != "temporary-secret" || request["totp_code"] != "123456" {
				t.Error("wrong two factor payload")
			}
			fmt.Fprint(w, `{"data":{"access_token":"access-1","refresh_token":"refresh-1"}}`)
		case "/api/v1/auth/me":
			fmt.Fprint(w, `{"code":0,"data":{"id":42,"email":"account@example.test"}}`)
		case "/api/v1/auth/refresh":
			p.refreshes.Add(1)
			if status := p.refreshStatus.Load(); status != 0 {
				w.WriteHeader(int(status))
				if status == 401 {
					fmt.Fprint(w, `{"code":"REFRESH_TOKEN_REVOKED"}`)
				} else {
					fmt.Fprint(w, `{"code":"TEMPORARY"}`)
				}
				return
			}
			fmt.Fprint(w, `{"data":{"access_token":"rotated-access","refresh_token":"rotated-refresh"}}`)
		case "/data", "/api/v1/user/profile":
			p.reads.Add(1)
			if p.rejectAccess.Load() && r.Header.Get("Authorization") == "Bearer access-1" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"code":401}`)
				return
			}
			fmt.Fprint(w, `{"data":{"balance":12.5}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.server.Close)
	return p
}

func testManager(t *testing.T, root string) *Manager {
	t.Helper()
	m := New(root)
	m.protect = func(plain []byte) ([]byte, error) { return append([]byte("test-envelope:"), plain...), nil }
	m.unprotect = func(cipher []byte) ([]byte, error) {
		if !strings.HasPrefix(string(cipher), "test-envelope:") {
			return nil, errors.New("invalid test envelope")
		}
		return append([]byte(nil), cipher[len("test-envelope:"):]...), nil
	}
	return m
}

func signIn(t *testing.T, m *Manager, p *panelFixture, remember bool) LoginResult {
	t.Helper()
	result, err := m.Login(context.Background(), LoginRequest{
		Base: p.server.URL + "/api/v1", Email: "account@example.test", Password: "password-secret",
		Remember: remember, Proxy: "direct", Target: "source-1", Revision: "revision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestIndependentCancelAroundLoginAcceptance(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("accepted=%t", accepted), func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			first := signIn(t, m, p, true).Account
			p.requireTwoFA.Store(true)
			input := LoginRequest{
				Base: p.server.URL, Email: first.Email, Password: "password-secret",
				Remember: true, Proxy: "direct", Target: "source-1", Revision: "revision-1",
			}
			pending, err := m.Login(t.Context(), input)
			if err != nil || pending.State != StateTwoFA {
				t.Fatalf("initial verification request: %+v %v", pending, err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			releaseIO := sync.OnceFunc(func() { close(release) })
			var gated atomic.Bool
			unprotect := m.unprotect
			m.unprotect = func(cipher []byte) ([]byte, error) {
				if !accepted && strings.Contains(string(cipher), `"tokens"`) && gated.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
				return unprotect(cipher)
			}
			var writes atomic.Int64
			m.write = func(path string, raw []byte) error {
				if accepted && gated.CompareAndSwap(false, true) {
					close(entered)
					<-release
				}
				writes.Add(1)
				return atomicFile(path, raw)
			}
			ctx, cancel := context.WithCancel(t.Context())
			var result LoginResult
			var completeErr error
			done, canceled := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				result, completeErr = m.Complete2FA(ctx, pending.Attempt, "123456")
			}()
			t.Cleanup(func() {
				cancel()
				releaseIO()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("verification outlived its test")
				}
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Error("Cancel outlived its test")
				}
			})
			go func() {
				defer close(canceled)
				select {
				case <-entered:
					m.Cancel(pending.Attempt)
				case <-ctx.Done():
				}
			}()
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("Cancel could not finish while local I/O was blocked")
			}
			if ctx.Err() != nil || writes.Load() != 0 {
				t.Fatalf("cancellation boundary: ctx=%v writes=%d", ctx.Err(), writes.Load())
			}
			if err := m.ValidateAttempt(pending.Attempt, input.Target, input.Revision); err == nil {
				t.Fatal("Cancel retained the verification attempt")
			}
			releaseIO()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("verification did not finish")
			}
			after, readErr := m.Info(first.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if accepted {
				if completeErr != nil || result.Account == nil || result.State != StateReady ||
					after.Generation != first.Generation+1 || after.Version != first.Version+1 {
					t.Fatalf("later cancellation rolled back an accepted login: result=%+v error=%v account=%+v",
						result, completeErr, after)
				}
			} else {
				var detail *Error
				if !errors.As(completeErr, &detail) || detail.Kind != KindConflict || result.Account != nil ||
					after.Generation != first.Generation || after.Version != first.Version || writes.Load() != 0 {
					t.Fatalf("canceled attempt committed credentials: result=%+v error=%v account=%+v writes=%d",
						result, completeErr, after, writes.Load())
				}
			}
		})
	}
}

func TestCanceledSignInDoesNotStoreCredentials(t *testing.T) {
	for _, phase := range []string{"password", "two-factor", "account-lock", "control-lock"} {
		t.Run(phase, func(t *testing.T) {
			p := fixture(t)
			p.requireTwoFA.Store(phase == "two-factor")
			entered, release := make(chan struct{}), make(chan struct{})
			releaseLogin := sync.OnceFunc(func() { close(release) })
			var identityReturned atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "password" && r.URL.Path == "/api/v1/auth/login" ||
					phase == "two-factor" && r.URL.Path == "/api/v1/auth/login/2fa" {
					close(entered)
					<-release
				}
				if r.URL.Path == "/api/v1/auth/me" {
					identityReturned.Store(true)
				}
				p.server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(panel.Close)
			m := testManager(t, t.TempDir())
			id := accountID(panel.URL, "42")
			lockFile := m.fileLock
			m.fileLock = func(path string, wait time.Duration) (func(), bool) {
				unlock, held := lockFile(path, wait)
				if held && identityReturned.Load() && (phase == "account-lock" && path == filepath.Join(m.root, "locks", id+".lock") ||
					phase == "control-lock" && path == filepath.Join(m.root, "locks", "control.lock")) {
					close(entered)
					<-release
				}
				return unlock, held
			}
			input := LoginRequest{Base: panel.URL, Proxy: "direct", Email: "reader@example.test",
				Password: "password-secret", Remember: true, Target: "source-1", Revision: "revision-1"}
			attempt := ""
			if phase == "two-factor" {
				initial, finish := context.WithCancel(t.Context())
				pending, err := m.Login(initial, input)
				finish()
				if err != nil || pending.State != StateTwoFA {
					t.Fatalf("initial verification request: %+v %v", pending, err)
				}
				attempt = pending.Attempt
			}
			ctx, cancel := context.WithCancel(t.Context())
			var result LoginResult
			var loginErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				if attempt != "" {
					result, loginErr = m.Complete2FA(ctx, attempt, "123456")
				} else {
					result, loginErr = m.Login(ctx, input)
				}
			}()
			t.Cleanup(func() {
				cancel()
				releaseLogin()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("canceled sign-in outlived its test")
				}
			})
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("sign-in did not reach its cancellation boundary")
			}
			cancel()
			releaseLogin()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("canceled sign-in did not finish")
			}
			accounts, err := m.Accounts()
			if err != nil || len(accounts) != 0 || loginErr == nil || result.Account != nil {
				t.Fatalf("canceled sign-in saved credentials: result=%+v login=%v accounts=%+v read=%v",
					result, loginErr, accounts, err)
			}
			for _, path := range []string{m.recordPath(id), m.secretPath(id), m.passwordPath(id)} {
				if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("canceled sign-in wrote local credentials: %s: %v", path, err)
				}
			}
			if attempt != "" && m.ValidateAttempt(attempt, input.Target, input.Revision) == nil {
				t.Fatal("canceled verification retained its temporary attempt")
			}
		})
	}
}

func TestTwoFactorSurvivesCompletedRequestCancellation(t *testing.T) {
	p := fixture(t)
	p.requireTwoFA.Store(true)
	m := testManager(t, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	pending, err := m.Login(ctx, LoginRequest{Base: p.server.URL, Proxy: "direct",
		Email: "reader@example.test", Password: "password-secret", Remember: true,
		Target: "source-1", Revision: "revision-1"})
	cancel()
	if err != nil || pending.State != StateTwoFA {
		t.Fatalf("initial verification request: %+v %v", pending, err)
	}
	result, err := m.Complete2FA(t.Context(), pending.Attempt, "123456")
	if err != nil || result.Account == nil || result.State != StateReady || !result.Account.Remember {
		t.Fatalf("completed request canceled a later verification: %+v %v", result, err)
	}
}

func TestSessionIdentityTwoFactorAndCancellation(t *testing.T) {
	p := fixture(t)
	p.requireTwoFA.Store(true)
	m := testManager(t, t.TempDir())
	result := signIn(t, m, p, false)
	if result.State != StateTwoFA || result.Attempt == "" || result.Account != nil {
		t.Fatalf("unsafe or incomplete two factor result: %+v", result)
	}
	if err := m.ValidateAttempt(result.Attempt, "source-1", "revision-2"); err == nil {
		t.Fatal("changed source configuration was accepted")
	}
	m.Cancel(result.Attempt)
	if _, err := m.Complete2FA(context.Background(), result.Attempt, "123456"); err == nil {
		t.Fatal("canceled two factor attempt was accepted")
	}
	result = signIn(t, m, p, false)
	signedIn, err := m.Complete2FA(context.Background(), result.Attempt, "123456")
	if err != nil {
		t.Fatal(err)
	}
	if signedIn.Account.Subject != "42" || signedIn.Account.ID != accountID(p.server.URL, "42") {
		t.Fatalf("identity was not derived from auth/me: %+v", signedIn.Account)
	}
	raw, _ := json.Marshal(signedIn)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "access-1") || strings.Contains(string(raw), "refresh-1") {
		t.Fatalf("login DTO leaked credentials: %s", raw)
	}
	if _, err := os.Stat(m.passwordPath(signedIn.Account.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("password was saved without opt-in: %v", err)
	}
}

func TestExistingAccountLoginWithoutSourceUsesSavedContext(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	input := LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation,
		Email: first.Email, Password: "new-password", Remember: true}
	result, err := m.Login(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID != first.ID || result.Target != "" || result.Revision != "" ||
		result.Account == nil || result.Account.ID != first.ID || result.Account.Generation != first.Generation+1 ||
		result.Account.Version != first.Version+1 || !result.Account.Remember {
		t.Fatalf("account login changed identity or tried to bind a source: %+v", result)
	}
	if err := m.CheckContext(first.ID, "direct", ""); err != nil {
		t.Fatalf("account login lost its saved network context: %v", err)
	}
	if p.logins.Load() != 2 {
		t.Fatalf("account login was not sent once: %d", p.logins.Load())
	}
}

func TestExistingAccountLoginRejectsAnotherIdentity(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	transport := m.transport
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/me" {
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"data":{"id":43}}`)), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
	_, err := m.Login(t.Context(), LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation,
		Email: first.Email, Password: "new-password"})
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != KindConflict {
		t.Fatalf("different account was not rejected: %v", err)
	}
	accounts, err := m.Accounts()
	if err != nil || len(accounts) != 1 || accounts[0].ID != first.ID || accounts[0].Generation != first.Generation {
		t.Fatalf("different identity replaced or created an account: %+v %v", accounts, err)
	}
}

func TestExistingAccountLoginChecksGenerationAndInputModeBeforePassword(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	for _, change := range []string{"stale", "missing", "source", "base", "proxy", "user-agent"} {
		t.Run(change, func(t *testing.T) {
			input := LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation,
				Email: first.Email, Password: "new-password"}
			switch change {
			case "stale":
				input.ExpectedGeneration++
			case "missing":
				input.ExpectedGeneration = 0
			case "source":
				input.Target, input.Revision = "source-1", "revision-1"
			case "base":
				input.Base = p.server.URL
			case "proxy":
				input.Proxy = "direct"
			case "user-agent":
				input.UserAgent = "caller-agent"
			}
			if _, err := m.Login(t.Context(), input); err == nil {
				t.Fatal("invalid account login was accepted")
			}
		})
	}
	if p.logins.Load() != 1 {
		t.Fatalf("invalid account login sent a password: logins=%d", p.logins.Load())
	}
}

func TestExistingAccountTwoFactorRetainsIdentityAndGeneration(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	p.requireTwoFA.Store(true)
	input := LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation,
		Email: first.Email, Password: "new-password"}
	pending, err := m.Login(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if pending.State != StateTwoFA || pending.SessionID != first.ID || pending.Target != "" || pending.Revision != "" {
		t.Fatalf("account mode was lost during two factor sign-in: %+v", pending)
	}
	if err := m.ValidateAccountAttempt(pending.Attempt, first.ID, first.Generation); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateAccountAttempt(pending.Attempt, first.ID, first.Generation+1); err == nil {
		t.Fatal("account verification accepted a different generation")
	}
	if err := m.ValidateAttempt(pending.Attempt, "", ""); err == nil {
		t.Fatal("account verification accepted source mode")
	}
	result, err := m.Complete2FA(t.Context(), pending.Attempt, "123456")
	if err != nil || result.Account == nil || result.Account.ID != first.ID || result.Account.Generation != first.Generation+1 ||
		result.SessionID != first.ID || result.Target != "" {
		t.Fatalf("two factor sign-in did not retain account mode: %+v %v", result, err)
	}
	input.ExpectedGeneration = result.Account.Generation
	pending, err = m.Login(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	other := testManager(t, m.root)
	if _, err := other.LogoutExpected(t.Context(), first.ID, result.Account.Generation); err != nil {
		t.Fatal(err)
	}
	if err := m.ValidateAccountAttempt(pending.Attempt, first.ID, result.Account.Generation); err == nil {
		t.Fatal("account verification survived logout")
	}
	transport := m.transport
	var twoFactorCalls atomic.Int64
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/login/2fa" {
			twoFactorCalls.Add(1)
		}
		return transport.RoundTrip(r)
	})
	if _, err := m.Complete2FA(t.Context(), pending.Attempt, "123456"); err == nil {
		t.Fatal("old account two factor attempt survived logout")
	}
	if twoFactorCalls.Load() != 0 {
		t.Fatal("stale account two factor attempt was sent")
	}
}

func TestExistingAccountLoginAfterLogoutRequiresContext(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	signedOut, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Login(t.Context(), LoginRequest{SessionID: first.ID, ExpectedGeneration: signedOut.Generation,
		Email: first.Email, Password: "new-password"})
	var detail *Error
	if !errors.As(err, &detail) || detail.Code != "ACCOUNT_CONTEXT_REQUIRED" || p.logins.Load() != 1 {
		t.Fatalf("signed-out context was guessed or retained: logins=%d err=%v", p.logins.Load(), err)
	}
}

func TestConcurrentRefreshPersistsOnePairAcrossRestart(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	m := testManager(t, root)
	id := signIn(t, m, p, false).Account.ID
	p.rejectAccess.Store(true)
	var group sync.WaitGroup
	for range 12 {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := m.Read(context.Background(), id, http.MethodGet, p.server.URL+"/data", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if p.refreshes.Load() != 1 {
		t.Fatalf("shared session rotated %d times", p.refreshes.Load())
	}
	restarted := testManager(t, root)
	if _, err := restarted.Read(context.Background(), id, http.MethodGet, "/data", nil); err != nil {
		t.Fatal(err)
	}
	if p.refreshes.Load() != 1 {
		t.Fatal("restart did not use the saved new token pair")
	}
}

func TestNoLockOrPendingWriteMeansNoRefresh(t *testing.T) {
	for _, fail := range []string{"lock", "pending"} {
		t.Run(fail, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			id := signIn(t, m, p, false).Account.ID
			if fail == "lock" {
				m.fileLock = func(string, time.Duration) (func(), bool) { return func() {}, false }
			} else {
				m.write = func(path string, data []byte) error {
					if path == m.recordPath(id) {
						return errors.New("storage full")
					}
					return atomicFile(path, data)
				}
			}
			if err := m.Refresh(context.Background(), id); err == nil {
				t.Fatal("unsafe refresh succeeded")
			}
			if p.refreshes.Load() != 0 {
				t.Fatal("refresh was sent without its lock and durable pending marker")
			}
		})
	}
}

func TestUnknownRefreshIsNeverReplayedAfterRestart(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	m := testManager(t, root)
	id := signIn(t, m, p, true).Account.ID
	p.refreshStatus.Store(503)
	p.rejectAccess.Store(true)
	var detail *Error
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); !errors.As(err, &detail) ||
		detail.Code != "REFRESH_UNCERTAIN" || detail.Kind != KindTemporary || detail.Status != 503 {
		t.Fatalf("uncertain refresh did not retain a distinct classification: %v", err)
	}
	restarted := testManager(t, root)
	if _, err := restarted.Read(context.Background(), id, http.MethodGet, "/data", nil); !errors.As(err, &detail) ||
		detail.Code != "REFRESH_UNCERTAIN" || detail.Kind != KindUnknown {
		t.Fatalf("unknown refresh state lost its classification after restart: %v", err)
	}
	if err := restarted.Refresh(t.Context(), id); !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
		t.Fatalf("explicit refresh lost its uncertain state: %v", err)
	}
	if p.refreshes.Load() != 1 || p.logins.Load() != 1 {
		t.Fatalf("unknown result replayed credentials: refresh=%d login=%d", p.refreshes.Load(), p.logins.Load())
	}
	view, err := restarted.Info(id)
	if err != nil || view.State != StateUnknown {
		t.Fatalf("unknown result was lost: %+v, %v", view, err)
	}
}

func TestUnknownResponseIsNotUncertainRefresh(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, false).Account.ID
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		reader, writer := io.Pipe()
		writer.CloseWithError(io.ErrUnexpectedEOF)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader, Request: r}, nil
	})
	_, err := m.Read(t.Context(), id, http.MethodGet, "/data", nil)
	var detail *Error
	if !errors.As(err, &detail) || detail.Kind != KindUnknown || detail.Code == "REFRESH_UNCERTAIN" {
		t.Fatalf("ordinary response failure was classified as uncertain refresh: %v", err)
	}
	view, err := m.Info(id)
	if err != nil || view.State != StateReady || p.refreshes.Load() != 0 {
		t.Fatalf("ordinary read failure invalidated a session: %+v %v", view, err)
	}
}

func TestNewPairStorageFailureNeverFallsBackToOldPair(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, false).Account.ID
	p.rejectAccess.Store(true)
	m.write = func(path string, data []byte) error {
		if path == m.secretPath(id) {
			return errors.New("storage full")
		}
		return atomicFile(path, data)
	}
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); err == nil {
		t.Fatal("failed token persistence was hidden")
	}
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); err == nil {
		t.Fatal("unsaved pair should wait for local storage")
	}
	if p.refreshes.Load() != 1 {
		t.Fatal("old refresh token replayed while the new pair was unsaved")
	}
	m.write = atomicFile
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); err != nil {
		t.Fatal(err)
	}
	if p.refreshes.Load() != 1 {
		t.Fatal("storage recovery rotated again")
	}
}

func TestSavedCipherRecoversPendingCommit(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	m := testManager(t, root)
	id := signIn(t, m, p, false).Account.ID
	p.rejectAccess.Store(true)
	var writes atomic.Int64
	m.write = func(path string, data []byte) error {
		if path == m.recordPath(id) && writes.Add(1) > 1 {
			return errors.New("metadata commit failed")
		}
		return atomicFile(path, data)
	}
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); err == nil {
		t.Fatal("failed metadata commit was hidden")
	}
	restarted := testManager(t, root)
	if _, err := restarted.Read(context.Background(), id, http.MethodGet, "/data", nil); err != nil {
		t.Fatal(err)
	}
	view, _ := restarted.Info(id)
	if view.State != StateReady || view.Version != 2 || p.refreshes.Load() != 1 {
		t.Fatalf("durable new pair not recovered: %+v refresh=%d", view, p.refreshes.Load())
	}
}

func TestExplicitLoginClearsOldRetryAt(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending=%t", pending), func(t *testing.T) {
			p := fixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			now := time.Now()
			m.now = func() time.Time { return now }
			first := signIn(t, m, p, false).Account
			p.refreshStatus.Store(http.StatusForbidden)
			if err := m.Refresh(t.Context(), first.ID); err == nil {
				t.Fatal("initial refresh failure was hidden")
			}
			old, err := m.record(first.ID)
			if err != nil || !old.View.RetryAt.Equal(now.Add(time.Minute)) {
				t.Fatalf("refresh failure did not save the local cooldown: %+v %v", old.View, err)
			}
			m.write = func(path string, raw []byte) error {
				if pending && path == m.recordPath(first.ID) {
					var record accountRecord
					if err := json.Unmarshal(raw, &record); err != nil {
						return err
					}
					if record.Pending == nil && record.View.Generation > first.Generation {
						return errors.New("final metadata commit failed")
					}
				}
				return atomicFile(path, raw)
			}
			_, err = m.Login(t.Context(), LoginRequest{
				Base: p.server.URL, Email: first.Email, Password: "password-secret",
				Proxy: "direct", Target: "source-1", Revision: "revision-1",
			})
			if pending && err == nil || !pending && err != nil {
				t.Fatalf("explicit login pending=%t: %v", pending, err)
			}
			durable, err := m.record(first.ID)
			if err != nil || (durable.Pending != nil) != pending {
				t.Fatalf("unexpected durable login state: %+v %v", durable, err)
			}

			// Restart without the unsaved in-memory record; only the new access token is expired.
			restarted := testManager(t, root)
			restarted.now = func() time.Time { return now }
			restarted.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/data" && r.Header.Get("Authorization") == "Bearer access-2" {
					return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{"code":401}`)), Request: r}, nil
				}
				return http.DefaultTransport.RoundTrip(r)
			})
			p.refreshStatus.Store(0)
			raw, readErr := restarted.Read(t.Context(), first.ID, http.MethodGet, "/data", nil)
			if readErr != nil || !strings.Contains(string(raw), `"balance":12.5`) {
				t.Errorf("new login retained the old refresh cooldown: %v", readErr)
			}
			recovered, err := restarted.record(first.ID)
			if err != nil || recovered.View.Generation != first.Generation+1 ||
				recovered.View.State != StateReady || recovered.Pending != nil || !recovered.View.RetryAt.IsZero() {
				t.Errorf("new login did not restore successful-login state: %+v %v", recovered.View, err)
			}
			if got := p.refreshes.Load(); got != 2 {
				t.Errorf("new session refresh requests = %d, want 2", got)
			}
		})
	}
}

func TestOnlyDefinitiveRefreshRejectionUsesRememberedPassword(t *testing.T) {
	for _, status := range []int{401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			id := signIn(t, m, p, true).Account.ID
			p.refreshStatus.Store(int64(status))
			p.rejectAccess.Store(true)
			_, _ = m.Read(context.Background(), id, http.MethodGet, "/data", nil)
			want := int64(1)
			if status == 401 {
				want = 2
			}
			if p.logins.Load() != want {
				t.Fatalf("HTTP %d caused %d password logins; want %d", status, p.logins.Load(), want)
			}
			view, err := m.Info(id)
			if err != nil || view.Generation != 1 {
				t.Fatalf("automatic recovery changed the lifecycle: %+v %v", view, err)
			}
		})
	}
}

func TestPasswordFailureStopsAutomaticLoginForVersion(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, true).Account.ID
	p.refreshStatus.Store(401)
	p.rejectAccess.Store(true)
	p.badPassword.Store(true)
	for range 3 {
		_, _ = m.Read(context.Background(), id, http.MethodGet, "/data", nil)
	}
	if p.refreshes.Load() != 1 || p.logins.Load() != 2 {
		t.Fatalf("failed password retried: login=%d refresh=%d", p.logins.Load(), p.refreshes.Load())
	}
}

func TestRecoveredSessionRevocationStaysBlockedAcrossRestart(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Minute, -time.Hour} {
		t.Run(elapsed.String(), func(t *testing.T) {
			p := fixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			now := time.Now()
			m.now = func() time.Time { return now }
			id := signIn(t, m, p, true).Account.ID
			p.refreshStatus.Store(401)
			if err := m.Refresh(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			recoveredAt := now
			now = now.Add(elapsed)
			if err := m.Refresh(context.Background(), id); err == nil {
				t.Fatal("repeated revocation did not require an explicit login")
			}
			record, err := m.record(id)
			if err != nil || record.View.State != StateLogin || record.AutoDone != record.View.Version {
				t.Fatalf("blocked version was not durable: %+v %v", record, err)
			}
			restarted := testManager(t, root)
			restarted.now = func() time.Time { return recoveredAt.Add(time.Hour) }
			if err := restarted.Refresh(context.Background(), id); err == nil {
				t.Fatal("waiting and restarting reset a blocked automatic login")
			}
			if p.logins.Load() != 2 || p.refreshes.Load() != 2 {
				t.Fatalf("blocked version retried: login=%d refresh=%d", p.logins.Load(), p.refreshes.Load())
			}
			signIn(t, restarted, p, true)
			record, err = restarted.record(id)
			if err != nil || record.AutoDone != 0 || !record.AutoRecoveredAt.IsZero() {
				t.Fatalf("explicit login did not clear recovery protection: %+v %v", record, err)
			}
			if err := restarted.Refresh(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if p.logins.Load() != 4 {
				t.Fatalf("explicit login did not permit recovery: login=%d", p.logins.Load())
			}
		})
	}
}

func TestLaterVersionCanRecoverOutsideRepeatedRevocationWindow(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	now := time.Now()
	m.now = func() time.Time { return now }
	id := signIn(t, m, p, true).Account.ID
	p.refreshStatus.Store(401)
	if err := m.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	if err := m.Refresh(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if p.logins.Load() != 3 {
		t.Fatalf("a later version outside the window did not recover: login=%d", p.logins.Load())
	}
}

func TestPostRenewsWithoutReplayAndLogoutDeletesSecrets(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, true).Account.ID
	p.rejectAccess.Store(true)
	before := p.reads.Load()
	raw, err := m.Read(context.Background(), id, http.MethodPost, "/data", nil)
	var detail *Error
	if raw != nil || !errors.As(err, &detail) || detail.Kind != "renewed" || accessRejected(err) {
		t.Fatalf("POST renewal needs a separate query, got body=%q err=%+v", raw, err)
	}
	if p.reads.Load() != before+1 || p.refreshes.Load() != 1 {
		t.Fatalf("POST was replayed or not renewed: reads=%d refreshes=%d", p.reads.Load(), p.refreshes.Load())
	}
	if raw, err := m.Read(context.Background(), id, http.MethodPost, "/data", nil); err != nil || !strings.Contains(string(raw), `"balance":12.5`) {
		t.Fatalf("the next explicit POST did not use the renewed session: body=%q err=%v", raw, err)
	}
	if err := m.Logout(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{m.secretPath(id), m.passwordPath(id)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("logout retained %s: %v", filepath.Base(path), err)
		}
	}
	if _, err := m.Read(context.Background(), id, http.MethodGet, "/data", nil); err == nil {
		t.Fatal("logged-out session was accepted")
	}
	if p.refreshes.Load() != 1 || p.logins.Load() != 1 {
		t.Fatal("logout automatically restored a credential")
	}
}

func TestLatePostRejectionUsesFailedCredentialVersion(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	first, late := testManager(t, root), testManager(t, root)
	id := signIn(t, first, p, false).Account.ID
	p.rejectAccess.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	ready := make(chan struct{}, 2)
	results := make(chan error, 2)
	releaseFirst, releaseLate := make(chan struct{}), make(chan struct{})
	var group sync.WaitGroup
	defer func() {
		cancel()
		group.Wait()
	}()
	for i, owner := range []*Manager{first, late} {
		transport := owner.transport
		release := []chan struct{}{releaseFirst, releaseLate}[i]
		owner.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/data" && r.Header.Get("Authorization") == "Bearer access-1" {
				ready <- struct{}{}
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			}
			return transport.RoundTrip(r)
		})
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := owner.Read(ctx, id, http.MethodPost, "/data", nil)
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-ready:
		case <-ctx.Done():
			t.Fatal("both POSTs did not start with the old access token")
		}
	}
	for _, release := range []chan struct{}{releaseFirst, releaseLate} {
		close(release)
		select {
		case err := <-results:
			var detail *Error
			if !errors.As(err, &detail) || detail.Kind != "renewed" {
				t.Fatalf("old POST rejection did not return a renewal result: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("POST renewal did not complete")
		}
	}
	if p.refreshes.Load() != 1 || p.reads.Load() != 2 {
		t.Fatalf("late rejection consumed the new refresh or replayed a POST: refreshes=%d reads=%d", p.refreshes.Load(), p.reads.Load())
	}
	if _, err := late.Read(ctx, id, http.MethodPost, "/data", nil); err != nil {
		t.Fatalf("the next explicit POST did not reuse the saved new pair: %v", err)
	}
	if p.refreshes.Load() != 1 || p.reads.Load() != 3 {
		t.Fatalf("the next explicit POST rotated or replayed: refreshes=%d reads=%d", p.refreshes.Load(), p.reads.Load())
	}
}

func TestPostDoesNotRenewWithoutExplicitAccessRejection(t *testing.T) {
	for _, c := range []struct {
		name   string
		status int
		err    error
		raw    bool
	}{
		{name: "permission", status: http.StatusForbidden},
		{name: "timeout", err: context.DeadlineExceeded},
		{name: "raw", status: http.StatusUnauthorized, raw: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			id := signIn(t, m, p, false).Account.ID
			transport := m.transport
			var requests atomic.Int64
			m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/data" {
					return transport.RoundTrip(r)
				}
				requests.Add(1)
				if c.err != nil {
					return nil, c.err
				}
				return &http.Response{StatusCode: c.status, Header: make(http.Header),
					Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
			})
			read := m.Read
			if c.raw {
				read = m.ReadRaw
			}
			if _, err := read(context.Background(), id, http.MethodPost, "/data", nil); err == nil {
				t.Fatal("failed POST was accepted")
			}
			if requests.Load() != 1 || p.refreshes.Load() != 0 || p.logins.Load() != 1 {
				t.Fatalf("POST without an explicit access rejection renewed or replayed: requests=%d refreshes=%d logins=%d",
					requests.Load(), p.refreshes.Load(), p.logins.Load())
			}
		})
	}
}

func TestOriginAndIdentityHeadersAreBoundaries(t *testing.T) {
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { foreignCalls.Add(1) }))
	defer foreign.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"/stolen", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	m := testManager(t, t.TempDir())
	s := sessionSecret{Base: server.URL}
	if _, err := m.request(context.Background(), s, http.MethodGet, foreign.URL, nil, nil, "panel-secret"); err == nil {
		t.Fatal("cross-origin direct request accepted")
	}
	if _, err := m.request(context.Background(), s, http.MethodPost, server.URL, nil, map[string]string{"password": "secret"}, "panel-secret"); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	for _, header := range []string{"Cookie", "Authorization", "Proxy-Authorization", "authorization", "cookie"} {
		headers := http.Header{header: []string{"conflicting-secret"}}
		if _, err := m.request(context.Background(), s, http.MethodGet, server.URL, headers, nil, "panel-secret"); err == nil {
			t.Fatalf("conflicting %s accepted", header)
		}
	}
	if foreignCalls.Load() != 0 {
		t.Fatal("secret-bearing request reached a foreign origin")
	}
}

func TestAnotherProcessLogoutInvalidatesInFlightLogin(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	m := testManager(t, root)
	id := signIn(t, m, p, false).Account.ID
	other := testManager(t, root)
	entered, release := make(chan struct{}), make(chan struct{})
	base := other.transport
	other.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/login" {
			close(entered)
			<-release
		}
		return base.RoundTrip(r)
	})
	done := make(chan error, 1)
	go func() {
		_, err := other.Login(context.Background(), LoginRequest{
			Base: p.server.URL, Email: "account@example.test", Password: "password-secret",
			Proxy: "direct", Target: "source-1", Revision: "revision-1",
		})
		done <- err
	}()
	<-entered
	if err := m.Logout(context.Background(), id); err != nil {
		close(release)
		<-done
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("in-flight login restored a session after logout in another process")
	}
	view, err := m.Info(id)
	if err != nil || view.State != StateLoggedOut {
		t.Fatalf("logout state was overwritten: %+v %v", view, err)
	}
}

func TestActiveLoginBlocksOrdinaryRefresh(t *testing.T) {
	for _, accountMode := range []bool{false, true} {
		t.Run(fmt.Sprint(accountMode), func(t *testing.T) {
			testActiveLoginBlocksOrdinaryRefresh(t, accountMode)
		})
	}
}

func testActiveLoginBlocksOrdinaryRefresh(t *testing.T, accountMode bool) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	other := testManager(t, m.root)
	entered, release := make(chan struct{}), make(chan struct{})
	releaseLogin := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseLogin)
	base := other.transport
	other.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/auth/login" {
			close(entered)
			<-release
		}
		return base.RoundTrip(r)
	})
	type loginOutcome struct {
		result LoginResult
		err    error
	}
	done := make(chan loginOutcome, 1)
	go func() {
		input := LoginRequest{
			Base: p.server.URL, Email: first.Email, Password: "password-secret",
			Proxy: "direct", Target: "source-1", Revision: "revision-1",
		}
		if accountMode {
			input = LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation,
				Email: first.Email, Password: "password-secret"}
		}
		result, err := other.Login(t.Context(), input)
		done <- loginOutcome{result, err}
	}()
	<-entered
	var detail *Error
	if err := m.Refresh(t.Context(), first.ID); !errors.As(err, &detail) || detail.Code != "AUTH_BUSY" {
		releaseLogin()
		<-done
		t.Fatalf("refresh competed with active sign-in: %v", err)
	}
	view, err := m.Info(first.ID)
	if err != nil || view.Generation != first.Generation || view.Version != first.Version ||
		view.State != StateReady || p.refreshes.Load() != 0 {
		releaseLogin()
		<-done
		t.Fatalf("busy refresh changed the account: %+v %v", view, err)
	}
	releaseLogin()
	outcome := <-done
	if outcome.err != nil {
		t.Fatalf("busy refresh invalidated an active login: %v", outcome.err)
	}
	if outcome.result.Account.Generation != first.Generation+1 || outcome.result.Account.Version != first.Version+1 {
		t.Fatalf("active login did not commit a new lifecycle: %+v", outcome.result.Account)
	}
}

func waitSessionCleanups(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		m.mu.Lock()
		pending := len(m.cleanups)
		m.mu.Unlock()
		if pending == 0 {
			return
		}
		select {
		case <-deadline:
			t.Fatal("session cleanup did not finish")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestLogoutInvalidatesRefreshBeforeNetworkOwnerFinishes(t *testing.T) {
	for _, otherManager := range []bool{false, true} {
		for _, status := range []int64{0, 401, 503} {
			t.Run(fmt.Sprintf("other=%t/status=%d", otherManager, status), func(t *testing.T) {
				p := fixture(t)
				m := testManager(t, t.TempDir())
				first := signIn(t, m, p, true).Account
				logout := m
				if otherManager {
					logout = testManager(t, m.root)
				}
				p.refreshStatus.Store(status)
				entered, release := make(chan struct{}), make(chan struct{})
				releaseRefresh := sync.OnceFunc(func() { close(release) })
				t.Cleanup(releaseRefresh)
				base := m.transport
				m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.URL.Path == "/api/v1/auth/refresh" {
						close(entered)
						<-release
					}
					return base.RoundTrip(r)
				})
				done := make(chan error, 1)
				go func() { done <- m.Refresh(t.Context(), first.ID) }()
				<-entered
				type logoutOutcome struct {
					result LogoutResult
					err    error
				}
				signedOut := make(chan logoutOutcome, 1)
				go func() {
					result, err := logout.LogoutExpected(t.Context(), first.ID, first.Generation)
					signedOut <- logoutOutcome{result, err}
				}()
				var outcome logoutOutcome
				select {
				case outcome = <-signedOut:
				case <-time.After(time.Second):
					releaseRefresh()
					<-done
					<-signedOut
					waitSessionCleanups(t, logout)
					t.Fatal("logical logout waited for the network owner")
				}
				if outcome.err != nil || !outcome.result.CleanupPending || outcome.result.Generation != first.Generation+1 {
					releaseRefresh()
					<-done
					waitSessionCleanups(t, logout)
					t.Fatalf("logout was not immediately durable: %+v %v", outcome.result, outcome.err)
				}
				durable, err := m.record(first.ID)
				if err != nil || durable.View.State != StateLoggedOut || durable.Pending != nil {
					releaseRefresh()
					<-done
					waitSessionCleanups(t, logout)
					t.Fatalf("old work was not invalidated: %+v %v", durable, err)
				}
				releaseRefresh()
				var detail *Error
				if err := <-done; !errors.As(err, &detail) || detail.Kind != KindConflict {
					waitSessionCleanups(t, logout)
					t.Fatalf("late refresh did not reject its write: %v", err)
				}
				waitSessionCleanups(t, logout)
				current, err := m.record(first.ID)
				if err != nil || current.View.State != StateLoggedOut || current.View.Version != first.Version ||
					current.View.Generation != first.Generation+1 || current.Pending != nil || current.View.CleanupPending {
					t.Fatalf("late refresh replaced the logout tombstone: %+v %v", current, err)
				}
				if p.logins.Load() != 1 {
					t.Fatal("late rejected refresh started password recovery")
				}
				for _, path := range []string{m.secretPath(first.ID), m.passwordPath(first.ID)} {
					if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("cleanup retained a secret: %v", err)
					}
				}
			})
		}
	}
}

func TestOldCleanupAndLogoutCannotDeleteNewLogin(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	result, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	second := signIn(t, m, p, true).Account
	if second.Generation != result.Generation+1 {
		t.Fatal("login reused the old lifecycle after a tombstone")
	}
	unlock, err := m.lockAccount(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.cleanupLocked(first.ID, result.Generation)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.LogoutExpected(t.Context(), first.ID, first.Generation); err == nil {
		t.Fatal("an old logout affected the new login")
	}
	if _, _, err := m.load(first.ID); err != nil {
		t.Fatalf("old cleanup removed the new login: %v", err)
	}
	called := false
	if err := m.WithGeneration(first.ID, first.Generation, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("an old lifecycle reached a guarded commit")
	}
	if err := m.WithGeneration(first.ID, second.Generation, func() error { called = true; return nil }); err != nil || !called {
		t.Fatalf("the current lifecycle could not commit: %v", err)
	}
	if err := atomicFile(m.recordPath(first.ID), []byte("{broken")); err != nil {
		t.Fatal(err)
	}
	called = false
	if err := m.WithGeneration(first.ID, second.Generation, func() error { called = true; return nil }); err == nil || called {
		t.Fatal("an unreadable lifecycle reached a guarded commit")
	}
}

func TestRepeatedLogoutInvalidatesNewSourceLogin(t *testing.T) {
	for _, browser := range []bool{false, true} {
		for _, otherManager := range []bool{false, true} {
			t.Run(fmt.Sprintf("browser=%t/other=%t", browser, otherManager), func(t *testing.T) {
				p := fixture(t)
				m := testManager(t, t.TempDir())
				first := signIn(t, m, p, false).Account
				out, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
				if err != nil {
					t.Fatal(err)
				}
				logout := m
				if otherManager {
					logout = testManager(t, m.root)
				}
				entered, release := make(chan struct{}), make(chan struct{})
				releaseLogin := sync.OnceFunc(func() { close(release) })
				defer releaseLogin()
				transport := m.transport
				if !browser {
					m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.URL.Path == "/api/v1/auth/login" {
							close(entered)
							<-release
						}
						return transport.RoundTrip(r)
					})
				}
				done := make(chan error, 1)
				go func() {
					if browser {
						_, err := m.BrowserLogin(t.Context(), browserInput(p),
							func(ctx context.Context, request BrowserRequest) (BrowserCandidate, error) {
								close(entered)
								select {
								case <-ctx.Done():
									return BrowserCandidate{}, ctx.Err()
								case <-release:
									return browserCandidate(request), nil
								}
							}, browserBalance)
						done <- err
					} else {
						_, err := m.Login(t.Context(), LoginRequest{
							Base: p.server.URL, Proxy: "direct", Email: first.Email, Password: "password-secret",
							Target: "source-1", Revision: "revision-2",
						})
						done <- err
					}
				}()
				<-entered
				again, err := logout.LogoutExpected(t.Context(), first.ID, out.Generation)
				releaseLogin()
				loginErr := <-done
				waitSessionCleanups(t, logout)
				if err != nil {
					t.Fatal(err)
				}
				view, readErr := m.Info(first.ID)
				if again.Generation != out.Generation+1 || loginErr == nil || readErr != nil ||
					view.Generation != again.Generation || view.State != StateLoggedOut || view.CleanupPending {
					t.Fatalf("late login revived an explicitly exited account: result=%+v login=%v account=%+v read=%v",
						again, loginErr, view, readErr)
				}
			})
		}
	}
}

func TestRepeatedLogoutKeepsQueuedCredentialCleanup(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	unlock, err := m.lockAccount(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	releaseOwner := sync.OnceFunc(unlock)
	defer releaseOwner()
	out, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	if err != nil {
		t.Fatal(err)
	}
	again, err := m.LogoutExpected(t.Context(), first.ID, out.Generation)
	releaseOwner()
	waitSessionCleanups(t, m)
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.record(first.ID)
	if err != nil || again.Generation != out.Generation+1 || record.View.Generation != again.Generation ||
		record.View.State != StateLoggedOut || record.View.CleanupPending {
		t.Fatalf("repeated logout lost its queued cleanup: result=%+v record=%+v read=%v", again, record, err)
	}
	for _, path := range []string{m.secretPath(first.ID), m.passwordPath(first.ID)} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("queued cleanup retained an exited credential: %v", err)
		}
	}
}

func TestLogoutDuringCleanupExitKeepsNewCleanup(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	exiting, release := make(chan struct{}), make(chan struct{})
	releaseCleanup := sync.OnceFunc(func() { close(release) })
	defer releaseCleanup()
	var acquired atomic.Int64
	lockFile := m.fileLock
	m.fileLock = func(path string, wait time.Duration) (func(), bool) {
		unlock, held := lockFile(path, wait)
		if !held || path != filepath.Join(m.root, "locks", first.ID+".lock") || acquired.Add(1) != 2 {
			return unlock, held
		}
		return func() {
			unlock()
			close(exiting)
			<-release
		}, true
	}
	unlock, err := m.lockAccount(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	out, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	<-exiting
	again, err := m.LogoutExpected(t.Context(), first.ID, out.Generation)
	releaseCleanup()
	waitSessionCleanups(t, m)
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.record(first.ID)
	if err != nil || record.View.Generation != again.Generation || record.View.State != StateLoggedOut || record.View.CleanupPending {
		t.Fatalf("exiting cleanup lost the newer logout: result=%+v record=%+v read=%v", again, record, err)
	}
}

func TestLogoutRejectsUnsavedPairAndRestartRecovery(t *testing.T) {
	for _, fail := range []string{"secret", "metadata"} {
		t.Run(fail, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			first := signIn(t, m, p, false).Account
			var writes atomic.Int64
			m.write = func(path string, raw []byte) error {
				if fail == "secret" && path == m.secretPath(first.ID) ||
					fail == "metadata" && path == m.recordPath(first.ID) && writes.Add(1) > 1 {
					return errors.New("local storage full")
				}
				return atomicFile(path, raw)
			}
			if err := m.Refresh(t.Context(), first.ID); err == nil {
				t.Fatal("storage failure was hidden")
			}
			m.write = atomicFile
			if _, err := m.LogoutExpected(t.Context(), first.ID, first.Generation); err != nil {
				t.Fatal(err)
			}
			for _, reader := range []*Manager{m, testManager(t, m.root)} {
				if _, _, err := reader.load(first.ID); err == nil {
					t.Fatal("old unsaved or durable pair restored a signed-out account")
				}
			}
			view, err := m.Info(first.ID)
			if err != nil || view.State != StateLoggedOut || view.Generation != first.Generation+1 {
				t.Fatalf("old recovery replaced logout: %+v %v", view, err)
			}
		})
	}
}

func TestLogoutInvalidatesIncompleteActiveLogin(t *testing.T) {
	for _, previouslyLoggedOut := range []bool{false, true} {
		for _, fail := range []string{"secret", "metadata"} {
			t.Run(fmt.Sprintf("loggedOut=%t/fail=%s", previouslyLoggedOut, fail), func(t *testing.T) {
				p := fixture(t)
				m := testManager(t, t.TempDir())
				first := signIn(t, m, p, false).Account
				generation := first.Generation
				if previouslyLoggedOut {
					result, err := m.LogoutExpected(t.Context(), first.ID, generation)
					if err != nil {
						t.Fatal(err)
					}
					generation = result.Generation
				}
				var writes atomic.Int64
				m.write = func(path string, raw []byte) error {
					if fail == "secret" && path == m.secretPath(first.ID) ||
						fail == "metadata" && path == m.recordPath(first.ID) && writes.Add(1) > 1 {
						return errors.New("local storage full")
					}
					return atomicFile(path, raw)
				}
				if _, err := m.Login(t.Context(), LoginRequest{
					Base: p.server.URL, Email: first.Email, Password: "password-secret",
					Proxy: "direct", Target: "source-1", Revision: "revision-1",
				}); err == nil {
					t.Fatal("an incomplete active login reported success")
				}
				m.write = atomicFile
				view, err := m.Info(first.ID)
				if err != nil || view.Generation != generation {
					t.Fatalf("uncommitted login exposed a new lifecycle: %+v %v", view, err)
				}
				result, err := m.LogoutExpected(t.Context(), first.ID, generation)
				if err != nil || result.Generation != generation+1 {
					t.Fatalf("logout did not invalidate an incomplete login: %+v %v", result, err)
				}
				for _, reader := range []*Manager{m, testManager(t, m.root)} {
					if _, _, err := reader.load(first.ID); err == nil {
						t.Fatal("incomplete active login restored the exited lifecycle")
					}
				}
			})
		}
	}
}

func TestProtocolClassificationDoesNotLeakVendorSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("cf-mitigated", "challenge")
		w.WriteHeader(403)
		fmt.Fprint(w, `{"code":"ACCESS_DENIED","message":"password-secret token-secret"}`)
	}))
	defer server.Close()
	m := testManager(t, t.TempDir())
	_, err := m.request(context.Background(), sessionSecret{Base: server.URL}, http.MethodGet, server.URL, nil, nil, "token-secret")
	var detail *Error
	if !errors.As(err, &detail) || !detail.Challenge || detail.Kind != KindChallenge || detail.RetryAfter != 2*time.Minute || detail.Status != 403 || detail.Code != "ACCESS_DENIED" {
		t.Fatalf("classification was discarded: %#v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("vendor error leaked a credential")
	}
}

func TestNormalizeBaseRejectsCredentialAndRemoteHTTP(t *testing.T) {
	for _, input := range []string{"http://example.test", "https://user:pass@example.test", "https://example.test?q=secret", "https://example.test?", "https://example.test/#part", "file:///tmp/secret"} {
		if _, err := NormalizeBase(input); err == nil {
			t.Fatalf("unsafe base accepted: %s", input)
		}
	}
	base, err := NormalizeBase("https://EXAMPLE.test/panel/api/v1/")
	if err != nil || base != "https://example.test/panel" {
		t.Fatalf("base not normalized: %s %v", base, err)
	}
}

func TestSessionNetworkContextComparesSecretProxy(t *testing.T) {
	m := testManager(t, t.TempDir())
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		raw := `{"data":{"id":"account-id"}}`
		if r.URL.Path == "/api/v1/auth/login" {
			raw = `{"data":{"access_token":"access-secret","refresh_token":"refresh-secret"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
	})
	input := LoginRequest{
		Base: "https://panel.example.test", Email: "account@example.test", Password: "password-secret",
		Proxy: "http://user:first-secret@proxy.example.test:8080", Target: "source-1", Revision: "revision-1",
	}
	result, err := m.Login(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	id := result.Account.ID
	if strings.Contains(result.Account.Proxy, "user") || strings.Contains(result.Account.Proxy, "secret") {
		t.Fatalf("proxy credentials leaked in DTO: %s", result.Account.Proxy)
	}
	if err := m.CheckContext(id, input.Proxy, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckContext(id, "http://user:second-secret@proxy.example.test:8080", ""); err == nil {
		t.Fatal("different proxy credentials matched through their redacted address")
	}
	if err := m.CheckContext(id, input.Proxy, "different-user-agent"); err == nil {
		t.Fatal("different user agent matched")
	}
	fingerprint, err := m.NetworkContext(id)
	if err != nil || strings.Contains(fingerprint, "secret") || len(fingerprint) != 64 {
		t.Fatalf("network fingerprint is unsafe or missing: %q %v", fingerprint, err)
	}
	if result.Account.Context != fingerprint {
		t.Fatal("account metadata did not preserve the panel network context")
	}
	restarted := testManager(t, m.root)
	restarted.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if netproxy.Choice(r.Context()) != input.Proxy || r.Header.Get("User-Agent") != defaultUserAgent {
			t.Error("account sign-in did not use its saved, unredacted proxy and user agent")
		}
		return m.transport.RoundTrip(r)
	})
	if _, err := restarted.Login(t.Context(), LoginRequest{SessionID: id, ExpectedGeneration: result.Account.Generation,
		Email: input.Email, Password: "new-password"}); err != nil {
		t.Fatal(err)
	}
	unlock, err := m.lockAccount(id)
	if err != nil {
		t.Fatal(err)
	}
	record, secret, err := m.load(id)
	if err == nil {
		secret.Version++
		record.View.Version = secret.Version
		err = m.persist(record, secret)
	}
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := m.NetworkContext(id)
	if err != nil || rotated != fingerprint {
		t.Fatalf("token version changed network identity: %q %v", rotated, err)
	}
	view, err := restarted.Info(id)
	if err != nil || view.Context != fingerprint {
		t.Fatalf("persisted panel context was lost across login or refresh: %+v %v", view, err)
	}
	for _, change := range []string{"proxy-password", "user-agent"} {
		unlock, err := m.lockAccount(id)
		if err != nil {
			t.Fatal(err)
		}
		record, secret, err := m.load(id)
		if err == nil {
			if change == "proxy-password" {
				secret.Proxy = "http://user:second-secret@proxy.example.test:8080"
			} else {
				secret.UserAgent = "changed-user-agent"
			}
			err = m.persist(record, secret)
		}
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		changed, err := m.NetworkContext(id)
		if err != nil || changed == fingerprint {
			t.Fatalf("%s did not change network identity: %q %v", change, changed, err)
		}
		fingerprint = changed
	}
	raw, err := os.ReadFile(m.recordPath(id))
	if err != nil || strings.Contains(string(raw), "secret") {
		t.Fatalf("metadata leaked secrets: %s %v", raw, err)
	}
}

func TestFetchKeyPreservesEffectiveUserAgent(t *testing.T) {
	for _, userAgent := range []string{"provider-key-agent", ""} {
		t.Run(userAgent, func(t *testing.T) {
			var got string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header.Get("User-Agent")
				if r.Header.Get("Authorization") != "Bearer key-secret" || r.Header.Get("Accept") != "application/json" {
					t.Error("key request did not use the final key headers")
				}
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			headers := http.Header{"User-Agent": {userAgent}, "Accept": {"text/plain"}}
			if _, err := FetchKey(netproxy.With(t.Context(), "direct"), http.MethodGet, server.URL, "key-secret", headers); err != nil {
				t.Fatal(err)
			}
			expected := userAgent
			if expected == "" {
				expected = defaultUserAgent
			}
			if got != expected {
				t.Fatalf("key request user agent = %q, want %q", got, expected)
			}
		})
	}
}

func TestKeyNetworkContextTracksEffectiveSettingsAndRejectsIdentityHeaders(t *testing.T) {
	endpoint := "https://panel.example.test/v1/sub2api/billing"
	headers := http.Header{"User-Agent": {"provider-key-agent"}, "X-Region": {"region-1"}, "Accept": {"text/plain"}}
	ctx := netproxy.With(t.Context(), "http://user:proxy-secret@proxy.example.test:8080")
	first, err := KeyNetworkContext(ctx, endpoint, headers)
	if err != nil || len(first) != 64 || strings.Contains(first, "secret") {
		t.Fatalf("unsafe or missing API key network context: %q %v", first, err)
	}
	same := http.Header{"user-agent": {"provider-key-agent"}, "x-region": {"region-1"}, "accept": {"overridden"}}
	if got, err := KeyNetworkContext(ctx, endpoint, same); err != nil || got != first {
		t.Fatalf("header spelling or overridden Accept changed effective context: %q %v", got, err)
	}
	for _, change := range []string{"proxy-password", "user-agent", "header"} {
		t.Run(change, func(t *testing.T) {
			changedCtx, changedHeaders := ctx, headers.Clone()
			switch change {
			case "proxy-password":
				changedCtx = netproxy.With(t.Context(), "http://user:other-secret@proxy.example.test:8080")
			case "user-agent":
				changedHeaders.Set("User-Agent", "changed-agent")
			case "header":
				changedHeaders.Set("X-Region", "region-2")
			}
			if got, err := KeyNetworkContext(changedCtx, endpoint, changedHeaders); err != nil || got == first {
				t.Fatalf("%s did not change effective key context: %q %v", change, got, err)
			}
		})
	}
	direct := netproxy.With(t.Context(), "direct")
	absent, err := KeyNetworkContext(direct, endpoint, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := KeyNetworkContext(direct, endpoint, http.Header{"User-Agent": {defaultUserAgent}}); err != nil || got != absent {
		t.Fatalf("default user agent changed effective key context: %q %v", got, err)
	}
	for _, forbidden := range []string{"authorization", "cookie", "proxy-authorization", "Bad\r\nName", "X-Unsafe"} {
		t.Run(forbidden, func(t *testing.T) {
			invalid := http.Header{forbidden: {"identity-secret"}}
			if forbidden == "X-Unsafe" {
				invalid[forbidden] = []string{"unsafe\r\nvalue"}
			}
			if _, err := KeyNetworkContext(direct, endpoint, invalid); err == nil {
				t.Fatal("unsafe key context was accepted")
			}
			var requests atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
			}))
			defer server.Close()
			if _, err := FetchKey(direct, http.MethodGet, server.URL, "key-secret", invalid); err == nil || requests.Load() != 0 {
				t.Fatalf("unsafe key headers sent a request: requests=%d err=%v", requests.Load(), err)
			}
		})
	}
	if _, err := KeyNetworkContext(netproxy.With(t.Context(), "http://%invalid"), endpoint, nil); err == nil {
		t.Fatal("invalid proxy was accepted as an API key network context")
	}
	if _, err := KeyNetworkContext(direct, endpoint, http.Header{"User-Agent": {"first"}, "user-agent": {"second"}}); err == nil {
		t.Fatal("ambiguous header spelling was accepted as an API key network context")
	}
}

func TestTwoFactorDoesNotRetainPlainPasswordAndExpiresOnRead(t *testing.T) {
	p := fixture(t)
	p.requireTwoFA.Store(true)
	m := testManager(t, t.TempDir())
	result := signIn(t, m, p, false)
	m.mu.Lock()
	attempt := m.attempts[result.Attempt]
	m.mu.Unlock()
	if attempt.Request.Password != "" || len(attempt.Password) != 0 {
		t.Fatal("default login retained a password for two factor")
	}
	now := m.now()
	m.now = func() time.Time { return now.Add(6 * time.Minute) }
	if _, err := m.Accounts(); err != nil {
		t.Fatal(err)
	}
	if attempt.Temporary != "" {
		t.Fatal("expired two factor secret was retained after a state read")
	}
	if err := m.ValidateAttempt(result.Attempt, "source-1", "revision-1"); err == nil {
		t.Fatal("expired attempt was accepted")
	}
}

func TestSeparateManagersShareRefreshOwnership(t *testing.T) {
	p := fixture(t)
	root := t.TempDir()
	m := testManager(t, root)
	id := signIn(t, m, p, false).Account.ID
	other := testManager(t, root)
	p.rejectAccess.Store(true)
	var group sync.WaitGroup
	for _, manager := range []*Manager{m, other} {
		group.Go(func() {
			if _, err := manager.Read(context.Background(), id, http.MethodGet, "/data", nil); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if p.refreshes.Load() != 1 {
		t.Fatalf("separate session managers rotated %d times", p.refreshes.Load())
	}
}
