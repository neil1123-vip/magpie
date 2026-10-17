package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func browserInput(p *panelFixture) LoginRequest {
	return LoginRequest{Base: p.server.URL, Proxy: "direct", Target: "source-1", Revision: "revision-1"}
}

func browserCandidate(req BrowserRequest) BrowserCandidate {
	ua := req.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return BrowserCandidate{Origin: req.Base, Access: "browser-access", Refresh: "browser-refresh",
		UserAgent: ua, HostExited: true}
}

func browserBalance(ctx context.Context, view AccountView, read BrowserRead) error {
	raw, err := read(ctx, http.MethodGet, view.Base+"/api/v1/user/profile", nil)
	if err != nil {
		return err
	}
	var reply struct {
		Data struct {
			Balance *float64 `json:"balance"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Data.Balance == nil {
		return errors.New("browser balance was not readable")
	}
	return nil
}

func browserGroups(t *testing.T, m *Manager) {
	t.Helper()
	transport := m.transport
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1/groups/available" {
			return &http.Response{StatusCode: 200, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"data":[{"id":1,"name":"group"}]}`)), Request: r}, nil
		}
		return transport.RoundTrip(r)
	})
}

func TestBrowserLoginWaitsForExitAndSavesOneRotatedPair(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	var requests atomic.Int64
	transport := m.transport
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if r.UserAgent() != "actual-browser-network-agent" {
			t.Errorf("handoff lost the actual user agent: %q", r.UserAgent())
		}
		if r.URL.Path == "/api/v1/user/profile" && r.Header.Get("Authorization") != "Bearer rotated-access" {
			t.Error("source verification did not use the final candidate")
		}
		return transport.RoundTrip(r)
	})
	result, err := m.BrowserLogin(t.Context(), browserInput(p), func(ctx context.Context, req BrowserRequest) (BrowserCandidate, error) {
		if req.Attempt == "" || req.Base != p.server.URL || req.Proxy != "direct" || ctx.Err() != nil || requests.Load() != 0 {
			t.Fatalf("browser request started API traffic or lost its context: %+v", req)
		}
		candidate := browserCandidate(req)
		candidate.UserAgent = "actual-browser-network-agent"
		return candidate, nil
	}, browserBalance)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account == nil || result.Account.Subject != "42" || result.Account.Email != "account@example.test" ||
		result.Account.UserAgent != "actual-browser-network-agent" || p.refreshes.Load() != 1 || requests.Load() != 4 {
		t.Fatalf("incomplete verified handoff: %+v, refresh=%d requests=%d", result, p.refreshes.Load(), requests.Load())
	}
	_, secret, err := m.load(result.Account.ID)
	if err != nil || secret.Tokens.Access != "rotated-access" || secret.Tokens.Refresh != "rotated-refresh" {
		t.Fatalf("candidate pair was saved instead of its rotated result: %+v %v", secret.Tokens, err)
	}
	if _, err := os.Stat(m.passwordPath(result.Account.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("browser login created a password: %v", err)
	}
}

func TestBrowserSourceLoginRequiresBalanceValidatorBeforeOpening(t *testing.T) {
	for _, name := range []string{"missing", "nil", "multiple"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			validators := []BrowserValidator(nil)
			switch name {
			case "nil":
				validators = []BrowserValidator{nil}
			case "multiple":
				validators = []BrowserValidator{browserBalance, browserBalance}
			}
			_, err := m.BrowserLogin(t.Context(), browserInput(p),
				func(context.Context, BrowserRequest) (BrowserCandidate, error) {
					t.Fatal("opened a browser without a valid source verifier")
					return BrowserCandidate{}, nil
				}, validators...)
			var detail *Error
			if !errors.As(err, &detail) || detail.Kind != KindInvalid {
				t.Fatalf("missing source verification was not rejected: %v", err)
			}
			if p.logins.Load()+p.refreshes.Load()+p.reads.Load() != 0 {
				t.Fatal("invalid source browser login sent a request")
			}
		})
	}
}

