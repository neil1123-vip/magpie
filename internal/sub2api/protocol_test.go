package sub2api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestPanelPostRedirectDoesNotReplayCredentials(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, path := range []string{"/api/v1/auth/login", "/api/v1/auth/login/2fa", "/api/v1/auth/refresh", "/balance"} {
			t.Run(fmt.Sprintf("%d%s", status, path), func(t *testing.T) {
				var first, redirected atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/redirected" {
						redirected.Add(1)
						fmt.Fprint(w, `{"data":{"balance":12.5}}`)
						return
					}
					first.Add(1)
					var payload map[string]string
					if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&payload) != nil || payload["credential"] != "one-use-secret" {
						t.Error("initial request lost its POST credentials")
					}
					w.Header().Set("Location", "/redirected")
					w.WriteHeader(status)
				}))
				defer server.Close()
				m := testManager(t, t.TempDir())
				_, err := m.request(t.Context(), sessionSecret{Base: server.URL, Proxy: "direct"},
					http.MethodPost, server.URL+path, nil, map[string]string{"credential": "one-use-secret"}, "access-secret")
				if err == nil || first.Load() != 1 || redirected.Load() != 0 {
					t.Fatalf("POST followed a redirect: error=%v initial=%d redirected=%d", err, first.Load(), redirected.Load())
				}
			})
		}
	}
}

func TestPanelReadRedirectKeepsSameOriginReads(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, status := range []int{http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s/%d", method, status), func(t *testing.T) {
				var redirected atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/redirected" {
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(status)
						return
					}
					redirected.Add(1)
					if r.Method != method || r.Header.Get("Authorization") != "Bearer access-secret" {
						t.Error("redirected read lost its method or session")
					}
					fmt.Fprint(w, `{"data":{"balance":12.5}}`)
				}))
				defer server.Close()
				m := testManager(t, t.TempDir())
				_, err := m.request(t.Context(), sessionSecret{Base: server.URL, Proxy: "direct"},
					method, server.URL+"/balance", nil, nil, "access-secret")
				if err != nil || redirected.Load() != 1 {
					t.Fatalf("same-origin read did not follow its redirect: error=%v redirected=%d", err, redirected.Load())
				}
			})
		}
	}
}
