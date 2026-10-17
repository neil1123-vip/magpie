package sub2api

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

type sessionProcess struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Reader
	errors bytes.Buffer
	waited bool
}

func startSessionProcess(t *testing.T, root, id string, mode ...string) *sessionProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	args := append([]string{"-test.run=^TestSub2APIProcessHelper$", "--", root, id}, mode...)
	child := &sessionProcess{cmd: proc.CommandContext(ctx, os.Args[0], args...)}
	var err error
	child.input, err = child.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	output, err := child.cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	child.output = bufio.NewReader(output)
	child.cmd.Stderr = &child.errors
	if err := child.cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !child.waited {
			child.cmd.Process.Kill()
			child.cmd.Wait()
		}
		child.input.Close()
		cancel()
	})
	child.expect(t, "ready")
	return child
}

func (child *sessionProcess) expect(t *testing.T, want string) {
	t.Helper()
	line, err := child.output.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != want {
		t.Fatalf("session subprocess said %q, want %q: %v", line, want, err)
	}
}

func (child *sessionProcess) start(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(child.input, "start"); err != nil {
		t.Fatal(err)
	}
	child.expect(t, "starting")
}

func (child *sessionProcess) wait(t *testing.T) {
	t.Helper()
	remaining, err := io.ReadAll(child.output)
	if err != nil {
		t.Fatal(err)
	}
	err = child.cmd.Wait()
	child.waited = true
	if err != nil {
		t.Fatalf("session subprocess failed: %v\n%s%s", err, remaining, child.errors.String())
	}
}

