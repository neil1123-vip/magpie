//go:build !windows && !nogui

package gui

import (
	"os"
	"runtime/debug"
)

func setDesktopCrashOutput(f *os.File) error {
	return debug.SetCrashOutput(f, debug.CrashOptions{})
}
