package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/provider"
)

func TestSub2APILocalSecretsNeverEnterPortableBundle(t *testing.T) {
	home(t)
	root := filepath.Join(appdir.Config(), "sub2api-local")
	for _, path := range []string{"secrets/account.dpapi", "accounts/account.json", "browser/profile.dat", "monitor.json"} {
		file := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("LOCAL_PANEL_SECRET_MARKER"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := provider.Provider{ID: "panel", Chat: "https://panel.example/v1", Key: "sk-example",
		Sub2API: &provider.Sub2APIBinding{Session: "portable-reference", GroupID: "g", Version: "portable-1"}}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	for _, keys := range []bool{false, true} {
		bundle, err := Collect(keys, "test")
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "LOCAL_PANEL_SECRET_MARKER") || strings.Contains(string(body), "sub2api-local") {
			t.Fatal("a local panel file entered a portable backup")
		}
		if !strings.Contains(string(body), "portable-reference") {
			t.Fatal("a portable binding reference was lost")
		}
	}
}