func TestSub2APIProcessHelper(t *testing.T) {
	marker := -1
	for i, arg := range os.Args {
		if arg == "--" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return
	}
	args := os.Args[marker+1:]
	if len(args) < 2 || len(args) > 3 {
		t.Fatal("missing subprocess account")
	}
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	fmt.Println("starting")
	m := testManager(t, args[0])
	err := m.refresh(t.Context(), args[1], 1)
	if len(args) == 3 && args[2] == "busy-refresh" {
		var detail *Error
		if !errors.As(err, &detail) || detail.Code != "AUTH_BUSY" {
			t.Fatalf("in-flight authentication did not return busy: %v", err)
		}
		return
	}
	if len(args) == 3 && args[2] == "stale-refresh" {
		var detail *Error
		if !errors.As(err, &detail) || detail.Kind != KindConflict {
			t.Fatalf("late refresh did not reject its write: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestSeparateProcessLogoutInvalidatesBlockedRefresh(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	releaseRefresh := sync.OnceFunc(func() { close(release) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/refresh":
			close(entered)
			<-release
			fmt.Fprint(w, `{"data":{"access_token":"rotated-access","refresh_token":"rotated-refresh"}}`)
		case "/api/v1/auth/login":
			fmt.Fprint(w, `{"data":{"access_token":"access-1","refresh_token":"refresh-1"}}`)
		default:
			fmt.Fprint(w, `{"data":{"id":42}}`)
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(releaseRefresh)
	m := testManager(t, t.TempDir())
	first, err := m.Login(t.Context(), LoginRequest{
		Base: server.URL, Email: "account@example.test", Password: "password-secret",
		Proxy: "direct", Target: "source-1", Revision: "revision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	child := startSessionProcess(t, m.root, first.Account.ID, "stale-refresh")
	t.Cleanup(func() {
		releaseRefresh()
		if !child.waited {
			child.cmd.Process.Kill()
			child.cmd.Wait()
			child.waited = true
		}
		waitSessionCleanups(t, m)
	})
	child.start(t)
	<-entered
	result, err := m.LogoutExpected(t.Context(), first.Account.ID, first.Account.Generation)
	if err != nil || !result.CleanupPending || result.Generation != first.Account.Generation+1 {
		t.Fatalf("logout did not bypass a remote owner: %+v %v", result, err)
	}
	record, err := m.record(first.Account.ID)
	if err != nil || record.View.State != StateLoggedOut || record.Pending != nil {
		t.Fatalf("logout was not durable while child owned the session: %+v %v", record, err)
	}
	releaseRefresh()
	child.wait(t)
	waitSessionCleanups(t, m)
	record, err = m.record(first.Account.ID)
	if err != nil || record.View.State != StateLoggedOut || record.View.Generation != result.Generation || record.View.CleanupPending {
		t.Fatalf("remote completion replaced logout: %+v %v", record, err)
	}
}

func TestSeparateProcessRefreshReturnsBusyOrReusesCompletedVersion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/refresh" {
			if refreshes.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"data":{"access_token":"rotated-access","refresh_token":"rotated-refresh"}}`)
			return
		}
		if r.URL.Path == "/api/v1/auth/login" {
			fmt.Fprint(w, `{"data":{"access_token":"access-1","refresh_token":"refresh-1"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"id":42}}`)
	}))
	t.Cleanup(server.Close)
	releaseRefresh := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseRefresh)
	m := testManager(t, t.TempDir())
	result, err := m.Login(t.Context(), LoginRequest{
		Base: server.URL, Email: "account@example.test", Password: "password-secret",
		Proxy: "direct", Target: "source-1", Revision: "revision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := result.Account.ID
	first := startSessionProcess(t, m.root, id)
	first.start(t)
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	unlock, held := filelock.Lock(filepath.Join(m.root, "locks", id+".lock"), 0)
	unlock()
	if held {
		t.Fatal("refreshing child did not retain an OS account lock")
	}
	second := startSessionProcess(t, m.root, id, "busy-refresh")
	second.start(t)
	second.wait(t)
	record, err := m.record(id)
	if err != nil || record.Pending == nil || record.View.Version != 1 || record.View.State != StateReady {
		t.Fatalf("busy reader changed its owner's durable state: %+v %v", record, err)
	}
	releaseRefresh()
	first.wait(t)
	completed := startSessionProcess(t, m.root, id)
	completed.start(t)
	completed.wait(t)
	if refreshes.Load() != 1 {
		t.Fatalf("two subprocesses consumed the same refresh token %d times", refreshes.Load())
	}
	view, err := m.Info(id)
	if err != nil || view.Version != 2 || view.State != StateReady {
		t.Fatalf("subprocess rotation was not durable: %+v %v", view, err)
	}
}

func TestBrowserAuthenticationGateBlocksSeparateProcessRefresh(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, done := make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := m.BrowserLogin(ctx, browserInput(p),
			func(ctx context.Context, req BrowserRequest) (BrowserCandidate, error) {
				close(entered)
				<-ctx.Done()
				return browserCandidate(req), nil
			}, browserBalance)
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-entered
	child := startSessionProcess(t, m.root, first.ID, "busy-refresh")
	child.start(t)
	child.wait(t)
	record, err := m.record(first.ID)
	if err != nil || record.Pending != nil || record.View.Generation != first.Generation ||
		record.View.Version != first.Version || record.View.State != StateReady || p.refreshes.Load() != 0 {
		t.Fatalf("remote refresh bypassed the browser origin owner: %+v %v", record, err)
	}
}

func TestCrashedRefreshProcessNeverReplaysItsPendingToken(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var refreshes atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/auth/refresh" {
			if refreshes.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, `{"data":{"access_token":"lost-access","refresh_token":"lost-refresh"}}`)
			return
		}
		if r.URL.Path == "/api/v1/auth/login" {
			fmt.Fprint(w, `{"data":{"access_token":"access-1","refresh_token":"refresh-1"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"id":42}}`)
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })
	m := testManager(t, t.TempDir())
	result, err := m.Login(t.Context(), LoginRequest{
		Base: server.URL, Email: "account@example.test", Password: "password-secret",
		Proxy: "direct", Target: "source-1", Revision: "revision-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	id := result.Account.ID
	child := startSessionProcess(t, m.root, id)
	child.start(t)
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
	record, err := m.record(id)
	if err != nil || record.Pending == nil {
		t.Fatalf("request was sent before the pending marker was durable: %+v %v", record, err)
	}
	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	child.cmd.Wait()
	child.waited = true
	restarted := testManager(t, m.root)
	if err := restarted.Refresh(t.Context(), id); err == nil {
		t.Fatal("restart accepted the unconfirmed refresh")
	}
	if refreshes.Load() != 1 {
		t.Fatalf("crashed refresh was replayed %d times", refreshes.Load())
	}
	view, err := restarted.Info(id)
	if err != nil || view.State != StateUnknown {
		t.Fatalf("restart lost the uncertain refresh state: %+v %v", view, err)
	}
}