func TestBrowserSourceVerificationFailurePreservesSavedSession(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	rejected := errors.New("configured balance could not be read")
	var verified bool
	_, err := m.BrowserLogin(t.Context(), browserInput(p),
		func(_ context.Context, req BrowserRequest) (BrowserCandidate, error) {
			return browserCandidate(req), nil
		}, func(ctx context.Context, view AccountView, read BrowserRead) error {
			verified = true
			if view.ID != first.ID || view.Context == "" || view.Proxy != "" || view.Generation != 0 || view.Version != 0 {
				t.Errorf("source verifier received unsafe or incomplete metadata: %+v", view)
			}
			if err := browserBalance(ctx, view, read); err != nil {
				return err
			}
			return rejected
		})
	if !verified || !errors.Is(err, rejected) || p.refreshes.Load() != 1 {
		t.Fatalf("source verifier was bypassed: verified=%v refresh=%d err=%v", verified, p.refreshes.Load(), err)
	}
	view, viewErr := m.Info(first.ID)
	_, secret, secretErr := m.load(first.ID)
	var password savedPassword
	passwordErr := m.readSecret(m.passwordPath(first.ID), &password)
	if viewErr != nil || secretErr != nil || passwordErr != nil || view.Generation != first.Generation ||
		view.Version != first.Version || view.State != StateReady || !view.Remember ||
		secret.Tokens.Refresh != "refresh-1" || password.Password != "password-secret" {
		t.Fatalf("failed source verification replaced saved credentials: %+v %v %v %v", view, viewErr, secretErr, passwordErr)
	}
}

