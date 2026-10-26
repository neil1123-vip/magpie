//go:build !nogui

package gui

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

func testDesktopWriter(t *testing.T) *desktopLog {
	t.Helper()
	w := &desktopLog{path: filepath.Join(t.TempDir(), "desktop.log")}
	t.Cleanup(func() {
		if w.file != nil {
			w.file.Close()
		}
	})
	return w
}

func TestDesktopLogRotation(t *testing.T) {
	w := testDesktopWriter(t)
	write := func(p []byte) {
		t.Helper()
		if n, err := w.Write(p); err != nil || n != len(p) {
			t.Fatalf("write: %d, %v; want %d", n, err, len(p))
		}
	}
	first := bytes.Repeat([]byte("a"), desktopLogSize)
	write(first)
	write(nil)
	if _, err := os.Stat(w.path + ".1"); !os.IsNotExist(err) {
		t.Fatalf("rotated before exceeding the limit: %v", err)
	}
	write([]byte("second\n"))
	if b, err := os.ReadFile(w.path + ".1"); err != nil || !bytes.Equal(b, first) {
		t.Fatalf("backup lost the previous log: size %d, %v", len(b), err)
	}
	write(bytes.Repeat([]byte("b"), desktopLogSize+1))
	write([]byte("last\n"))
	b, err := os.ReadFile(w.path + ".1")
	if err != nil || len(b) != desktopLogSize || !bytes.HasSuffix(b, []byte("\n[desktop: log entry truncated]\n")) {
		t.Fatalf("oversized entry not bounded: size %d, %v", len(b), err)
	}
	if b, err := os.ReadFile(w.path); err != nil || string(b) != "last\n" {
		t.Fatalf("current log: %q, %v", b, err)
	}
	files, err := os.ReadDir(filepath.Dir(w.path))
	if err != nil || len(files) != 2 {
		t.Fatalf("logs: %v, %v; want current and one backup", files, err)
	}
}

func TestDesktopLogConcurrentWrites(t *testing.T) {
	w := testDesktopWriter(t)
	p := bytes.Repeat([]byte("c"), 256<<10)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 8 {
				if n, err := w.Write(p); err != nil || n != len(p) {
					t.Errorf("concurrent write: %d, %v", n, err)
					return
				}
			}
		})
	}
	wg.Wait()
	for _, path := range []string{w.path, w.path + ".1"} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() == 0 || info.Size() > desktopLogSize {
			t.Errorf("log size %d outside limit", info.Size())
		}
	}
}

func TestDesktopLogWriteFailure(t *testing.T) {
	w := testDesktopWriter(t)
	w.path = filepath.Join(filepath.Dir(w.path), "missing", "desktop.log")
	if n, err := w.Write([]byte("not written")); err == nil || n != 0 {
		t.Fatalf("open failure: %d, %v", n, err)
	}
	if err := os.Mkdir(filepath.Dir(w.path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(bytes.Repeat([]byte("a"), desktopLogSize)); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(w.path+".1", "blocked")
	if err := os.Mkdir(w.path+".1", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("next\n")); err == nil || n != 0 {
		t.Fatalf("rotation failure: %d, %v", n, err)
	}
	info, err := os.Stat(w.path)
	if err != nil || info.Size() != desktopLogSize {
		t.Fatalf("failed rotation changed the current log: %v, %v", info, err)
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(w.path + ".1"); err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("next\n")); err != nil || n != 5 {
		t.Fatalf("rotation did not recover: %d, %v", n, err)
	}
}

func TestDesktopLogRenameFailure(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows open handles block renaming")
	}
	w := testDesktopWriter(t)
	p := bytes.Repeat([]byte("a"), desktopLogSize)
	if _, err := w.Write(p); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(w.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if n, err := w.Write([]byte("retry\n")); err == nil || n != 0 {
		t.Fatalf("rename failure: %d, %v", n, err)
	}
	if b, err := io.ReadAll(reader); err != nil || !bytes.Equal(b, p) {
		t.Fatalf("failed rename lost the current log: size %d, %v", len(b), err)
	}
	reader.Close()
	if n, err := w.Write([]byte("retry\n")); err != nil || n != 6 {
		t.Fatalf("write did not recover after rename failure: %d, %v", n, err)
	}
	if b, err := os.ReadFile(w.path + ".1"); err != nil || !bytes.Equal(b, p) {
		t.Fatalf("retry lost the backup: size %d, %v", len(b), err)
	}
}

func TestDesktopLogCleanup(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	makeFile := func(id int, suffix string, size int64, age time.Duration) string {
		t.Helper()
		path := filepath.Join(dir, fmt.Sprintf("desktop-20261026-120000-1-%d%s", id, suffix))
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		err = f.Truncate(size)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		at := now.Add(-age)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
		return path
	}
	expired := makeFile(1, ".log", 1, desktopLogAge+time.Hour)
	oldest := makeFile(2, ".log.1", 12<<20, 6*24*time.Hour)
	older := makeFile(3, ".log", 12<<20, 5*24*time.Hour)
	newest := makeFile(4, ".log", 2<<20, 4*24*time.Hour)
	oldCrash := makeFile(5, ".crash", 1024, 20*24*time.Hour)
	keptCrash := makeFile(6, ".crash", 1024, 19*24*time.Hour)
	newCrash := makeFile(7, ".crash", 1024, 18*24*time.Hour)
	extraCrash := makeFile(8, ".crash", 1024, 21*24*time.Hour)
	var empty []string
	for i := 10; i < 20; i++ {
		empty = append(empty, makeFile(i, ".crash", 0, time.Hour))
	}
	active := makeFile(20, ".log", 30<<20, desktopLogAge+time.Hour)
	activeCrash := makeFile(20, ".crash", 0, desktopLogAge+time.Hour)
	lock, err := os.OpenFile(activeCrash, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if held, err := filelock.TryLock(lock); err != nil || !held {
		t.Fatalf("hold active log: %v, %v", held, err)
	}
	foreign := filepath.Join(dir, "desktop-user-notes.log")
	if err := os.WriteFile(foreign, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	foreignDir := filepath.Join(dir, "desktop-20261026-120000-1-21.log")
	if err := os.Mkdir(foreignDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := cleanDesktopLogs(dir, now); err != nil {
			t.Fatal(err)
		}
		for _, path := range append(empty, expired, oldest, extraCrash) {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("stale file kept: %s, %v", path, err)
			}
		}
		for _, path := range []string{older, newest, oldCrash, keptCrash, newCrash, active, activeCrash, foreign, foreignDir} {
			if _, err := os.Stat(path); err != nil {
				t.Errorf("retained file removed: %s, %v", path, err)
			}
		}
	}
	largeCrash := makeFile(30, ".crash", desktopLogHistory+1, 0)
	if err := cleanDesktopLogs(dir, now); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{largeCrash, keptCrash, newCrash, active, activeCrash} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("crash budget removed a protected file: %s, %v", path, err)
		}
	}
	for _, path := range []string{oldCrash, older, newest} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("crash budget left excess history: %s, %v", path, err)
		}
	}
}

