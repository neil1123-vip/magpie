package gui

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sub2api"
	"github.com/yetone/magpie/internal/sub2apibrowser"
)

func TestSub2APILoginRecoversAfterCorruptCredentialLogout(t *testing.T) {
	for _, artifact := range []string{"normal", "tokens", "password"} {
		for _, action := range []string{"login", "browser"} {
			t.Run(artifact+"/"+action, func(t *testing.T) {
				home := t.TempDir()
				t.Setenv("HOME", home)
				t.Setenv("USERPROFILE", home)
				t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
				t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
				var logins, opened atomic.Int32
				panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					switch r.URL.Path {
					case "/api/v1/auth/login":
						logins.Add(1)
						fallthrough
					case "/api/v1/auth/refresh":
						w.Write([]byte(`{"data":{"access_token":"fake-access","refresh_token":"fake-refresh"}}`))
					case "/api/v1/auth/me":
						w.Write([]byte(`{"data":{"id":42,"email":"user@example.test"}}`))
					case "/wallet":
						w.Write([]byte(`{"balance":12.5}`))
					default:
						http.NotFound(w, r)
					}
				}))
				t.Cleanup(panel.Close)
				p := provider.Provider{ID: "recovery-panel", Name: "Recovery panel", Chat: panel.URL + "/v1",
					Key: "fake-key", BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
				if err := provider.Save(p); err != nil {
					t.Fatal(err)
				}
				p, err := provider.Sub2APIProvider(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				s, _, err := provider.Sub2APIServices()
				if err != nil {
					t.Fatal(err)
				}
				input, err := provider.Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "fake-password", true, provider.Sub2APIRevision(p))
				if err != nil {
					t.Fatal(err)
				}
				first, err := s.Login(t.Context(), input)
				if err != nil {
					t.Fatal(err)
				}
				var brokenPath string
				const damaged = "corrupt DPAPI blob"
				if artifact != "normal" {
					name := first.Account.ID + ".dpapi"
					if artifact == "password" {
						name = first.Account.ID + "-password.dpapi"
					}
					brokenPath = filepath.Join(appdir.Config(), "sub2api-local", "secrets", name)
					if err := os.WriteFile(brokenPath, []byte(damaged), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				mux := http.NewServeMux()
				sub2apiRoutesWithBrowser(mux, nil, true, "", func(_ context.Context, req sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
					opened.Add(1)
					return sub2api.BrowserCandidate{Origin: req.Base, Access: "fake-access", Refresh: "fake-refresh",
						UserAgent: "Magpie Sub2API", HostExited: true}, nil
				})
				post := func(action string, in any) *httptest.ResponseRecorder {
					t.Helper()
					raw, err := json.Marshal(in)
					if err != nil {
						t.Fatal(err)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sub2api/"+action, bytes.NewReader(raw)))
					return w
				}
				out := post("logout", map[string]any{"session": first.Account.ID, "expectedGeneration": first.Account.Generation})
				var logout sub2api.LogoutResult
				if out.Code != 200 || json.Unmarshal(out.Body.Bytes(), &logout) != nil || logout.Generation <= first.Account.Generation {
					t.Fatalf("logical logout: %d %s", out.Code, out.Body)
				}
				if logout.CleanupPending || logout.CleanupError != "" {
					t.Errorf("credential cleanup did not finish: %+v", logout)
				}
				if brokenPath != "" {
					if _, err := os.Stat(brokenPath); !os.IsNotExist(err) {
						t.Errorf("damaged credentials still occupy the active path: %v", err)
					}
					copies, err := filepath.Glob(filepath.Join(filepath.Dir(brokenPath), ".quarantine-*", filepath.Base(brokenPath)))
					if err != nil || len(copies) != 1 {
						t.Errorf("damaged original was not quarantined: %v %v", copies, err)
					} else if raw, err := os.ReadFile(copies[0]); err != nil || string(raw) != damaged {
						t.Errorf("quarantined original changed: %v", err)
					}
				}
				out = post(action, map[string]any{"provider": p.ID, "base": panel.URL, "email": "user@example.test",
					"password": "fake-password", "remember": true, "expectedRevision": provider.Sub2APIRevision(p)})
				if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), `"state":"ready"`) {
					t.Fatalf("explicit source recovery blocked after logout: status=%d response=%s logins=%d opened=%d",
						out.Code, out.Body, logins.Load(), opened.Load())
				}
				if action == "login" && (logins.Load() != 2 || opened.Load() != 0) ||
					action == "browser" && (logins.Load() != 1 || opened.Load() != 1) {
					t.Fatalf("unexpected recovery requests: logins=%d opened=%d", logins.Load(), opened.Load())
				}
				var restored sub2api.LoginResult
				if err := json.Unmarshal(out.Body.Bytes(), &restored); err != nil ||
					restored.Account == nil || restored.Account.Generation <= logout.Generation {
					t.Fatalf("recovery did not adopt a new generation: %s %v", out.Body, err)
				}
				if brokenPath != "" {
					copies, err := filepath.Glob(filepath.Join(filepath.Dir(brokenPath), ".quarantine-*", filepath.Base(brokenPath)))
					if err != nil || len(copies) != 1 {
						t.Fatalf("recovery lost the quarantined original: %v %v", copies, err)
					}
					if raw, err := os.ReadFile(copies[0]); err != nil || string(raw) != damaged {
						t.Fatalf("recovery changed the quarantined original: %v", err)
					}
				}
			})
		}
	}
}

