package sub2api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func setStoredFormat(t *testing.T, m *Manager, path string, encrypted bool, version any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		raw, err = m.unprotect(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	if version == nil {
		delete(object, "formatVersion")
	} else {
		object["formatVersion"], err = json.Marshal(version)
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		raw, err = m.protect(raw)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicFile(path, raw); err != nil {
		t.Fatal(err)
	}
}

func accountFiles(t *testing.T, m *Manager, id string) map[string][]byte {
	t.Helper()
	files := make(map[string][]byte)
	for _, path := range []string{m.recordPath(id), m.secretPath(id), m.passwordPath(id)} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = raw
	}
	return files
}

func TestFutureSessionFormatsStopRequestsAndPreserveFiles(t *testing.T) {
	for _, artifact := range []string{"metadata", "tokens", "password"} {
		for _, action := range []string{"login", "forget-password", "refresh", "read", "logout", "persist"} {
			t.Run(artifact+"/"+action, func(t *testing.T) {
				p := fixture(t)
				m := testManager(t, t.TempDir())
				id := signIn(t, m, p, true).Account.ID
				record, secret, err := m.load(id)
				if err != nil {
					t.Fatal(err)
				}
				path := m.recordPath(id)
				if artifact == "tokens" {
					path = m.secretPath(id)
				} else if artifact == "password" {
					path = m.passwordPath(id)
				}
				setStoredFormat(t, m, path, artifact != "metadata", sessionFormatVersion+1)
				before := accountFiles(t, m, id)
				switch action {
				case "login", "forget-password":
					_, err = m.Login(context.Background(), LoginRequest{
						Base: p.server.URL, Email: "account@example.test", Password: "password-secret",
						Remember: action == "login", Proxy: "direct", Target: "source-1", Revision: "revision-1",
					})
				case "refresh":
					err = m.Refresh(context.Background(), id)
				case "read":
					_, err = m.Read(context.Background(), id, http.MethodGet, "/data", nil)
				case "logout":
					if artifact != "metadata" {
						result, logoutErr := m.LogoutExpected(t.Context(), id, record.View.Generation)
						if logoutErr != nil || !result.CleanupPending || result.CleanupError == "" {
							t.Fatalf("logical logout waited for future secrets: %+v %v", result, logoutErr)
						}
						current, readErr := m.record(id)
						after, fileErr := os.ReadFile(path)
						if readErr != nil || current.View.State != StateLoggedOut ||
							current.View.Generation != record.View.Generation+1 || !current.View.CleanupPending {
							t.Fatalf("logical logout was not durable: %+v %v", current, readErr)
						}
						if fileErr != nil || !bytes.Equal(before[path], after) {
							t.Fatalf("future secret was changed: %v", fileErr)
						}
						return
					}
					err = m.Logout(context.Background(), id)
				case "persist":
					err = m.persist(record, secret)
				}
				var detail *Error
				if !errors.As(err, &detail) || detail.Kind != KindStorage || detail.Code != "UNSUPPORTED_LOCAL_FORMAT" {
					t.Fatalf("future format was not recognized: %v", err)
				}
				if p.logins.Load() != 1 || p.refreshes.Load() != 0 || p.reads.Load() != 0 {
					t.Fatalf("future format sent a request: login=%d refresh=%d read=%d", p.logins.Load(), p.refreshes.Load(), p.reads.Load())
				}
				for path, raw := range accountFiles(t, m, id) {
					if !bytes.Equal(before[path], raw) {
						t.Fatalf("future format changed %s", path)
					}
				}
			})
		}
	}
}

func TestLegacySessionFormatsUpgradeOnLogin(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, true).Account.ID
	for _, path := range []string{m.recordPath(id), m.secretPath(id), m.passwordPath(id)} {
		setStoredFormat(t, m, path, path != m.recordPath(id), nil)
	}
	signIn(t, m, p, true)
	record, secret, err := m.load(id)
	if err != nil {
		t.Fatal(err)
	}
	var password savedPassword
	if err := m.readSecret(m.passwordPath(id), &password); err != nil {
		t.Fatal(err)
	}
	if record.FormatVersion != sessionFormatVersion || secret.FormatVersion != sessionFormatVersion || password.FormatVersion != sessionFormatVersion {
		t.Fatalf("legacy formats were not upgraded: metadata=%d tokens=%d password=%d", record.FormatVersion, secret.FormatVersion, password.FormatVersion)
	}
	if record.View.Version != 2 || secret.Version != 2 {
		t.Fatal("format version replaced the independent token version")
	}
}

