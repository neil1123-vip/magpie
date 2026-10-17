package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCookiePasswordLoginKeepsLatestSession(t *testing.T) {
	var mu sync.Mutex
	cookie := ""
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/login" {
			cookie = "fixture-session-1"
			http.SetCookie(w, &http.Cookie{Name: "sub2_session", Value: cookie, Path: "/", HttpOnly: true})
			fmt.Fprint(w, `{"code":0,"data":{"access_token":"","refresh_token":"","session_managed":true,"csrf_token":"fixture-csrf-token-1","expires_in":3600}}`)
			return
		}
		c, err := r.Cookie("sub2_session")
		if err != nil || c.Value != cookie || r.Header.Get("Authorization") != "" {
			http.Error(w, `{"code":401}`, http.StatusUnauthorized)
			return
		}
		reads++
		cookie = fmt.Sprintf("fixture-session-%d", reads+1)
		http.SetCookie(w, &http.Cookie{Name: "sub2_session", Value: cookie, Path: "/", HttpOnly: true})
		if r.URL.Path == "/api/v1/auth/me" {
			fmt.Fprint(w, `{"code":0,"data":{"id":42,"email":"account@example.test"}}`)
		} else {
			fmt.Fprint(w, `{"code":0,"data":{"balance":88.07}}`)
		}
	}))
	defer server.Close()
	m := testManager(t, t.TempDir())
	result, err := m.Login(t.Context(), LoginRequest{
		Base: server.URL, Proxy: "direct", Email: "account@example.test", Password: "password",
		Target: "source", Revision: "revision",
	})
	if err != nil {
		t.Fatalf("BFF password login did not complete: %v", err)
	}
	if result.Account == nil {
		t.Fatal("BFF password login returned no account")
	}
	for range 2 {
		if _, err := m.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil); err != nil {
			t.Fatalf("BFF balance request lost the rotated session: %v", err)
		}
	}
	_, secret, err := m.load(result.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(secret.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]any
	if json.Unmarshal(raw, &saved) != nil || saved["session_cookie"] != "fixture-session-4" ||
		saved["csrf_token"] != "fixture-csrf-token-1" || saved["access_token"] != "" || saved["refresh_token"] != "" {
		t.Fatal("BFF login or reads did not preserve the latest encrypted session")
	}
}

type cookiePanelFixture struct {
	mu                   sync.Mutex
	cookie, csrf         string
	serial, logins       int
	refreshes, requests  int
	twoFA, badRefresh    bool
	failRead, revokeRead bool
	dropRead             bool
	readStarted          chan struct{}
	readRelease          chan struct{}
}

