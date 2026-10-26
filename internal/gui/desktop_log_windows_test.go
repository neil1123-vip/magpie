//go:build windows && !nogui

package gui

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/proc"
	"golang.org/x/sys/windows"
)

func TestDesktopNativeException(t *testing.T) {
	if dir := os.Getenv("MAGPIE_TEST_DESKTOP_EXCEPTION"); dir != "" {
		appdir.UseExecutable(filepath.Join(dir, "magpie.exe"))
		if err := windows.SetStdHandle(windows.STD_ERROR_HANDLE, 0); err != nil {
			t.Fatal(err)
		}
		if err := startDesktopLog("native-exception-probe"); err != nil {
			t.Fatal(err)
		}
		if os.Getenv("MAGPIE_TEST_DESKTOP_NATIVE_MODE") == "panic" {
			panic("desktop OS stderr panic probe")
		}
		windows.NewLazySystemDLL("kernel32.dll").NewProc("RaiseException").Call(0xe0000001, 0, 0, 0)
		t.Fatal("native exception returned")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"exception", "panic"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, appdir.MarkerName), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := proc.CommandContext(ctx, self, "-test.run=^TestDesktopNativeException$")
			cmd.Env = append(os.Environ(), "MAGPIE_TEST_DESKTOP_EXCEPTION="+dir, "MAGPIE_TEST_DESKTOP_NATIVE_MODE="+mode)
			err := cmd.Run()
			if ctx.Err() != nil || err == nil || cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 2 {
				t.Fatalf("child exit: %v, state %v, context %v; want 2", err, cmd.ProcessState, ctx.Err())
			}
			files, err := filepath.Glob(filepath.Join(dir, "data", "cache", "logs", "*.crash"))
			if err != nil || len(files) != 1 {
				t.Fatalf("crash files: %v, %v; want one", files, err)
			}
			b, err := os.ReadFile(files[0])
			if err != nil {
				t.Fatal(err)
			}
			want := "Exception 0xe0000001"
			if mode == "panic" {
				want = "panic: desktop OS stderr panic probe"
			}
			if bytes.Count(b, []byte(want)) != 1 || !bytes.Contains(b, []byte("desktop_log_windows_test.go:")) {
				t.Fatalf("exception stack lost or duplicated:\n%s", b)
			}
		})
	}
}
