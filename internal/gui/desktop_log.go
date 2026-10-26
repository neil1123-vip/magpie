//go:build !nogui

package gui

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
)

const (
	desktopLogSize    = 5 << 20
	desktopLogHistory = 25 << 20
	desktopLogAge     = 7 * 24 * time.Hour
	desktopCrashKeep  = 3
)

var desktopLogName = regexp.MustCompile(`^desktop-[0-9]{8}-[0-9]{6}-[0-9]+-[0-9]+(\.log(\.1)?|\.crash)$`)

type desktopLog struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	size  int64
	crash *os.File // Its lifetime lock keeps this process's files out of cleanup.
}

func (w *desktopLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(p)
	if len(p) > desktopLogSize {
		const tail = "\n[desktop: log entry truncated]\n"
		clipped := make([]byte, desktopLogSize)
		copy(clipped, p[:desktopLogSize-len(tail)])
		copy(clipped[desktopLogSize-len(tail):], tail)
		p = clipped
	}
	if w.file == nil {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return 0, err
		}
		w.file, w.size = f, info.Size()
	}
	if w.size+int64(len(p)) > desktopLogSize {
		backup := w.path + ".1"
		if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
		err := w.file.Close()
		w.file = nil
		if err != nil {
			return 0, err
		}
		if err := os.Rename(w.path, backup); err != nil {
			return 0, err
		}
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return 0, err
		}
		w.file, w.size = f, 0
	}
	n, err := w.file.Write(p)
	w.size += int64(n)
	if err != nil {
		return n, err
	}
	return original, nil
}

// startDesktopLog keeps bounded diagnostics from a Windows launch without a console.
func startDesktopLog(version string) error {
	dir := filepath.Join(appdir.Cache(), "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, fmt.Sprintf("desktop-%s-%d-*.crash", time.Now().Format("20060102-150405"), os.Getpid()))
	if err != nil {
		return err
	}
	held, err := filelock.TryLock(f)
	if err != nil {
		f.Close()
		return err
	}
	if !held {
		f.Close()
		return errors.New("desktop crash report is in use")
	}
	w := &desktopLog{path: strings.TrimSuffix(f.Name(), ".crash") + ".log", crash: f}
	if _, err := w.Write(nil); err != nil {
		f.Close()
		return err
	}
	if err := setDesktopCrashOutput(f); err != nil {
		w.file.Close()
		f.Close()
		return err
	}
	log.SetOutput(io.MultiWriter(w, log.Writer()))
	if _, err := os.Stderr.Stat(); err != nil {
		r, stderr, err := os.Pipe()
		if err != nil {
			log.Println("desktop stderr:", err)
		} else {
			os.Stderr = stderr
			go func() {
				defer r.Close()
				if _, err := io.Copy(w, r); err != nil {
					log.Println("desktop stderr:", err)
				}
			}()
		}
	}
	if err := cleanDesktopLogs(dir, time.Now()); err != nil {
		log.Println("desktop log cleanup:", err)
	}
	log.Printf("desktop: starting magpie %s (pid %d)", version, os.Getpid())
	return nil
}

func cleanDesktopLogs(dir string, now time.Time) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type savedLog struct {
		path string
		info os.FileInfo
	}
	var files []savedLog
	var firstErr error
	record := func(err error) {
		if err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !desktopLogName.MatchString(name) {
			continue
		}
		base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".1"), ".log"), ".crash")
		f, err := os.OpenFile(filepath.Join(dir, base+".crash"), os.O_RDWR, 0)
		if err == nil {
			held, lockErr := filelock.TryLock(f)
			f.Close()
			record(lockErr)
			if lockErr != nil || !held {
				continue
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			record(err)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			record(err)
			continue
		}
		files = append(files, savedLog{filepath.Join(dir, name), info})
	}
	slices.SortFunc(files, func(a, b savedLog) int {
		return b.info.ModTime().Compare(a.info.ModTime())
	})
	var used int64
	crashes := 0
	for _, f := range files {
		if !strings.HasSuffix(f.path, ".crash") {
			continue
		}
		if f.info.Size() > 0 && crashes < desktopCrashKeep {
			crashes++
			used += f.info.Size()
		} else {
			record(os.Remove(f.path))
		}
	}
	ordinary := files[:0]
	for _, f := range files {
		if strings.HasSuffix(f.path, ".crash") {
			continue
		}
		if now.Sub(f.info.ModTime()) > desktopLogAge {
			err := os.Remove(f.path)
			record(err)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				continue
			}
		}
		used += f.info.Size()
		ordinary = append(ordinary, f)
	}
	for i := len(ordinary) - 1; i >= 0 && used > desktopLogHistory; i-- {
		f := ordinary[i]
		err := os.Remove(f.path)
		record(err)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			used -= f.info.Size()
		}
	}
	return firstErr
}
