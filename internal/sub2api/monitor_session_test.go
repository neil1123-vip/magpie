package sub2api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitorExplicitLoginReleasesLocalRefreshBackoff(t *testing.T) {
	for _, serverRetryAfter := range []time.Duration{0, 30 * time.Second, 2 * time.Minute} {
		t.Run(serverRetryAfter.String(), func(t *testing.T) {
			var logins, groups, refreshes atomic.Int64
			var challenge atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					n := logins.Add(1)
					fmt.Fprintf(w, `{"data":{"access_token":"access-%d","refresh_token":"refresh-%d"}}`, n, n)
				case "/api/v1/auth/me":
					fmt.Fprint(w, `{"data":{"id":42,"email":"account@example.test"}}`)
				case "/api/v1/auth/refresh":
					refreshes.Add(1)
					w.Header().Set("cf-mitigated", "challenge")
					if serverRetryAfter > 0 {
						w.Header().Set("Retry-After", fmt.Sprint(int(serverRetryAfter/time.Second)))
					}
					w.WriteHeader(http.StatusForbidden)
					fmt.Fprint(w, `{"code":"CHALLENGE"}`)
				case "/api/v1/groups/available":
					groups.Add(1)
					if challenge.Load() && r.Header.Get("Authorization") == "Bearer access-1" {
						w.WriteHeader(http.StatusUnauthorized)
						fmt.Fprint(w, `{"code":401}`)
						return
					}
					rate := 1
					if challenge.Load() {
						rate = 3
					}
					fmt.Fprintf(w, `{"data":[{"id":5,"name":"Group A","rate_multiplier":%d}]}`, rate)
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(panel.Close)
			root := filepath.Join(t.TempDir(), "local")
			session := testManager(t, root)
			now := time.Now()
			session.now = func() time.Time { return now }
			login := func() AccountView {
				t.Helper()
				got, err := session.Login(t.Context(), LoginRequest{
					Base: panel.URL, Email: "account@example.test", Password: "fictional-password",
					Proxy: "direct", Target: "source", Revision: "test-revision",
				})
				if err != nil {
					t.Fatal(err)
				}
				return *got.Account
			}
			first := login()
			newMonitor := func() *Monitor {
				t.Helper()
				monitor, err := NewMonitor(root, func(ctx context.Context, id, path string) ([]byte, error) {
					view, err := session.Info(id)
					if err != nil {
						return nil, err
					}
					return session.Read(ctx, id, http.MethodGet, view.Base+path, nil)
				}, monitorTestBilling)
				if err != nil {
					t.Fatal(err)
				}
				monitor.now = func() time.Time { return now }
				monitor.SetGenerationGuard(session.WithGeneration)
				monitor.SetTargetReader(func() ([]Target, error) {
					view, err := session.Info(first.ID)
					if err != nil {
						return nil, err
					}
					return []Target{{ID: "source", Site: view.Base, SessionID: view.ID,
						Context: view.Context, Generation: view.Generation, GroupID: "5", GroupName: "Group A"}}, nil
				})
				return monitor
			}
			monitor := newMonitor()
			if err := monitor.SetAccountExpected(first.ID, true, nil, first.Generation, ""); err != nil {
				t.Fatal(err)
			}
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			challenge.Store(true)
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			record, err := session.record(first.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantDelay := serverRetryAfter
			if wantDelay == 0 {
				wantDelay = time.Minute
			}
			if !record.View.RetryAt.Equal(now.Add(wantDelay)) {
				t.Errorf("local refresh cooldown = %s, want %s", record.View.RetryAt.Sub(now), wantDelay)
			}
			monitor.mu.Lock()
			before := monitor.cool[first.ID+"\x00/api/v1/groups/available"]
			monitor.mu.Unlock()
			if refreshes.Load() != 1 || !before.Challenge {
				t.Fatalf("fixture missed the refresh challenge: refreshes=%d cooldown=%+v", refreshes.Load(), before)
			}
			if before.RetryAfter != serverRetryAfter {
				t.Errorf("monitor treated a local delay as server Retry-After: got %s, want %s", before.RetryAfter, serverRetryAfter)
			}
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if groups.Load() != 2 || refreshes.Load() != 1 {
				t.Fatal("the same login bypassed the challenge cooldown")
			}
			monitor = newMonitor()
			next := login()
			if next.Generation <= first.Generation {
				t.Fatal("explicit login did not advance generation")
			}
			monitor.Wake(next.ID)
			if err := monitor.refresh(t.Context(), wakeRound); err != nil {
				t.Fatal(err)
			}
			got := monitor.Snapshot().Accounts[0]
			wantReads := int64(3)
			if serverRetryAfter > 0 {
				wantReads = 2
			}
			if groups.Load() != wantReads {
				t.Errorf("new login groups requests = %d, want %d; error = %q", groups.Load(), wantReads, got.GroupsError)
			}
			if serverRetryAfter == 0 && (got.GroupsError != "" || !got.Complete ||
				len(got.Groups) != 1 || got.Groups[0].Rate == nil || *got.Groups[0].Rate != 3) {
				t.Errorf("recovered login did not read the changed group rate: %+v", got)
			}
		})
	}
}
