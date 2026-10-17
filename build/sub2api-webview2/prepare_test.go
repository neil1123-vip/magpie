package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOriginalsRejectsChangedDependency(t *testing.T) {
	root := t.TempDir()
	original := []byte("package fixture\n")
	digest := sha256.Sum256(original)
	pinned := originals
	originals = map[string]string{
		"existing.go": hex.EncodeToString(digest[:]),
		"added.go":    "",
	}
	t.Cleanup(func() { originals = pinned })
	path := filepath.Join(root, "existing.go")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	files, err := readOriginals(root)
	if err != nil || string(files["existing.go"]) != string(original) {
		t.Fatalf("unchanged dependency: files=%v err=%v", files, err)
	}
	if err := os.WriteFile(path, []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOriginals(root); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("changed upstream file accepted: %v", err)
	}
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "added.go"), original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOriginals(root); err == nil || !strings.Contains(err.Error(), "expected no upstream") {
		t.Fatalf("unexpected upstream addition accepted: %v", err)
	}
}

func TestTreeDigestChecksTheWholePrivateModule(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"patched.go", "untouched.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package fixture\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before, err := treeDigest(root)
	if err != nil {
		t.Fatal(err)
	}
	copy := filepath.Join(t.TempDir(), "module")
	if err := os.CopyFS(copy, os.DirFS(root)); err != nil {
		t.Fatal(err)
	}
	after, err := treeDigest(copy)
	if err != nil || before != after {
		t.Fatalf("identical private module differs: before=%s after=%s err=%v", before, after, err)
	}
	if err := os.WriteFile(filepath.Join(copy, "untouched.go"), []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	after, err = treeDigest(copy)
	if err != nil || before == after {
		t.Fatalf("modified unpatched file accepted: before=%s after=%s err=%v", before, after, err)
	}
}
