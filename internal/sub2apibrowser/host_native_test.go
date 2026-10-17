//go:build windows && !nogui && sub2api_webview2

package sub2apibrowser

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/sub2api"
	"github.com/yetone/magpie/internal/testenv"
	"golang.org/x/sys/windows"
)

func TestMain(m *testing.M) {
	if runJobFixture() {
		return
	}
	if MaybeRun() {
		return
	}
	if len(os.Args) == 3 && os.Args[1] == "--sub2api-login-fixture" {
		_, _ = Login(context.Background(), sub2api.BrowserRequest{Base: os.Args[2], Proxy: "direct"})
		return
	}
	os.Exit(testenv.Run(m))
}

func TestNativeHostHandoff(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	var refreshes atomic.Int32
	const userAgent = "Sub2API native fixture"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/auth/refresh" {
			refreshes.Add(1)
			t.Error("browser refresh reached server")
			http.Error(w, "unexpected refresh", 500)
			return
		}
		if r.URL.Path != "/login" {
			http.NotFound(w, r)
			return
		}
		if r.UserAgent() != userAgent {
			t.Errorf("network user agent = %q", r.UserAgent())
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><title>Sub2API fixture</title><script>
(async()=>{
 const response=await fetch('/api/v1/auth/refresh',{method:'POST'});
 if(response.status!==403)return;
 localStorage.setItem('auth_token','fixture-access');
 localStorage.setItem('refresh_token','fixture-refresh');
})();
</script>`)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := Login(ctx, sub2api.BrowserRequest{Base: server.URL, Proxy: "direct", UserAgent: userAgent})
	if err != nil {
		t.Fatal(err)
	}
	if !c.HostExited || c.Origin != server.URL || c.Access != "fixture-access" ||
		c.Refresh != "fixture-refresh" || c.UserAgent != userAgent || refreshes.Load() != 0 {
		t.Fatalf("invalid handoff: origin=%q exited=%v ua=%q refreshes=%d",
			c.Origin, c.HostExited, c.UserAgent, refreshes.Load())
	}
	assertNoProfile(t, temp)
}

func TestNativeHostCancellation(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	entered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			select {
			case entered <- struct{}{}:
			default:
			}
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<!doctype html><title>Waiting for cancellation</title>")
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Login(ctx, sub2api.BrowserRequest{Base: server.URL, Proxy: "direct"})
		done <- err
	}()
	select {
	case <-entered:
		cancel()
	case err := <-done:
		t.Fatalf("host exited before showing the fixture: %v", err)
	case <-ctx.Done():
		t.Fatal("host did not reach the local fixture")
	}
	if err := <-done; err == nil {
		t.Fatal("canceled browser returned success")
	}
	assertNoProfile(t, temp)
}

func TestNativeProfilesHaveIndependentCleaners(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	first, cleanupFirst, err := startPrivateProfile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	firstClosed := false
	defer func() {
		if !firstClosed {
			_ = cleanupFirst()
		}
	}()
	second, cleanupSecond, err := startPrivateProfile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := cleanupSecond(); err != nil {
			t.Error(err)
		}
	}()
	if first == second || filepath.Dir(first) != temp || filepath.Dir(second) != temp {
		t.Fatal("cleaners did not create separate profiles under the temporary root")
	}
	if err := cleanupFirst(); err != nil {
		t.Fatal(err)
	}
	firstClosed = true
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("first profile remains after cleanup: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("cleanup removed an active profile: %v", err)
	}
}

func TestNativeHostRejectsProfileCleanupFailure(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	locked := make(chan windows.Handle, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			http.NotFound(w, r)
			return
		}
		profiles, err := filepath.Glob(filepath.Join(temp, "magpie-sub2api-*"))
		if err != nil || len(profiles) != 1 {
			t.Errorf("profile fixture = %v: %v", profiles, err)
		} else {
			path, err := windows.UTF16PtrFromString(filepath.Join(profiles[0], "cleanup-locked"))
			if err == nil {
				var handle windows.Handle
				handle, err = windows.CreateFile(path, windows.GENERIC_WRITE,
					windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
					windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
				if err == nil {
					locked <- handle
				}
			}
			if err != nil {
				t.Errorf("cannot lock the profile fixture: %v", err)
			}
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<!doctype html><script>
localStorage.setItem('auth_token','fixture-access');
localStorage.setItem('refresh_token','fixture-refresh');
</script>`)
	}))
	defer server.Close()
	defer func() {
		select {
		case handle := <-locked:
			_ = windows.CloseHandle(handle)
		default:
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	c, err := Login(ctx, sub2api.BrowserRequest{Base: server.URL, Proxy: "direct"})
	if err == nil || !strings.Contains(err.Error(), "profile cleanup failed") || c.Access != "" || c.Refresh != "" {
		t.Fatalf("cleanup failure returned a usable candidate: err=%v access=%v refresh=%v",
			err, c.Access != "", c.Refresh != "")
	}
}

func TestNativeHostParentExitRemovesProfile(t *testing.T) {
	const secret = "SUB2API_CRASH_PROFILE_FIXTURE"
	temp := t.TempDir()
	t.Setenv("TMP", temp)
	t.Setenv("TEMP", temp)
	reached := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/stored" {
			select {
			case reached <- struct{}{}:
			default:
			}
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!doctype html><script>
localStorage.setItem('auth_token',%q);
fetch('/stored');
</script>`, secret)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := proc.CommandContext(ctx, os.Args[0], "--sub2api-login-fixture", server.URL)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	}()
	select {
	case <-reached:
	case <-ctx.Done():
		t.Fatal("browser never wrote localStorage")
	}
	var stored string
	for deadline := time.Now().Add(10 * time.Second); stored == "" && time.Now().Before(deadline); {
		if err := filepath.WalkDir(temp, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			data, err := os.ReadFile(path)
			if err == nil && bytes.Contains(data, []byte(secret)) {
				stored = path
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if stored == "" {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if stored == "" {
		t.Fatal("fixture token was not persisted; the crash case was not exercised")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	for deadline := time.Now().Add(15 * time.Second); ; {
		entries, err := os.ReadDir(temp)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "magpie-sub2api-") {
				found = true
			}
		}
		if !found {
			return
		}
		if time.Now().After(deadline) {
			data, readErr := os.ReadFile(stored)
			t.Fatalf("profile remains after parent exit; plaintext token readable=%v",
				readErr == nil && bytes.Contains(data, []byte(secret)))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func assertNoProfile(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		for _, entry := range entries {
			t.Logf("remaining temporary entry: %q", entry.Name())
		}
		t.Fatalf("browser left %d profile entries", len(entries))
	}
}
