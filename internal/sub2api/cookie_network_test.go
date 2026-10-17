package sub2api

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCookieReadCancellationKeepsRotatedSession(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			server, panel := cookieFixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			result, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct",
				Email: "account@example.test", Password: "password", Target: "source", Revision: "revision"})
			if err != nil {
				t.Fatal(err)
			}
			canceled, stop := context.WithCancel(t.Context())
			stop()
			if _, err := m.Read(canceled, result.Account.ID, method, server.URL+"/api/v1/user/profile", nil); !errors.Is(err, context.Canceled) {
				t.Fatalf("already canceled query returned %v", err)
			}
			unchanged, _, err := m.load(result.Account.ID)
			if err != nil || unchanged.Pending != nil || unchanged.View.Version != result.Account.Version {
				t.Fatal("already canceled query changed the saved session")
			}
			started, release := make(chan struct{}), make(chan struct{})
			releaseRead := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseRead)
			panel.mu.Lock()
			panel.readStarted, panel.readRelease = started, release
			panel.mu.Unlock()
			wire := make(chan context.Context, 1)
			base := m.transport
			m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				wire <- r.Context()
				return base.RoundTrip(r)
			})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := m.Read(ctx, result.Account.ID, method, server.URL+"/api/v1/user/profile", nil)
				done <- err
			}()
			<-started
			requestContext := <-wire
			cancel()
			if requestContext.Err() != nil {
				t.Error("canceling a query canceled its pending cookie rotation")
			}
			releaseRead()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Errorf("canceled query returned %v, want context.Canceled", err)
			}
			panel.mu.Lock()
			panel.readStarted, panel.readRelease = nil, nil
			cookie := panel.cookie
			panel.mu.Unlock()
			restarted := testManager(t, root)
			record, secret, err := restarted.load(result.Account.ID)
			if err != nil || record.Pending != nil || record.View.State != StateReady ||
				secret.Version != result.Account.Version+1 || secret.Tokens.SessionCookie != cookie {
				t.Fatalf("query cancellation lost the rotated session: state=%s pending=%t err=%v",
					record.View.State, record.Pending != nil, err)
			}
			if _, err := restarted.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil); err != nil {
				t.Fatalf("canceled query prevented later monitoring: %v", err)
			}
		})
	}
}

