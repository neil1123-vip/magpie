//go:build !windows

package recoverystore

import (
	"os"
	"path/filepath"

	"github.com/yetone/magpie/internal/steady"
)

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return os.Chmod(path, 0o700)
}

func replaceFile(from, to string) error {
	if err := steady.Rename(from, to); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(to))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
