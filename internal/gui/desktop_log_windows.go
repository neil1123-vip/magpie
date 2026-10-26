//go:build windows && !nogui

package gui

import (
	"os"
	"runtime/debug"

	"golang.org/x/sys/windows"
)

func setDesktopCrashOutput(f *os.File) error {
	if h, err := windows.GetStdHandle(windows.STD_ERROR_HANDLE); err == nil {
		if kind, err := windows.GetFileType(h); err == nil && kind != windows.FILE_TYPE_UNKNOWN {
			return debug.SetCrashOutput(f, debug.CrashOptions{})
		}
	}
	// Windows native exception tracebacks use the OS handle, not os.Stderr.
	return windows.SetStdHandle(windows.STD_ERROR_HANDLE, windows.Handle(f.Fd()))
}