func cookieFixture(t *testing.T) (*httptest.Server, *cookiePanelFixture) {
	t.Helper()
	p := &cookiePanelFixture{cookie: "fixture-session-0", csrf: "fixture-csrf-token-0"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.requests++
		w.Header().Set("Content-Type", "application/json")
		login := r.URL.Path == "/api/v1/auth/login" || r.URL.Path == "/api/v1/auth/login/2fa"
		if login {
			if p.twoFA && r.URL.Path == "/api/v1/auth/login" {
				fmt.Fprint(w, `{"code":0,"data":{"requires_2fa":true,"temp_token":"fixture-2fa"}}`)
				return
			}
			p.logins++
		} else {
			cookie, err := r.Cookie("sub2_session")
			if err != nil || cookie.Value != p.cookie || r.Header.Get("Authorization") != "" {
				http.Error(w, `{"code":401}`, http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodPost && (r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("X-CSRF-Token") != p.csrf) {
				http.Error(w, `{"code":403}`, http.StatusForbidden)
				return
			}
			if r.URL.Path == "/api/v1/user/profile" && p.revokeRead {
				p.revokeRead = false
				http.Error(w, `{"code":401}`, http.StatusUnauthorized)
				return
			}
		}
		if r.URL.Path == "/api/v1/user/profile" && p.readStarted != nil {
			close(p.readStarted)
			<-p.readRelease
		}
		p.serial++
		p.cookie = fmt.Sprintf("fixture-session-%d", p.serial)
		if r.URL.Path == "/api/v1/user/profile" && p.dropRead {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		if r.URL.Path == "/api/v1/user/profile" && p.failRead {
			http.Error(w, `{"code":503}`, http.StatusServiceUnavailable)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "sub2_session", Value: p.cookie, Path: "/", HttpOnly: true})
		switch r.URL.Path {
		case "/api/v1/auth/login", "/api/v1/auth/login/2fa", "/api/v1/auth/refresh":
			if r.URL.Path == "/api/v1/auth/refresh" {
				p.refreshes++
				if p.badRefresh {
					http.Error(w, `{"code":401}`, http.StatusUnauthorized)
					return
				}
			}
			p.csrf = fmt.Sprintf("fixture-csrf-token-%d", p.serial)
			fmt.Fprintf(w, `{"code":0,"data":{"access_token":"","refresh_token":"","session_managed":true,"csrf_token":%q,"expires_in":3600}}`, p.csrf)
		case "/api/v1/auth/me":
			fmt.Fprint(w, `{"code":0,"data":{"id":42,"email":"account@example.test"}}`)
		case "/api/v1/groups/available":
			fmt.Fprint(w, `{"code":0,"data":[{"id":1,"name":"group"}]}`)
		default:
			fmt.Fprint(w, `{"code":0,"data":{"balance":88.07}}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, p
}

func TestCookieBrowserHandoffAndConcurrentReads(t *testing.T) {
	server, p := cookieFixture(t)
	m := testManager(t, t.TempDir())
	result, err := m.BrowserLogin(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct", Target: "source", Revision: "revision"},
		func(_ context.Context, req BrowserRequest) (BrowserCandidate, error) {
			return BrowserCandidate{Origin: req.Base, SessionCookie: p.cookie, CSRF: p.csrf, UserAgent: "fixture-browser", HostExited: true}, nil
		}, browserBalance)
	if err != nil || result.Account == nil {
		t.Fatalf("BFF browser handoff failed: %v", err)
	}
	if err := m.Refresh(t.Context(), result.Account.ID); err != nil {
		t.Fatalf("BFF explicit refresh failed: %v", err)
	}
	if _, err := m.Read(t.Context(), result.Account.ID, http.MethodPost, server.URL+"/api/v1/user/profile", nil); err != nil {
		t.Fatalf("BFF POST omitted its session's CSRF context: %v", err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if _, err := m.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil); err != nil {
				t.Errorf("concurrent BFF read reused a rotated cookie: %v", err)
			}
		})
	}
	wg.Wait()
	_, saved, err := m.load(result.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if saved.Tokens.SessionCookie != p.cookie || saved.Tokens.CSRF != p.csrf || p.logins != 0 || p.refreshes != 2 {
		t.Fatal("BFF handoff, explicit refresh or concurrent reads lost the final session")
	}
}

func TestCookieTwoFactorAndRememberedRecovery(t *testing.T) {
	server, p := cookieFixture(t)
	p.twoFA = true
	m := testManager(t, t.TempDir())
	pending, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct", Email: "account@example.test",
		Password: "password", Remember: true, Target: "source", Revision: "revision"})
	if err != nil || pending.State != StateTwoFA {
		t.Fatalf("BFF 2FA did not return its challenge: %v", err)
	}
	result, err := m.Complete2FA(t.Context(), pending.Attempt, "123456")
	if err != nil || result.Account == nil {
		t.Fatalf("BFF 2FA did not complete: %v", err)
	}
	p.mu.Lock()
	p.twoFA, p.revokeRead = false, true
	p.mu.Unlock()
	if _, err := m.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil); err != nil {
		t.Fatalf("BFF remembered-password recovery failed: %v", err)
	}
	_, secret, err := m.load(result.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.logins != 2 || secret.Tokens.SessionCookie != p.cookie || secret.Tokens.CSRF != p.csrf {
		t.Fatal("BFF recovery did not preserve the new session")
	}
}

func TestCookieReLoginRecoversUncertainBearerSession(t *testing.T) {
	for _, browser := range []bool{false, true} {
		t.Run(fmt.Sprintf("browser=%v", browser), func(t *testing.T) {
			server, panel := cookieFixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			result, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct",
				UserAgent: "saved-browser-agent", Email: "account@example.test", Password: "password",
				Target: "source", Revision: "revision"})
			if err != nil {
				t.Fatal(err)
			}
			record, secret, err := m.load(result.Account.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Older builds left a bearer refresh pending when the panel switched to BFF.
			secret.Tokens = tokenPair{Access: "old-access", Refresh: "consumed-refresh"}
			if err := m.saveSecret(m.secretPath(secret.ID), secret); err != nil {
				t.Fatal(err)
			}
			record.Pending = &pendingRotation{Version: secret.Version, Generation: secret.Generation,
				Hash: refreshFingerprint(secret.Tokens.Refresh), Attempt: "uncertain-refresh"}
			record.View.State, record.View.LastError = StateUnknown, "Sub2API did not return a complete token pair"
			if err := m.writeRecord(record); err != nil {
				t.Fatal(err)
			}
			m = testManager(t, root)
			input := LoginRequest{SessionID: secret.ID, ExpectedGeneration: secret.Generation,
				Email: "account@example.test"}
			if browser {
				result, err = m.BrowserLogin(t.Context(), input,
					func(_ context.Context, req BrowserRequest) (BrowserCandidate, error) {
						if req.UserAgent != secret.UserAgent {
							t.Error("account re-login lost its saved browser user agent")
						}
						return BrowserCandidate{Origin: req.Base, SessionCookie: panel.cookie, CSRF: panel.csrf,
							UserAgent: req.UserAgent, HostExited: true}, nil
					})
			} else {
				input.Password = "password"
				result, err = m.Login(t.Context(), input)
			}
			if err != nil || result.Account == nil {
				t.Fatalf("uncertain legacy session could not be replaced by explicit login: %v", err)
			}
			if _, err := m.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil); err != nil {
				t.Fatalf("recovered cookie account could not read its balance: %v", err)
			}
			recovered, saved, err := m.load(secret.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Pending != nil || recovered.View.State != StateReady || recovered.View.LastError != "" ||
				saved.Generation != secret.Generation+1 || saved.Tokens.SessionCookie == "" ||
				saved.Tokens.Access != "" || saved.Tokens.Refresh != "" || saved.UserAgent != secret.UserAgent {
				t.Fatal("explicit cookie re-login retained the uncertain bearer session")
			}
		})
	}
}

func TestCookieAmbiguousRotationIsNeverReplayed(t *testing.T) {
	for _, failure := range []string{"read", "refresh"} {
		t.Run(failure, func(t *testing.T) {
			server, p := cookieFixture(t)
			root := t.TempDir()
			m := testManager(t, root)
			result, err := m.Login(t.Context(), LoginRequest{Base: server.URL, Proxy: "direct", Email: "account@example.test",
				Password: "password", Target: "source", Revision: "revision"})
			if err != nil {
				t.Fatal(err)
			}
			p.mu.Lock()
			p.failRead, p.badRefresh = failure == "read", failure == "refresh"
			p.mu.Unlock()
			if failure == "read" {
				_, err = m.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil)
			} else {
				err = m.Refresh(t.Context(), result.Account.ID)
			}
			var detail *Error
			if !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
				t.Fatalf("ambiguous BFF rotation was not fenced: %v", err)
			}
			p.mu.Lock()
			requests := p.requests
			p.mu.Unlock()
			restarted := testManager(t, root)
			_, err = restarted.Read(t.Context(), result.Account.ID, http.MethodGet, server.URL+"/api/v1/user/profile", nil)
			if !errors.As(err, &detail) || detail.Code != "REFRESH_UNCERTAIN" {
				t.Fatalf("restart forgot the uncertain BFF rotation: %v", err)
			}
			p.mu.Lock()
			defer p.mu.Unlock()
			if requests != p.requests {
				t.Fatal("uncertain BFF session was sent again after restart")
			}
		})
	}
}

func TestCookieAuthResponseRequiresOneFreshCookie(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "mixed", "injected"} {
		t.Run(mode, func(t *testing.T) {
			m := testManager(t, t.TempDir())
			m.transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				headers := make(http.Header)
				raw := `{"data":{"session_managed":true,"csrf_token":"fixture-csrf-token"}}`
				if mode != "missing" {
					headers.Add("Set-Cookie", "sub2_session=fixture-session; Path=/; HttpOnly")
				}
				if mode == "duplicate" {
					headers.Add("Set-Cookie", "sub2_session=another-session; Path=/other; HttpOnly")
				}
				if mode == "mixed" {
					raw = `{"data":{"session_managed":true,"csrf_token":"fixture-csrf-token","access_token":"access","refresh_token":"refresh"}}`
				}
				if mode == "injected" {
					headers.Del("Set-Cookie")
					raw = `{"data":{"access_token":"access","refresh_token":"refresh","session_cookie":"injected","csrf_token":"fixture-csrf-token"}}`
				}
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(raw)), Request: r}, nil
			})
			_, err := m.Login(t.Context(), LoginRequest{Base: "https://panel.test", Proxy: "direct", Email: "account@example.test",
				Password: "password", Target: "source", Revision: "revision"})
			if err == nil {
				t.Fatal("incomplete or ambiguous BFF authentication was accepted")
			}
		})
	}
}