func TestSub2APIWebWritesAreRejectedBeforeReadingSecrets(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	desktop := Handler(nil, nil)
	web := Handler(webHost{}, nil)
	for _, action := range []string{"login", "2fa", "login-account", "2fa-account", "browser", "browser-account", "cancel", "logout", "bind", "bind-key", "unbind", "refresh", "refresh-account", "preview", "monitor", "apply-rate", "apply-key-rate"} {
		w := httptest.NewRecorder()
		web.ServeHTTP(w, httptest.NewRequest("POST", "/api/sub2api/"+action, strings.NewReader("{not-json")))
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s decoded a read-only request: %d %s", action, w.Code, w.Body)
		}
	}
	for _, tt := range []struct {
		name     string
		handler  http.Handler
		writable bool
	}{{"desktop after web construction", desktop, true}, {"web", web, false}} {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tt.handler.ServeHTTP(w, httptest.NewRequest("GET", "/api/sub2api", nil))
			var result struct {
				Capability struct {
					Writable bool   `json:"writable"`
					Reason   string `json:"reason"`
				} `json:"capability"`
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil ||
				result.Capability.Writable != tt.writable || !tt.writable && result.Capability.Reason == "" {
				t.Fatalf("capability: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestSub2APIUnavailableBrowserRejectsBeforeReadingSecrets(t *testing.T) {
	if available, _ := sub2apibrowser.Available(); available {
		t.Skip("this build includes the native browser host")
	}
	mux := http.NewServeMux()
	sub2apiRoutes(mux, nil)
	for _, action := range []string{"browser", "browser-account"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sub2api/"+action, strings.NewReader("{not-json")))
		if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "Browser verification is unavailable in this build.") {
			t.Fatalf("%s decoded an unavailable browser request: %d %s", action, w.Code, w.Body)
		}
	}
}

func TestSub2APIAccountLoginDoesNotRequireOrBindAProvider(t *testing.T) {
	for _, mode := range []string{"password", "2fa", "browser"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var signingAgain atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					if mode == "2fa" && signingAgain.Load() {
						w.Write([]byte(`{"data":{"requires_2fa":true,"temp_token":"temporary-secret"}}`))
						return
					}
					fallthrough
				case "/api/v1/auth/login/2fa", "/api/v1/auth/refresh":
					w.Write([]byte(`{"data":{"access_token":"panel-secret","refresh_token":"refresh-secret","expires_in":3600}}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/api/v1/groups/available":
					w.Write([]byte(`{"data":[]}`))
				default:
					t.Errorf("account-only login requested %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer panel.Close()
			s, monitor, err := provider.Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			initial, err := s.Login(t.Context(), sub2api.LoginRequest{Base: panel.URL, Email: "reader@example.test", Password: "test-password",
				Target: "deleted-source", Revision: "original-revision", UserAgent: "test-browser"})
			if err != nil || initial.Account == nil {
				t.Fatalf("initial login: %+v, %v", initial, err)
			}
			if err := monitor.SetAccountExpected(initial.Account.ID, true, []sub2api.Group{{ID: "5"}},
				initial.Account.Generation, ""); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				monitor.Run(ctx, nil, nil)
			}()
			defer func() { cancel(); <-done }()
			waitSample := func(generation uint64) {
				t.Helper()
				deadline, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				tick := time.NewTicker(10 * time.Millisecond)
				defer tick.Stop()
				for {
					snapshot := monitor.Snapshot()
					if !snapshot.Polling && len(snapshot.Accounts) == 1 &&
						snapshot.Accounts[0].Generation == generation && !snapshot.Accounts[0].LastSuccess.IsZero() {
						return
					}
					select {
					case <-tick.C:
					case <-deadline.Done():
						t.Fatalf("sign-in did not wake monitoring for generation %d: %+v", generation, snapshot)
					}
				}
			}
			waitSample(initial.Account.Generation)
			signingAgain.Store(true)
			mux := http.NewServeMux()
			sub2apiRoutesWithBrowser(mux, nil, true, "", func(ctx context.Context, req sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
				if req.Attempt == "" || req.Base != panel.URL {
					t.Errorf("browser request: %+v", req)
				}
				return sub2api.BrowserCandidate{Origin: panel.URL, Access: "panel-secret", Refresh: "refresh-secret",
					UserAgent: "test-browser", HostExited: true}, nil
			})
			post := func(action string, in map[string]any, want int) []byte {
				t.Helper()
				body, err := json.Marshal(in)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				mux.ServeHTTP(w, httptest.NewRequest("POST", "/api/sub2api/"+action, strings.NewReader(string(body))))
				if w.Code != want {
					t.Fatalf("%s: %d %s", action, w.Code, w.Body)
				}
				for _, secret := range []string{"test-password", "panel-secret", "refresh-secret", "temporary-secret"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Fatalf("%s exposed a secret", action)
					}
				}
				return w.Body.Bytes()
			}
			in := map[string]any{"session": initial.Account.ID, "expectedGeneration": initial.Account.Generation,
				"email": initial.Account.Email, "password": "test-password"}
			action := "login-account"
			if mode == "browser" {
				action = "browser-account"
				in["base"], in["remember"] = "https://untrusted.invalid", true
			}
			body := post(action, in, http.StatusOK)
			if mode == "2fa" {
				var pending sub2api.LoginResult
				if err := json.Unmarshal(body, &pending); err != nil || pending.Attempt == "" || pending.SessionID != initial.Account.ID {
					t.Fatalf("account attempt: %s, %v", body, err)
				}
				body = post("2fa-account", map[string]any{"session": initial.Account.ID, "expectedGeneration": initial.Account.Generation,
					"attempt": pending.Attempt, "code": "123456"}, http.StatusOK)
			}
			var result sub2api.LoginResult
			if err := json.Unmarshal(body, &result); err != nil || result.Account == nil ||
				result.Account.ID != initial.Account.ID || result.Account.Generation <= initial.Account.Generation ||
				result.Target != "" || strings.Contains(string(body), `"binding"`) {
				t.Fatalf("account-only result: %s, %v", body, err)
			}
			waitSample(result.Account.Generation)
			post(action, in, http.StatusConflict)
		})
	}
}

func TestSub2APISavedLoginKeepsPartialAssociationResult(t *testing.T) {
	for _, mode := range []string{"password", "2fa", "browser"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("USERPROFILE", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			var balances atomic.Int32
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					if mode == "2fa" {
						w.Write([]byte(`{"data":{"requires_2fa":true,"temp_token":"temporary-secret"}}`))
						return
					}
					fallthrough
				case "/api/v1/auth/login/2fa", "/api/v1/auth/refresh":
					w.Write([]byte(`{"data":{"access_token":"panel-secret","refresh_token":"refresh-secret","expires_in":3600}}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/api/v1/user/profile":
					if mode == "browser" && balances.Add(1) == 1 {
						w.Write([]byte(`{"data":{"balance":12.5}}`))
						return
					}
					http.Error(w, "balance temporarily unavailable", http.StatusServiceUnavailable)
				default:
					t.Errorf("unexpected panel request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer panel.Close()
			if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: panel.URL + "/v1",
				Key: "sk-inference", BalanceURL: panel.URL + "/api/v1/user/profile", BalanceToken: "old-static"}); err != nil {
				t.Fatal(err)
			}
			p, err := provider.Sub2APIProvider("relay")
			if err != nil {
				t.Fatal(err)
			}
			h := http.NewServeMux()
			sub2apiRoutesWithBrowser(h, nil, true, "", func(context.Context, sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
				return sub2api.BrowserCandidate{Origin: panel.URL, Access: "panel-secret", Refresh: "refresh-secret",
					UserAgent: "test-browser", HostExited: true}, nil
			})
			post := func(action string, input map[string]any) []byte {
				t.Helper()
				b, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("POST", "/api/sub2api/"+action, strings.NewReader(string(b))))
				if w.Code != http.StatusOK {
					t.Fatalf("%s lost a saved login: %d %s", action, w.Code, w.Body)
				}
				for _, secret := range []string{"panel-secret", "refresh-secret", "temporary-secret", "test-password"} {
					if strings.Contains(w.Body.String(), secret) {
						t.Fatalf("%s exposed a secret", action)
					}
				}
				return w.Body.Bytes()
			}
			action := "login"
			if mode == "browser" {
				action = "browser"
			}
			input := map[string]any{"provider": p.ID, "base": panel.URL, "email": "reader@example.test",
				"password": "test-password", "expectedRevision": provider.Sub2APIRevision(p)}
			if mode == "browser" {
				input["remember"] = true
			}
			body := post(action, input)
			if mode == "2fa" {
				var pending sub2api.LoginResult
				if err := json.Unmarshal(body, &pending); err != nil || pending.Attempt == "" {
					t.Fatalf("2fa attempt: %s, %v", body, err)
				}
				body = post("2fa", map[string]any{"provider": p.ID, "attempt": pending.Attempt, "code": "123456"})
			}
			var result struct {
				sub2api.LoginResult
				Binding struct {
					Linked bool   `json:"linked"`
					Error  string `json:"error"`
				} `json:"binding"`
			}
			if err := json.Unmarshal(body, &result); err != nil || result.Account == nil ||
				result.Account.Generation == 0 || result.State != sub2api.StateReady || result.Binding.Linked || result.Binding.Error == "" {
				t.Fatalf("partial login: %s, %v", body, err)
			}
			current, err := provider.Sub2APIProvider(p.ID)
			if err != nil || current.Sub2API != nil || current.BalanceToken != "old-static" {
				t.Fatalf("failed association changed the old balance: %+v, %v", current, err)
			}
		})
	}
}

func TestSub2APIBrowserRequestCancellationClosesHostWithoutSaving(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: "https://panel.example.test/v1",
		Key: "sk-inference", BalanceToken: "old-static"}); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Sub2APIProvider("relay")
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	sub2apiRoutesWithBrowser(mux, nil, true, "", func(ctx context.Context, req sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return sub2api.BrowserCandidate{}, ctx.Err()
	})
	input, err := json.Marshal(map[string]any{"provider": p.ID, "base": "https://panel.example.test",
		"expectedRevision": provider.Sub2APIRevision(p)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest("POST", "/api/sub2api/browser", strings.NewReader(string(input))).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		mux.ServeHTTP(rec, request)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("browser request outlived its test")
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("browser request did not open the host")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled browser request left its host running")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("browser handler returned before stopping the host")
	}
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"BROWSER_CANCELED"`) {
		t.Fatalf("canceled request: %d %s", rec.Code, rec.Body)
	}
	current, err := provider.Sub2APIProvider(p.ID)
	if err != nil || current.Sub2API != nil || current.BalanceToken != "old-static" {
		t.Fatalf("cancellation changed the old balance: %+v, %v", current, err)
	}
	s, _, err := provider.Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := s.Accounts()
	if err != nil || len(accounts) != 0 {
		t.Fatalf("canceled browser saved an account: %+v, %v", accounts, err)
	}
}

func TestSub2APIPasswordRequestCancellationDoesNotSaveAccount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	entered, release := make(chan struct{}), make(chan struct{})
	releaseLogin := sync.OnceFunc(func() { close(release) })
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			close(entered)
			<-release
			w.Write([]byte(`{"data":{"access_token":"late-access","refresh_token":"late-refresh"}}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42,"email":"reader@example.test"}}`))
		case "/api/v1/user/profile":
			w.Write([]byte(`{"data":{"balance":12.5}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(panel.Close)
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: panel.URL + "/v1",
		Key: "sk-inference", BalanceURL: panel.URL + "/api/v1/user/profile", BalanceToken: "old-static"}); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Sub2APIProvider("relay")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"provider": p.ID, "base": panel.URL,
		"email": "reader@example.test", "password": "test-password", "remember": true,
		"expectedRevision": provider.Sub2APIRevision(p)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	rec := httptest.NewRecorder()
	request := httptest.NewRequest("POST", "/api/sub2api/login", strings.NewReader(string(body))).WithContext(ctx)
	mux := http.NewServeMux()
	sub2apiRoutes(mux, nil)
	done := make(chan struct{})
	go func() { defer close(done); mux.ServeHTTP(rec, request) }()
	t.Cleanup(func() {
		cancel()
		releaseLogin()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("password request outlived its test")
		}
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("password request did not reach its local panel")
	}
	cancel()
	releaseLogin()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled password request did not finish")
	}
	current, err := provider.Sub2APIProvider(p.ID)
	if err != nil || current.Sub2API != nil || current.BalanceToken != "old-static" {
		t.Fatalf("cancellation changed the source balance: %+v %v", current, err)
	}
	s, _, err := provider.Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := s.Accounts()
	if rec.Code == http.StatusOK || err != nil || len(accounts) != 0 {
		t.Fatalf("canceled password request saved an account: status=%d accounts=%+v error=%v response=%s",
			rec.Code, accounts, err, rec.Body)
	}
}