func TestNativeTokenAuthKeepsSessionAfterPanelFailure(t *testing.T) {
	panel := fixture(t)
	m := testManager(t, t.TempDir())
	base := m.transport
	outage := false
	m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/api/v1/auth/") {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("native authentication did not request the JSON token contract")
			}
			for _, key := range []string{"Cookie", "Origin", "Referer", "Sec-Fetch-Site", "Sec-Fetch-Dest", "Sec-Fetch-User", "Sec-Fetch-Mode"} {
				if _, ok := r.Header[key]; ok {
					t.Errorf("native authentication sent browser marker %s", key)
				}
			}
		}
		if outage && r.URL.Path == "/api/v1/user/profile" {
			return &http.Response{StatusCode: http.StatusBadGateway, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"code":502,"message":"Backend unavailable"}`)), Request: r}, nil
		}
		return base.RoundTrip(r)
	})
	result, err := m.Login(t.Context(), LoginRequest{Base: panel.server.URL, Proxy: "direct",
		UserAgent: "Mozilla/5.0 fixture-browser", Email: "account@example.test", Password: "password",
		Target: "source", Revision: "revision"})
	if err != nil {
		t.Fatal(err)
	}
	outage = true
	_, err = m.Read(t.Context(), result.Account.ID, http.MethodGet, "/api/v1/user/profile", nil)
	var detail *Error
	if !errors.As(err, &detail) || detail.Status != http.StatusBadGateway || detail.Code == "REFRESH_UNCERTAIN" {
		t.Fatalf("ordinary native read failure invalidated its session: %v", err)
	}
	record, secret, err := m.load(result.Account.ID)
	if err != nil || record.Pending != nil || record.View.State != StateReady || secret.Tokens.SessionCookie != "" {
		t.Fatalf("native panel failure left an uncertain cookie session: state=%s err=%v", record.View.State, err)
	}
	outage = false
	panel.rejectAccess.Store(true)
	if _, err := m.Read(t.Context(), result.Account.ID, http.MethodGet, "/api/v1/user/profile", nil); err != nil {
		t.Fatalf("native session could not refresh after panel recovery: %v", err)
	}
	if panel.refreshes.Load() != 1 || panel.logins.Load() != 1 {
		t.Fatal("native recovery unexpectedly repeated authentication")
	}
}

func TestCookieConnectionFailureCanRecover(t *testing.T) {
	for _, operation := range []string{"read", "refresh"} {
		t.Run(operation, func(t *testing.T) {
			server, p := cookieFixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			result, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct",
				Email: "account@example.test", Password: "password", Target: "source", Revision: "revision"})
			if err != nil {
				t.Fatal(err)
			}
			id := result.Account.ID
			read := func(manager *Manager) error {
				if operation == "refresh" {
					return manager.Refresh(t.Context(), id)
				}
				_, err := manager.Read(t.Context(), id, http.MethodGet, server.URL+"/api/v1/user/profile", nil)
				return err
			}
			p.mu.Lock()
			before := p.requests
			p.mu.Unlock()
			offline := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("fixture proxy connection failed")
			}}
			t.Cleanup(offline.CloseIdleConnections)
			m.transport = offline
			err = read(m)
			var detail *Error
			if !errors.As(err, &detail) || detail.Kind != KindTemporary || detail.Code == "REFRESH_UNCERTAIN" {
				t.Fatalf("connection failure incorrectly requires a new sign-in: %v", err)
			}
			if !strings.Contains(monitorError(err), "network and proxy") {
				t.Fatal("monitor hid the connection recovery action")
			}
			record, secret, err := m.load(id)
			if err != nil || record.Pending != nil || record.View.State == StateUnknown || secret.Version != result.Account.Version {
				t.Fatalf("unsent request left an uncertain rotation: state=%s pending=%t err=%v",
					record.View.State, record.Pending != nil, err)
			}
			p.mu.Lock()
			requests := p.requests
			p.mu.Unlock()
			if requests != before {
				t.Fatal("failed connection unexpectedly reached the panel")
			}
			restarted := testManager(t, root)
			resumeAt := time.Now().Add(2 * time.Minute)
			restarted.now = func() time.Time { return resumeAt }
			if err := read(restarted); err != nil {
				t.Fatalf("restored connection could not resume the saved session: %v", err)
			}
		})
	}
}

func TestCookieLostResponseStillRequiresSignIn(t *testing.T) {
	for _, failure := range []string{"disconnect", "disconnect_then_dial_failure", "untraced"} {
		t.Run(failure, func(t *testing.T) {
			server, p := cookieFixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			result, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct",
				Email: "account@example.test", Password: "password", Target: "source", Revision: "revision"})
			if err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			p.dropRead = true
			p.mu.Unlock()
			transport := &http.Transport{}
			offline := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("fixture retry connection failed")
			}}
			t.Cleanup(transport.CloseIdleConnections)
			t.Cleanup(offline.CloseIdleConnections)
			m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if failure == "untraced" {
					return nil, errors.New("fixture transport did not report connection events")
				}
				response, err := transport.RoundTrip(r)
				if err != nil && failure == "disconnect_then_dial_failure" {
					return offline.RoundTrip(r)
				}
				return response, err
			})
			id := result.Account.ID
			_, err = m.Read(t.Context(), id, http.MethodGet, server.URL+"/api/v1/user/profile", nil)
			var detail *Error
			if !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
				t.Fatalf("lost response was treated as safe to retry: %v", err)
			}
			if !strings.Contains(monitorError(err), "sign in again") {
				t.Fatal("monitor hid the session recovery action")
			}
			p.mu.Lock()
			before := p.requests
			p.mu.Unlock()
			restarted := testManager(t, root)
			_, err = restarted.Read(t.Context(), id, http.MethodGet, server.URL+"/api/v1/user/profile", nil)
			if !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
				t.Fatalf("restart forgot the lost response: %v", err)
			}
			p.mu.Lock()
			requests := p.requests
			p.mu.Unlock()
			if requests != before {
				t.Fatal("possibly consumed cookie was replayed after restart")
			}
		})
	}
}

func TestMonitorAuthenticationCooldownExplainsRecovery(t *testing.T) {
	err := (cooldown{Until: time.Now().Add(time.Hour), NeedsAction: true}).err(time.Now(), 1)
	if !strings.Contains(monitorError(err), "Sign in") {
		t.Fatal("authentication cooldown hid the sign-in recovery action")
	}
	if !strings.Contains(monitorError(&Error{Kind: KindAuth, Status: http.StatusUnauthorized}), "HTTP 401") {
		t.Fatal("API-key rejection was mislabeled as a panel sign-in failure")
	}
}