func TestSecretWritesAndDeletesRefuseFutureFormats(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, true).Account.ID
	for _, path := range []string{m.secretPath(id), m.passwordPath(id)} {
		setStoredFormat(t, m, path, true, sessionFormatVersion+1)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, action := range []func() error{
			func() error { return m.saveSecret(path, savedPassword{FormatVersion: sessionFormatVersion, ID: id}) },
			func() error { return m.removeSecret(path) },
		} {
			if err := action(); err == nil {
				t.Fatal("future secret was replaced or removed")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("future secret bytes changed: %v", err)
			}
		}
	}
}

func TestLegacyGenerationUpgradesWithoutChangingLifecycle(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, false).Account
	record, secret, err := m.load(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	record.FormatVersion, record.View.Generation = 1, 0
	secret.FormatVersion, secret.Generation = 1, 0
	for path, value := range map[string]any{m.recordPath(first.ID): record, m.secretPath(first.ID): secret} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if path == m.secretPath(first.ID) {
			raw, err = m.protect(raw)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := atomicFile(path, raw); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Refresh(t.Context(), first.ID); err != nil {
		t.Fatal(err)
	}
	record, secret, err = m.load(first.ID)
	if err != nil || record.View.Generation != 1 || secret.Generation != 1 ||
		record.FormatVersion != sessionFormatVersion || secret.FormatVersion != sessionFormatVersion {
		t.Fatalf("legacy lifecycle was not preserved: %+v %+v %v", record, secret, err)
	}
}

func TestLogoutQuarantinesCorruptSecrets(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	first := signIn(t, m, p, true).Account
	originals := map[string][]byte{
		m.secretPath(first.ID):   []byte("corrupt token DPAPI blob"),
		m.passwordPath(first.ID): []byte("corrupt password DPAPI blob"),
	}
	for path, raw := range originals {
		if err := atomicFile(path, raw); err != nil {
			t.Fatal(err)
		}
	}
	result, err := m.LogoutExpected(t.Context(), first.ID, first.Generation)
	if err != nil || result.CleanupPending || result.CleanupError != "" {
		t.Fatalf("unreadable secrets blocked logout cleanup: %+v %v", result, err)
	}
	view, err := m.Info(first.ID)
	if err != nil || view.State != StateLoggedOut || view.Generation != first.Generation+1 {
		t.Fatalf("logical logout was not persisted: %+v %v", view, err)
	}
	kept := map[string]bool{}
	preserved := map[string][]byte{}
	for path, original := range originals {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("corrupt credentials remain active: %v", err)
		}
		paths, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".quarantine-*", filepath.Base(path)))
		if err != nil || len(paths) != 1 || kept[filepath.Dir(paths[0])] {
			t.Fatalf("original did not get its own quarantine: %v %v", paths, err)
		}
		kept[filepath.Dir(paths[0])] = true
		raw, err := os.ReadFile(paths[0])
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatalf("quarantined original changed: %v", err)
		}
		preserved[paths[0]] = original
	}
	signIn(t, m, p, true)
	for path, original := range preserved {
		raw, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(raw, original) {
			t.Fatalf("re-login changed quarantined credentials: %v", err)
		}
	}
}

func TestSecretWritesAndDeletesRefuseCorruptCiphertext(t *testing.T) {
	p := fixture(t)
	m := testManager(t, t.TempDir())
	id := signIn(t, m, p, true).Account.ID
	for _, path := range []string{m.secretPath(id), m.passwordPath(id)} {
		original := []byte("corrupt DPAPI blob")
		if err := atomicFile(path, original); err != nil {
			t.Fatal(err)
		}
		for _, action := range []func() error{
			func() error { return m.saveSecret(path, savedPassword{FormatVersion: sessionFormatVersion, ID: id}) },
			func() error { return m.removeSecret(path) },
		} {
			if err := action(); err == nil {
				t.Fatal("ordinary secret operation replaced or removed corrupt ciphertext")
			}
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, original) {
				t.Fatalf("ordinary secret operation changed the original: %v", err)
			}
		}
	}
	paths, err := filepath.Glob(filepath.Join(m.root, "secrets", ".quarantine-*"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("ordinary secret operation quarantined credentials: %v %v", paths, err)
	}
}