func TestSub2APIBrowserLoginVerifiesBalanceBeforeSaving(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(map[bool]string{true: "verified", false: "rejected"}[succeeds], func(t *testing.T) {
			testSub2APIBrowserBalance(t, succeeds)
		})
	}
}

func testSub2APIBrowserBalance(t *testing.T, succeeds bool) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var refreshes atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("User-Agent") != "test-browser" {
			t.Error("browser validation lost its network context")
		}
		switch r.URL.Path {
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42,"email":"reader@example.test"}}`))
		case "/api/v1/auth/refresh":
			refreshes.Add(1)
			w.Write([]byte(`{"data":{"access_token":"verified-panel-secret","refresh_token":"verified-refresh-secret","expires_in":3600}}`))
		case "/api/v1/user/profile":
			if !succeeds {
				http.Error(w, "candidate balance unavailable", http.StatusServiceUnavailable)
				return
			}
			if r.Header.Get("Authorization") != "Bearer verified-panel-secret" {
				t.Error("balance used an unverified browser candidate")
			}
			w.Write([]byte(`{"data":{"balance":12.5}}`))
		default:
			t.Errorf("unexpected browser sign-in request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: panel.URL + "/v1",
		Key: "sk-inference", BalanceToken: "old-static"}); err != nil {
		t.Fatal(err)
	}
	p, err := provider.Sub2APIProvider("relay")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	sub2apiRoutesWithBrowser(mux, nil, true, "", func(context.Context, sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
		return sub2api.BrowserCandidate{Origin: panel.URL, Access: "candidate-panel-secret", Refresh: "candidate-refresh-secret",
			UserAgent: "test-browser", HostExited: true}, nil
	})
	input, err := json.Marshal(map[string]any{"provider": p.ID, "base": panel.URL, "password": "ignored-password",
		"remember": true, "expectedRevision": provider.Sub2APIRevision(p)})
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sub2api/browser", strings.NewReader(string(input))))
	if !succeeds {
		if rec.Code == http.StatusOK {
			t.Fatalf("failed candidate balance was accepted: %s", rec.Body)
		}
		current, err := provider.Sub2APIProvider(p.ID)
		if err != nil || current.Sub2API != nil || current.BalanceToken != "old-static" {
			t.Fatalf("failed candidate changed the old balance: %+v, %v", current, err)
		}
		s, _, err := provider.Sub2APIServices()
		if err != nil {
			t.Fatal(err)
		}
		accounts, err := s.Accounts()
		if err != nil || len(accounts) != 0 || refreshes.Load() != 1 {
			t.Fatalf("failed candidate saved an account: %+v, %v; refreshes=%d", accounts, err, refreshes.Load())
		}
		return
	}
	var result struct {
		sub2api.LoginResult
		Binding struct {
			Linked bool `json:"linked"`
		} `json:"binding"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Account == nil ||
		result.State != sub2api.StateReady || !result.Binding.Linked || result.Account.Remember || refreshes.Load() != 1 {
		t.Fatalf("verified browser sign-in: %d %s; refreshes=%d", rec.Code, rec.Body, refreshes.Load())
	}
	if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "ignored-password") {
		t.Fatal("browser sign-in returned a secret")
	}
	current, err := provider.Sub2APIProvider(p.ID)
	if err != nil || current.Sub2API == nil || current.Sub2API.Session != result.Account.ID || current.BalanceToken != "" {
		t.Fatalf("verified browser balance was not linked: %+v, %v", current, err)
	}
}

