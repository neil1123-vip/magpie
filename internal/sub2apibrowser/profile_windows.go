//go:build windows && !nogui && sub2api_webview2

package sub2apibrowser

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"golang.org/x/sys/windows"
)

const profileArg = "--sub2api-profile-cleaner"

func privateProfile() (string, error) {
	root, err := filepath.Abs(os.TempDir())
	if err != nil {
		return "", err
	}
	path, err := os.MkdirTemp(root, "magpie-sub2api-")
	if err != nil {
		return "", err
	}
	if filepath.Dir(path) != root || !strings.HasPrefix(filepath.Base(path), "magpie-sub2api-") {
		return "", fmt.Errorf("Invalid browser profile path")
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err == nil {
		var sd *windows.SECURITY_DESCRIPTOR
		sd, err = windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + user.User.Sid.String() + ")")
		if err == nil {
			var acl *windows.ACL
			acl, _, err = sd.DACL()
			if err == nil {
				err = windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
					windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
					nil, nil, acl, nil)
			}
		}
	}
	if err != nil {
		_ = os.RemoveAll(path)
		return "", err
	}
	return path, nil
}

// The cleaner owns its directory; stdin only signals the parent's lifetime.
// shortcut: helper death can leave a profile; add restart recovery if needed.
func runProfileCleaner() error {
	profile, err := privateProfile()
	if err != nil {
		return err
	}
	writeErr := json.NewEncoder(os.Stdout).Encode(profile)
	if writeErr == nil {
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	// Parent death also closes its browser Job; allow those files to unlock.
	for attempt := 0; ; attempt++ {
		err = os.RemoveAll(profile)
		if err == nil {
			return writeErr
		}
		if attempt == 99 {
			return err
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func startPrivateProfile(exe string) (string, func() error, error) {
	input, lifetime, err := os.Pipe()
	if err != nil {
		return "", nil, err
	}
	output, childOutput, err := os.Pipe()
	if err != nil {
		_ = input.Close()
		_ = lifetime.Close()
		return "", nil, err
	}
	cmd := proc.Command(exe, profileArg)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, childOutput, io.Discard
	err = cmd.Start()
	_ = input.Close()
	_ = childOutput.Close()
	defer output.Close()
	if err != nil {
		_ = lifetime.Close()
		return "", nil, err
	}
	cleanup := func() error {
		_ = lifetime.Close()
		return cmd.Wait()
	}
	var profile string
	if json.NewDecoder(io.LimitReader(output, maxMessage)).Decode(&profile) != nil || profile == "" {
		_ = cleanup()
		return "", nil, fmt.Errorf("Cannot initialize the browser profile cleaner")
	}
	return profile, cleanup, nil
}