func TestBrowserLoginRejectsUnverifiedCandidateWithoutReplacingSession(t *testing.T) {
	for _, name := range []string{"running", "origin", "user-agent", "empty-user-agent", "tokens",
		"first-identity", "final-identity", "unknown-refresh", "groups"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			first := signIn(t, m, p, true).Account
			browserGroups(t, m)
			var identities atomic.Int64
			transport := m.transport
			m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				changeID := r.URL.Path == "/api/v1/auth/me" &&
					(name == "first-identity" || identities.Add(1) == 2 && name == "final-identity")
				if changeID {
					return &http.Response{StatusCode: 200, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{"data":{"id":43}}`)), Request: r}, nil
				}
				if name == "groups" && r.URL.Path == "/api/v1/groups/available" {
					return &http.Response{StatusCode: 503, Header: make(http.Header),
						Body: io.NopCloser(strings.NewReader(`{}`)), Request: r}, nil
				}
				return transport.RoundTrip(r)
			})
			if name == "unknown-refresh" {
				p.refreshStatus.Store(503)
			}
			_, err := m.BrowserLogin(t.Context(), LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation},
				func(_ context.Context, req BrowserRequest) (BrowserCandidate, error) {
					candidate := browserCandidate(req)
					switch name {
					case "running":
						candidate.HostExited = false
					case "origin":
						candidate.Origin = "https://another.example.test"
					case "user-agent":
						candidate.UserAgent = "another-agent"
					case "empty-user-agent":
						candidate.UserAgent = ""
					case "tokens":
						candidate.Refresh = ""
					}
					return candidate, nil
				}, browserBalance)
			if err == nil {
				t.Fatal("unverified browser candidate was accepted")
			}
			if name == "unknown-refresh" {
				var detail *Error
				if !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
					t.Fatalf("unknown refresh was not distinguished: %v", err)
				}
			}
			view, viewErr := m.Info(first.ID)
			_, secret, secretErr := m.load(first.ID)
			if viewErr != nil || secretErr != nil || view.Generation != first.Generation || view.Version != first.Version ||
				view.State != StateReady || secret.Tokens.Refresh != "refresh-1" || !view.Remember {
				t.Fatalf("failed candidate changed the saved session: %+v %v %v", view, viewErr, secretErr)
			}
			wantRefresh := int64(0)
			if name == "final-identity" || name == "unknown-refresh" || name == "groups" {
				wantRefresh = 1
			}
			if p.refreshes.Load() != wantRefresh {
				t.Fatalf("candidate refresh was repeated or used too early: %d, want %d", p.refreshes.Load(), wantRefresh)
			}
		})
	}
}

func TestBrowserAccountLoginPreservesRememberedPassword(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	browserGroups(t, m)
	result, err := m.BrowserLogin(t.Context(), LoginRequest{SessionID: first.ID, ExpectedGeneration: first.Generation},
		func(_ context.Context, req BrowserRequest) (BrowserCandidate, error) {
			return browserCandidate(req), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var password savedPassword
	if err := m.readSecret(m.passwordPath(first.ID), &password); err != nil || password.Password != "password-secret" {
		t.Fatalf("browser re-login removed the remembered password: %v", err)
	}
	if result.Account == nil || !result.Account.Remember || result.Account.Generation != first.Generation+1 ||
		result.Account.Version != first.Version+1 || p.logins.Load() != 1 || p.refreshes.Load() != 1 {
		t.Fatalf("account browser re-login lost its saved preferences: %+v", result)
	}
}

func TestBrowserLoginCancellationReleasesOriginAndRejectsLateCandidate(t *testing.T) {
	for _, name := range []string{"request", "cancel", "logout", "other-manager-logout", "replacement"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t)
			m := testManager(t, t.TempDir())
			first := signIn(t, m, p, false).Account
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered := make(chan BrowserRequest, 1)
			done := make(chan error, 1)
			go func() {
				_, err := m.BrowserLogin(ctx, browserInput(p), func(ctx context.Context, req BrowserRequest) (BrowserCandidate, error) {
					entered <- req
					<-ctx.Done()
					return browserCandidate(req), nil
				}, browserBalance)
				done <- err
			}()
			request := <-entered
			switch name {
			case "request":
				cancel()
			case "cancel":
				m.Cancel(request.Attempt)
			case "logout", "other-manager-logout":
				owner := m
				if name == "other-manager-logout" {
					owner = testManager(t, m.root)
				}
				if _, err := owner.LogoutExpected(t.Context(), first.ID, first.Generation); err != nil {
					t.Fatal(err)
				}
			case "replacement":
				_, _ = m.Login(t.Context(), LoginRequest{Base: p.server.URL, Email: first.Email, Password: "password-secret",
					Proxy: "direct", Target: "source-1", Revision: "revision-2"})
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("late browser candidate survived cancellation")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("browser opener did not observe cancellation")
			}
			if p.refreshes.Load() != 0 {
				t.Fatal("canceled browser candidate consumed its refresh")
			}
			unlock, err := m.lockAuth(p.server.URL)
			if err != nil {
				t.Fatalf("canceled browser still owns the site: %v", err)
			}
			unlock()
		})
	}
}

func TestOriginAuthenticationGateCoversGoActionsAndNotLogout(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	p.requireTwoFA.Store(true)
	twoFA := signIn(t, m, p, false)
	unlock, err := m.lockAuth(p.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	actions := []func() error{
		func() error {
			_, err := m.Login(t.Context(), LoginRequest{Base: p.server.URL, Email: first.Email, Password: "password-secret",
				Proxy: "direct", Target: "another-source", Revision: "revision-1"})
			return err
		},
		func() error { _, err := m.Complete2FA(t.Context(), twoFA.Attempt, "123456"); return err },
		func() error { return m.Refresh(t.Context(), first.ID) },
	}
	for _, action := range actions {
		var detail *Error
		if err := action(); !errors.As(err, &detail) || detail.Kind != KindTemporary || detail.Code != "AUTH_BUSY" {
			t.Fatalf("authentication did not respect the origin owner: %v", err)
		}
	}
	if p.logins.Load() != 2 || p.refreshes.Load() != 0 {
		t.Fatal("busy authentication sent credentials")
	}
	record, err := m.record(first.ID)
	if err != nil || record.Pending != nil || record.View.State != StateReady {
		t.Fatalf("busy authentication changed durable state: %+v %v", record.View, err)
	}
	if _, err := m.LogoutExpected(t.Context(), first.ID, first.Generation); err != nil {
		t.Fatalf("origin owner blocked logout: %v", err)
	}
}

func TestAuthenticationGateCanonicalOrigin(t *testing.T) {
	for input, want := range map[string]string{
		"https://EXAMPLE.test/path":       "https://example.test:443",
		"https://example.test:443/api/v1": "https://example.test:443",
		"http://127.0.0.1/path":           "http://127.0.0.1:80",
		"http://[::1]:0080/path":          "http://[::1]:80",
	} {
		origin, err := canonicalOrigin(input)
		if err != nil || origin != want {
			t.Fatalf("canonical origin %q = %q, want %q: %v", input, origin, want, err)
		}
	}
	for _, input := range []string{"https://example.test:0", "https://example.test:65536", "https://example.test:bad"} {
		if _, err := canonicalOrigin(input); err == nil {
			t.Fatalf("invalid origin port accepted: %s", input)
		}
	}
	m := testManager(t, t.TempDir())
	unlock, err := m.lockAuth("https://example.test/path")
	if err != nil {
		t.Fatal(err)
	}
	other := testManager(t, m.root)
	if extra, err := other.lockAuth("https://EXAMPLE.test:443/api/v1"); err == nil {
		extra()
		unlock()
		t.Fatal("equivalent origins obtained independent authentication ownership")
	}
	unlock()
	extra, err := other.lockAuth("https://EXAMPLE.test:443/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	extra()
}