func TestSub2APIAPIRejectsStaleActionsAndKeepsSecretsLocal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var logins atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			logins.Add(1)
			w.Write([]byte(`{"data":{"access_token":"panel-secret","refresh_token":"refresh-secret","expires_in":3600}}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/api/v1/user/profile":
			if r.Header.Get("Authorization") != "Bearer panel-secret" {
				t.Error("account balance did not use the panel token")
			}
			w.Write([]byte(`{"data":{"balance":12.5}}`))
		case "/v1/sub2api/billing":
			if r.Header.Get("Authorization") != "Bearer sk-inference" {
				t.Error("billing did not use the inference key")
			}
			w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
		default:
			t.Errorf("unexpected panel request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Chat: panel.URL + "/v1", Key: "sk-inference"}); err != nil {
		t.Fatal(err)
	}
	current := func() provider.Provider {
		t.Helper()
		p, err := provider.Sub2APIProvider("relay")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	mux := http.NewServeMux()
	sub2apiRoutes(mux, nil)
	post := func(action string, body map[string]any, status int) string {
		t.Helper()
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/sub2api/"+action, strings.NewReader(string(data))))
		if rec.Code != status {
			t.Fatalf("%s: status %d, want %d: %s", action, rec.Code, status, rec.Body)
		}
		for _, secret := range []string{"panel-secret", "refresh-secret", "test-password"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("%s returned a secret", action)
			}
		}
		return rec.Body.String()
	}
	input := map[string]any{"provider": "relay", "base": panel.URL, "email": "user@example.test",
		"password": "test-password", "expectedRevision": "stale"}
	post("login", input, http.StatusConflict)
	if logins.Load() != 0 {
		t.Fatal("a stale login request reached the panel")
	}
	for _, name := range []string{"aUtHoRiZaTiOn", "cOoKiE", "pRoXy-AuThOrIzAtIoN"} {
		p := current()
		p.Headers = map[string]string{name: "another-identity"}
		p.BalanceURL, p.BalanceToken = panel.URL+"/api/v1/user/profile", "old-static"
		if err := provider.Save(p); err != nil {
			t.Fatal(err)
		}
		input["expectedRevision"] = provider.Sub2APIRevision(current())
		post("login", input, http.StatusConflict)
		if logins.Load() != 0 {
			t.Fatalf("%s conflict sent the password before checking the balance headers", name)
		}
		if p := current(); p.BalanceToken != "old-static" || p.Sub2API != nil {
			t.Fatal("login preflight changed the old balance configuration")
		}
	}
	clean := current()
	clean.Headers = map[string]string{"User-Agent": "sub2api-test", "X-Org": "acme"}
	if err := provider.Save(clean); err != nil {
		t.Fatal(err)
	}
	input["expectedRevision"] = provider.Sub2APIRevision(current())
	var login sub2api.LoginResult
	if err := json.Unmarshal([]byte(post("login", input, http.StatusOK)), &login); err != nil || login.Account == nil {
		t.Fatalf("login account: %+v, %v", login, err)
	}
	p := current()
	if p.Sub2API == nil || p.BalanceToken != "" {
		t.Fatal("successful login did not bind the verified balance")
	}
	binding := map[string]any{"provider": p.ID, "session": p.Sub2API.Session, "groupId": "group",
		"expectedVersion": p.Sub2API.Version, "expectedRevision": "stale", "expectedGeneration": login.Account.Generation}
	post("bind", binding, http.StatusConflict)
	binding["expectedRevision"] = provider.Sub2APIRevision(p)
	post("bind", binding, http.StatusOK)
	newer := current()
	// The version confirmed before the newer group binding cannot unlink it.
	binding["expectedRevision"] = provider.Sub2APIRevision(newer)
	post("unbind", binding, http.StatusConflict)
	if current().Sub2API == nil {
		t.Fatal("a stale unlink removed the new binding")
	}
	binding["expectedVersion"] = newer.Sub2API.Version
	post("unbind", binding, http.StatusOK)
	if current().Sub2API != nil {
		t.Fatal("explicit unlink did not remove the binding")
	}
	post("logout", map[string]any{"session": login.Account.ID, "expectedGeneration": login.Account.Generation + 1}, http.StatusConflict)
	var signedOut sub2api.LogoutResult
	logout := post("logout", map[string]any{"session": login.Account.ID, "expectedGeneration": login.Account.Generation}, http.StatusOK)
	if err := json.Unmarshal([]byte(logout), &signedOut); err != nil ||
		signedOut.Generation <= login.Account.Generation || !strings.Contains(logout, `"cleanupPending"`) {
		t.Fatalf("logout result: %s, %v", logout, err)
	}
}
