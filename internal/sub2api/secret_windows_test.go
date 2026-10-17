package sub2api

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsSecretRoundTrip(t *testing.T) {
	plain := []byte(`{"access_token":"access-secret","refresh_token":"refresh-secret"}`)
	cipher, err := protectSecret(plain)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(cipher, plain) || bytes.Contains(cipher, []byte("refresh-secret")) {
		t.Fatal("DPAPI output contains cleartext credentials")
	}
	got, err := unprotectSecret(cipher)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("DPAPI round trip failed: %v", err)
	}
	cipher[len(cipher)-1] ^= 1
	if _, err := unprotectSecret(cipher); err == nil {
		t.Fatal("tampered DPAPI data was accepted")
	}
}

func TestCorruptSecretQuarantineFailureRemainsPending(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	path := m.secretPath(first.ID)
	original := []byte("corrupt DPAPI blob")
	if err := atomicFile(path, original); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			windows.CloseHandle(handle)
		}
	}()
	result, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	if err != nil || !result.CleanupPending || result.CleanupError == "" {
		t.Fatalf("quarantine failure lost pending cleanup: %+v %v", result, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, original) {
		t.Fatalf("quarantine failure changed the active original: %v", err)
	}
	paths, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".quarantine-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("failed quarantine left a placeholder: %v %v", paths, err)
	}
	record, err := m.record(first.ID)
	if err != nil || record.View.State != StateLoggedOut || !record.View.CleanupPending ||
		record.View.Generation != result.Generation {
		t.Fatalf("quarantine failure undid logical logout: %+v %v", record, err)
	}
	if err := windows.CloseHandle(handle); err != nil {
		t.Fatal(err)
	}
	closed = true
	result, err = m.LogoutExpected(t.Context(), first.ID, result.Generation)
	if err != nil || result.CleanupPending || result.CleanupError != "" {
		t.Fatalf("released original could not be quarantined: %+v %v", result, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful retry left active damaged credentials: %v", err)
	}
	paths, err = filepath.Glob(filepath.Join(filepath.Dir(path), ".quarantine-*", filepath.Base(path)))
	if err != nil || len(paths) != 1 {
		t.Fatalf("successful retry did not preserve the original: %v %v", paths, err)
	}
	if raw, err := os.ReadFile(paths[0]); err != nil || !bytes.Equal(raw, original) {
		t.Fatalf("successful retry changed the original: %v", err)
	}
}
