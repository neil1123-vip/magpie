package provider

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func storeFixture(t *testing.T, f file) error {
	t.Helper()
	current, err := read()
	if err != nil {
		return err
	}
	f.baseline = current.baseline
	return store(f)
}

func TestProviderConfigRejectsStaleWrite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	first, err := read()
	if err != nil {
		t.Fatal(err)
	}
	second := first
	first.Providers = []Provider{{ID: "first"}}
	if err := store(first); err != nil {
		t.Fatal(err)
	}
	second.Providers = []Provider{{ID: "stale"}}
	if err := store(second); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale absent baseline: %v", err)
	}
	current, err := read()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), append(utf8BOM, []byte("{\"providers\":[{\"id\":\"other\"}]}\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store(current); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("external raw-byte change: %v", err)
	}
	if err := store(file{}); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("missing baseline: %v", err)
	}
}

func TestProviderSaveRevisionRejectsOldEditor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", PriceRate: 1}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	opened, err := Find("relay")
	if err != nil {
		t.Fatal(err)
	}
	p.PriceRate = 2
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	opened.Name = "Old editor"
	if err := SaveWithRevision(*opened, opened.SaveRevision); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("stale form: %v", err)
	}
	current, _ := Find("relay")
	if current.PriceRate != 2 || current.Name != "Relay" {
		t.Fatalf("stale form changed the provider: %+v", current)
	}
	if err := SaveWithRevision(*current, ""); !errors.Is(err, ErrConfigConflict) {
		t.Fatalf("missing revision: %v", err)
	}
	current.Name = "Current editor"
	if err := SaveWithRevision(*current, current.SaveRevision); err != nil {
		t.Fatalf("current form: %v", err)
	}
}

func TestProviderSaveRevisionKeepsNewAccountModels(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear=%v", clear), func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			ref := KeyID("k")
			p := Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", Models: []string{"one", "two"}}
			if clear {
				p.AccountModels = map[string][]string{ref: {"one"}}
			}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			opened, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			var allow []string
			if !clear {
				allow = []string{"two"}
			}
			if err := SetAccountModels(p.ID, ref, allow); err != nil {
				t.Fatal(err)
			}
			opened.Name = "Edited"
			if err := SaveWithRevision(*opened, opened.SaveRevision); err != nil {
				t.Fatalf("saving the form after its account-model picks: %v", err)
			}
			current, err := Find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Name != "Edited" || current.AccountServes(ref, "one") != clear || !current.AccountServes(ref, "two") {
				t.Fatalf("the form lost its edits or restored old model picks: %+v", current)
			}
		})
	}
}

func TestProviderConfigWriterProcess(t *testing.T) {
	shared := os.Getenv("MAGPIE_CONFIG_CAS_SHARED")
	if shared == "" {
		t.Skip("run by TestProviderConfigTwoProcesses")
	}
	t.Setenv("XDG_CONFIG_HOME", shared)
	f, err := read()
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("config-cas:ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	f.Providers[0].Name = os.Getenv("MAGPIE_CONFIG_CAS_WRITER")
	switch err := store(f); {
	case err == nil:
		fmt.Println("config-cas:saved")
	case errors.Is(err, ErrConfigConflict):
		fmt.Println("config-cas:conflict")
	default:
		t.Fatal(err)
	}
}

func TestProviderConfigTwoProcesses(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", shared)
	if err := Save(Provider{ID: "relay", Name: "Original", Key: "k", Chat: "http://127.0.0.1:1/v1"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	type writer struct {
		cmd    *exec.Cmd
		lines  *bufio.Scanner
		input  func()
		stderr bytes.Buffer
	}
	writers := make([]*writer, 2)
	for i := range writers {
		w := &writer{}
		w.cmd = exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProviderConfigWriterProcess$", "-test.count=1")
		w.cmd.Env = append(os.Environ(), "MAGPIE_CONFIG_CAS_SHARED="+shared, fmt.Sprintf("MAGPIE_CONFIG_CAS_WRITER=writer-%d", i))
		stdout, err := w.cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdin, err := w.cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		w.lines = bufio.NewScanner(stdout)
		w.cmd.Stderr = &w.stderr
		w.input = func() {
			if _, err := fmt.Fprintln(stdin, "write"); err != nil {
				t.Error(err)
			}
			stdin.Close()
		}
		if err := w.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if w.cmd.ProcessState == nil {
				w.cmd.Process.Kill()
				w.cmd.Wait()
			}
		})
		if !w.lines.Scan() || w.lines.Text() != "config-cas:ready" {
			t.Fatalf("writer did not read the baseline: %q %s", w.lines.Text(), w.stderr.String())
		}
		writers[i] = w
	}
	for _, w := range writers {
		w.input()
	}
	saved, conflicted := 0, 0
	for _, w := range writers {
		for w.lines.Scan() {
			switch w.lines.Text() {
			case "config-cas:saved":
				saved++
			case "config-cas:conflict":
				conflicted++
			}
		}
		if err := w.cmd.Wait(); err != nil {
			t.Fatalf("writer: %v %s", err, w.stderr.String())
		}
	}
	if saved != 1 || conflicted != 1 {
		t.Fatalf("saved=%d conflicted=%d; want one of each", saved, conflicted)
	}
	f, err := read()
	if err != nil || len(f.Providers) != 1 || !strings.HasPrefix(f.Providers[0].Name, "writer-") {
		t.Fatalf("winning write lost: %+v, %v", f.Providers, err)
	}
	if f.baseline.path != filepath.Join(shared, "magpie", "providers.json") {
		t.Fatalf("unexpected shared file: %s", f.baseline.path)
	}
}