func TestDesktopLog(t *testing.T) {
	if dir := os.Getenv("MAGPIE_TEST_DESKTOP_LOG"); dir != "" {
		appdir.UseExecutable(filepath.Join(dir, "magpie.exe"))
		stderr, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		stderr.Close()
		os.Stderr = stderr // A desktop launch can have no valid standard stream.
		log.SetOutput(stderr)
		if err := startDesktopLog("crash-probe"); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("MAGPIE_TEST_DESKTOP_PANIC") == "1" {
			log.Print(strings.Repeat("rotation probe ", desktopLogSize/4))
			_, _ = io.WriteString(os.Stderr, strings.Repeat("stderr rotation probe ", desktopLogSize/4))
		}
		_, _ = io.WriteString(os.Stderr, "desktop stderr before exit\n")
		// Wait for the pipe, then exercise an immediate exit with no logger flush.
		deadline := time.Now().Add(10 * time.Second)
		for {
			files, _ := filepath.Glob(filepath.Join(dir, "data", "cache", "logs", "*.log"))
			if len(files) == 1 {
				b, _ := os.ReadFile(files[0])
				if bytes.Contains(b, []byte("desktop stderr before exit")) {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("stderr pipe did not drain")
			}
			time.Sleep(5 * time.Millisecond)
		}
		log.Print("desktop error before exit")
		if os.Getenv("MAGPIE_TEST_DESKTOP_PANIC") == "1" {
			go func() { panic("desktop background panic probe") }()
			select {}
		}
		os.Exit(23) // Wails also reports fatal errors before exiting directly.
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exit", "panic"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, appdir.MarkerName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := proc.CommandContext(ctx, self, "-test.run=^TestDesktopLog$")
			cmd.Env = append(os.Environ(), "MAGPIE_TEST_DESKTOP_LOG="+dir)
			wantExit := 23
			if mode == "panic" {
				cmd.Env = append(cmd.Env, "MAGPIE_TEST_DESKTOP_PANIC=1")
				wantExit = 2
			}
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			err := cmd.Run()
			if ctx.Err() != nil || err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != wantExit {
				t.Fatalf("child exit: %v, state %v, context %v; want %d", err, cmd.ProcessState, ctx.Err(), wantExit)
			}
			logDir := filepath.Join(dir, "data", "cache", "logs")
			files, err := filepath.Glob(filepath.Join(logDir, "desktop-*.log*"))
			wantFiles := 1
			if mode == "panic" {
				wantFiles = 2
			}
			if err != nil || len(files) != wantFiles {
				t.Fatalf("desktop logs: %v, %v; want %d", files, err, wantFiles)
			}
			var logs []byte
			for _, path := range files {
				b, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if len(b) > desktopLogSize {
					t.Errorf("log exceeded limit: %d bytes", len(b))
				}
				logs = append(logs, b...)
			}
			for _, want := range []string{"desktop error before exit", "desktop stderr before exit"} {
				if !bytes.Contains(logs, []byte(want)) {
					t.Errorf("log missing %q", want)
				}
			}
			if mode == "exit" && !bytes.Contains(logs, []byte("starting magpie crash-probe")) {
				t.Error("startup log missing")
			}
			crashes, err := filepath.Glob(filepath.Join(logDir, "desktop-*.crash"))
			if err != nil || len(crashes) != 1 {
				t.Fatalf("crash files: %v, %v; want one", crashes, err)
			}
			b, err := os.ReadFile(crashes[0])
			if err != nil {
				t.Fatal(err)
			}
			if mode == "panic" && (!bytes.Contains(b, []byte("panic: desktop background panic probe")) || !bytes.Contains(b, []byte("desktop_log_test.go:"))) {
				t.Errorf("background panic stack was lost:\n%s", b)
			}
			if err := cleanDesktopLogs(logDir, time.Now()); err != nil {
				t.Fatal(err)
			}
			_, err = os.Stat(crashes[0])
			if mode == "exit" && !os.IsNotExist(err) || mode == "panic" && err != nil {
				t.Errorf("crash retention after %s: %v", mode, err)
			}
		})
	}
}
