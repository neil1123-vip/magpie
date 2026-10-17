// Command sub2api-webview2 prepares a private, versioned Wails module for the
// isolated Windows login host. It never changes the shared module cache.
package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yetone/magpie/internal/proc"
)

const (
	wailsModule  = "github.com/wailsapp/wails/v3"
	wailsVersion = "v3.0.0-beta.24"
)

//go:embed wails-native-hooks.patch
var patch []byte

var originals = map[string]string{
	"pkg/application/application.go":             "13f80791710adb366de90e375ba9df84b5da609ff2c0facd387d2dca63ebf5a8",
	"pkg/application/application_options.go":     "ce44225734a67dafd7937b6c1089e1a7ff6c36582d4947a0e2e42f82362de6dc",
	"pkg/application/webview_window_options.go":  "153b19915e180d6f4d664539e76ef3e52e64a0995bd33d09919e5a3bc62a4eb6",
	"pkg/application/webview_window_windows.go":  "2095ae41043d1097da400244f55d0a110edd0109560928f6fbd207756dfda9e1",
	"internal/webview2/pkg/edge/chromium.go":     "7605f17481c25b5ab1e3f7561b763fd0a7fb3b30858c178fc33cace7d1cd6ac2",
	"internal/webview2/pkg/edge/native_hooks.go": "",
}

type module struct {
	Version string
	Dir     string
	Replace *module
	Error   *struct{ Err string }
}

func main() {
	path, err := prepare()
	if err != nil {
		fmt.Fprintln(os.Stderr, "Sub2API WebView2:", err)
		os.Exit(1)
	}
	fmt.Println(filepath.ToSlash(path))
}

func prepare() (string, error) {
	root, err := os.Getwd()
	if err != nil {
		return "", err
	}
	manifest, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil || !strings.HasPrefix(strings.ReplaceAll(string(manifest), "\r\n", "\n"), "module github.com/yetone/magpie\n") {
		return "", fmt.Errorf("run from magpie's repository root")
	}
	goTool := filepath.Join(runtime.GOROOT(), "bin", "go")
	gitTool := "git"
	if runtime.GOOS == "windows" {
		goTool += ".exe"
		gitTool = `C:\Program Files\Git\cmd\git.exe`
	}
	var dependency module
	if err := readModule(goTool, root, &dependency, "list", "-m", "-json", wailsModule); err != nil {
		return "", err
	}
	if dependency.Version != wailsVersion || dependency.Replace != nil {
		return "", fmt.Errorf("requires unmodified %s %s; review the patch before upgrading", wailsModule, wailsVersion)
	}
	if dependency.Dir == "" {
		if err := readModule(goTool, root, &dependency, "mod", "download", "-json", wailsModule+"@"+wailsVersion); err != nil {
			return "", err
		}
	}
	if dependency.Dir == "" {
		return "", fmt.Errorf("Wails module directory unavailable")
	}
	if _, err := readOriginals(dependency.Dir); err != nil {
		return "", err
	}
	output := filepath.Join(root, ".tmp-sub2api-wails")
	if err := os.MkdirAll(output, 0700); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(output, "patch-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	if err := os.CopyFS(stage, os.DirFS(dependency.Dir)); err != nil {
		return "", fmt.Errorf("copy private Wails module: %w", err)
	}
	stageRelative, err := filepath.Rel(root, stage)
	if err != nil {
		return "", err
	}
	patchData := bytes.ReplaceAll(patch, []byte("\r\n"), []byte("\n"))
	for _, check := range []bool{true, false} {
		args := []string{"-c", "core.autocrlf=false", "apply", "--directory=" + filepath.ToSlash(stageRelative), "--whitespace=error"}
		if check {
			args = append(args, "--check")
		}
		command := proc.Command(gitTool, args...)
		command.Dir = root
		command.Stdin = bytes.NewReader(patchData)
		if out, err := command.CombinedOutput(); err != nil {
			return "", fmt.Errorf("apply pinned Wails patch: %w\n%s", err, out)
		}
	}
	digest, err := treeDigest(stage)
	if err != nil {
		return "", err
	}
	patched := filepath.Join(output, digest)
	if err := os.Rename(stage, patched); err != nil {
		existing, readErr := treeDigest(patched)
		if readErr != nil || existing != digest {
			return "", fmt.Errorf("publish private Wails module: %w", err)
		}
	}
	relative, err := filepath.Rel(root, patched)
	if err != nil {
		return "", err
	}
	modfile := filepath.Join(output, "go.mod")
	temporary, err := os.CreateTemp(output, "go-*.mod")
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name())
	_, writeErr := temporary.Write(manifest)
	closeErr := temporary.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	command := proc.Command(goTool, "mod", "edit", "-modfile="+temporary.Name(), "-replace="+wailsModule+"=./"+filepath.ToSlash(relative))
	command.Dir = root
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	if out, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("write private Wails replacement: %w\n%s", err, out)
	}
	sums, err := os.ReadFile(filepath.Join(root, "go.sum"))
	if err != nil {
		return "", err
	}
	sumTemporary := strings.TrimSuffix(temporary.Name(), ".mod") + ".sum"
	defer os.Remove(sumTemporary)
	if err := os.WriteFile(sumTemporary, sums, 0600); err != nil {
		return "", err
	}
	if err := os.Rename(sumTemporary, filepath.Join(output, "go.sum")); err != nil {
		return "", err
	}
	if err := os.Rename(temporary.Name(), modfile); err != nil {
		return "", err
	}
	return modfile, nil
}

func readModule(goTool, root string, dependency *module, args ...string) error {
	command := proc.Command(goTool, args...)
	command.Dir = root
	command.Env = append(os.Environ(), "GOFLAGS=", "GOWORK=off")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return fmt.Errorf("read Wails module: %w\n%s", err, stderr.Bytes())
	}
	if err := json.Unmarshal(out, dependency); err != nil {
		return fmt.Errorf("read Wails module: %w", err)
	}
	if dependency.Error != nil {
		return fmt.Errorf("read Wails module: %s", dependency.Error.Err)
	}
	return nil
}

func treeDigest(root string) (string, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return "", fmt.Errorf("read private Wails module: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("private Wails module is not a directory: %s", root)
	}
	tree := os.DirFS(root)
	digest := sha256.New()
	err = fs.WalkDir(tree, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("private Wails module has non-regular file: %s", path)
		}
		data, err := fs.ReadFile(tree, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		digest.Write([]byte(path))
		digest.Write([]byte{0})
		digest.Write(sum[:])
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("verify private Wails module: %w", err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func readOriginals(root string) (map[string][]byte, error) {
	files := make(map[string][]byte, len(originals))
	for path, expected := range originals {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if expected == "" {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("expected no upstream %s; review the patch", path)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read upstream %s: %w", path, err)
		}
		actual := sha256.Sum256(data)
		if hex.EncodeToString(actual[:]) != expected {
			return nil, fmt.Errorf("upstream %s changed; review the patch", path)
		}
		files[path] = data
	}
	return files, nil
}
