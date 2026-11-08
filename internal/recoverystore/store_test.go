package recoverystore

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

func TestProviderChangesHoldRecoveryLock(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	unlock, err := s.GuardProviderRename("from", "to")
	if err != nil {
		t.Fatal(err)
	}
	other, held := filelock.Lock(s.path("store.lock"), 0)
	other()
	unlock()
	if held {
		t.Fatal("provider rename did not retain the recovery lock")
	}
	on := true
	refused := errors.New("provider changed")
	err = s.configure("to", &on, nil, func() error {
		other, held := filelock.Lock(s.path("store.lock"), 0)
		other()
		if held {
			t.Fatal("provider identity was checked before taking the recovery lock")
		}
		return refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("provider identity failure was lost: %v", err)
	}
	config, err := s.LoadConfig()
	if err != nil || len(config.Providers) != 0 {
		t.Fatalf("failed identity check still enabled copies: %+v %v", config, err)
	}
}

func testStore(t *testing.T) Store {
	t.Helper()
	s := Store{Dir: t.TempDir()}
	if err := s.SaveConfig(Config{Providers: []string{"channel"}, MaxBytes: DefaultMaxBytes}); err != nil {
		t.Fatal(err)
	}
	return s
}

func example() Record {
	return Record{Scope: "caller-a", Provider: "channel", Writer: "account-a", Model: "model-a",
		Input: json.RawMessage(`[{"role":"user","content":"private requirement"},{"type":"function_call","call_id":"tool-1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"tool-1","output":"read contents"}]`),
		Hash:  "ciphertext-a", Output: json.RawMessage(`[{"role":"user","content":"retained"},{"type":"compaction","encrypted_content":"ciphertext-a"}]`)}
}

func put(t *testing.T, s Store, r Record) Record {
	t.Helper()
	p, err := s.Begin(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Cancel)
	if err := p.Commit(r); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(); err != nil {
		t.Fatalf("committed result failed its publication check: %v", err)
	}
	saved, err := s.Load(r.Scope, r.Hash)
	if err != nil {
		t.Fatal(err)
	}
	return saved
}

func TestPrepareCommitRestartAndIsolation(t *testing.T) {
	s := testStore(t)
	r := example()
	p, err := s.Begin(r)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cancel()
	prepared, err := decode(s.path(p.id + ".input.gz"))
	if err != nil || !bytes.Equal(prepared.Input, r.Input) {
		t.Fatalf("Begin returned without a complete persisted input: %v", err)
	}
	if _, err := s.Load(r.Scope, r.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("uncommitted result became readable: %v", err)
	}
	status, err := s.Stats()
	if err != nil || status.Pending != 1 || status.ReservedBytes == 0 || len(status.Records) != 0 {
		t.Fatalf("missing reservation: %+v, %v", status, err)
	}
	if err := p.Commit(r); err != nil {
		t.Fatal(err)
	}
	restarted := Store{Dir: s.Dir}
	saved, err := restarted.Load(r.Scope, r.Hash)
	if err != nil || !bytes.Equal(saved.Input, r.Input) || !bytes.Equal(saved.Output, r.Output) {
		t.Fatalf("restart lost exact history/window: %+v, %v", saved, err)
	}
	if _, err := restarted.Load("caller-b", r.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ciphertext hash disclosed another caller's history: %v", err)
	}
	status, err = restarted.Stats()
	b, _ := json.Marshal(status)
	if err != nil || status.Pending != 0 || status.ReservedBytes != 0 || len(status.Records) != 1 || status.Bytes == 0 {
		t.Fatalf("wrong committed accounting: %+v, %v", status, err)
	}
	for _, secret := range []string{"private requirement", "read contents", "caller-a", "encrypted_content"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("Stats disclosed %q", secret)
		}
	}
}

func TestEmptyStoreGenerationIsStableAcrossProcesses(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	first, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	second, err := (Store{Dir: s.Dir}).Stats()
	if err != nil || first.Generation == 0 || first.Generation != second.Generation {
		t.Fatalf("unconfigured store changed identity: %d -> %d, %v", first.Generation, second.Generation, err)
	}
}

func TestClearDisableAndCancelInvalidateOperations(t *testing.T) {
	for _, action := range []string{"clear", "disable", "delete"} {
		t.Run(action, func(t *testing.T) {
			s := testStore(t)
			r := put(t, s, example())
			pending := example()
			pending.Hash = "next"
			p, err := s.Begin(pending)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Cancel()
			switch action {
			case "clear":
				err = s.Clear()
			case "disable":
				enabled := false
				err = s.Configure("channel", &enabled, nil)
			case "delete":
				err = s.DeleteID(r.id)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Check(r); !errors.Is(err, ErrStale) {
				t.Fatalf("old reader survived %s: %v", action, err)
			}
			if err := p.Commit(pending); !errors.Is(err, ErrStale) {
				t.Fatalf("pending wrote back after %s: %v", action, err)
			}
			if _, err := s.Load(pending.Scope, pending.Hash); !errors.Is(err, ErrNotFound) {
				t.Fatalf("stale result published: %v", err)
			}
		})
	}
	s := testStore(t)
	r := example()
	p, err := s.Begin(r)
	if err != nil {
		t.Fatal(err)
	}
	p.Cancel()
	if err := p.Commit(r); !errors.Is(err, ErrStale) {
		t.Fatalf("cancelled operation committed: %v", err)
	}
}

func TestPreparedInputAndCheckpointAreImmutable(t *testing.T) {
	s := testStore(t)
	r := example()
	p, err := s.Begin(r)
	if err != nil {
		t.Fatal(err)
	}
	changed := r
	changed.Input = json.RawMessage(`[{"role":"user","content":"different source"}]`)
	if err := p.Commit(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("Commit changed the prepared input: %v", err)
	}
	first := put(t, s, r)
	put(t, s, r)
	p, err = s.Begin(changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second source reused the same hash: %v", err)
	}
	if _, err := s.Load(r.Scope, r.Hash); !errors.Is(err, ErrConflict) {
		t.Fatalf("ambiguous source remained recoverable: %v", err)
	}
	if err := s.Check(first); !errors.Is(err, ErrConflict) {
		t.Fatalf("reader ignored newly discovered ambiguity: %v", err)
	}
}

func TestPayloadPreservesHTMLCharactersAndChecksums(t *testing.T) {
	s := testStore(t)
	r := example()
	r.Input = json.RawMessage(`[{"role":"user","content":"<tag> & value > 2"}]`)
	saved := put(t, s, r)
	if !bytes.Equal(saved.Input, r.Input) {
		t.Fatalf("storage rewrote the exact input: %s", saved.Input)
	}
	b, err := encode(saved)
	if err != nil {
		t.Fatal(err)
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(z)
	z.Close()
	if err != nil {
		t.Fatal(err)
	}
	decoded = bytes.Replace(decoded, []byte("<tag>"), []byte("<bad>"), 1)
	var corrupt bytes.Buffer
	w := gzip.NewWriter(&corrupt)
	w.Write(decoded)
	w.Close()
	if err := os.WriteFile(s.path(saved.file), corrupt.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r.Scope, r.Hash); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("valid gzip with modified history passed its checksum: %v", err)
	}
}

func TestViewsRemainScopedAndLossyDescendantsAreMaterialized(t *testing.T) {
	s := testStore(t)
	r := put(t, s, example())
	r.Input = json.RawMessage(`[{"role":"user","content":"selected history"}]`)
	r.Lossy, r.Omitted = true, 4
	if err := s.SaveView(r.Scope, "branch-a", r.Hash, r); err != nil {
		t.Fatal(err)
	}
	view, err := s.View(r.Scope, "branch-a", r.Hash)
	if err != nil || !view.Lossy || view.Omitted != 4 || !bytes.Equal(view.Input, r.Input) {
		t.Fatalf("lost recovery view: %+v, %v", view, err)
	}
	if _, err := s.View(r.Scope, "branch-b", r.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("view leaked to sibling: %v", err)
	}
	if err := s.SaveView(r.Scope, "", r.Hash, r); !errors.Is(err, ErrStale) {
		t.Fatalf("anonymous branch acquired state: %v", err)
	}
	view.Hash = "ciphertext-b"
	view.Input = json.RawMessage(`[{"role":"user","content":"selected history"},{"role":"user","content":"new suffix"}]`)
	p, err := s.Begin(view)
	if err != nil {
		t.Fatal(err)
	}
	result := view
	result.Lossy, result.Omitted = false, 0
	if err := p.Commit(result); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteID(key(r.Scope, r.Hash)); err != nil {
		t.Fatal(err)
	}
	descendant, err := s.Load(view.Scope, view.Hash)
	if err != nil || !bytes.Equal(descendant.Input, view.Input) || !descendant.Lossy || descendant.Omitted != 4 {
		t.Fatalf("descendant depended on deleted parent or forgot loss: %+v, %v", descendant, err)
	}
}

func TestViewReplacementRejectsConcurrentAndOriginalVersions(t *testing.T) {
	s := testStore(t)
	original := put(t, s, example())
	if err := s.SaveView(original.Scope, "branch", original.Hash, original); err != nil {
		t.Fatal(err)
	}
	current, err := s.View(original.Scope, "branch", original.Hash)
	if err != nil {
		t.Fatal(err)
	}
	trimmed := current
	trimmed.Input, trimmed.Lossy, trimmed.Omitted = json.RawMessage(`[{"role":"user","content":"selected"}]`), true, 3
	concurrent := trimmed
	concurrent.Input = json.RawMessage(`[{"role":"user","content":"concurrent selection"}]`)
	p, err := s.begin(concurrent, "view", key(original.Scope, "branch", original.Hash))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Cancel()
	if err := s.SaveView(original.Scope, "branch", original.Hash, trimmed); err != nil {
		t.Fatal(err)
	}
	if err := p.Commit(concurrent); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent version overwrote the view: %v", err)
	}
	if err := s.SaveView(original.Scope, "branch", original.Hash, original); !errors.Is(err, ErrConflict) {
		t.Fatalf("original checkpoint restored discarded history: %v", err)
	}
	if err := s.Check(current); !errors.Is(err, ErrStale) {
		t.Fatalf("obsolete reader still passed publication check: %v", err)
	}
	latest, err := s.View(original.Scope, "branch", original.Hash)
	if err != nil || !latest.Lossy || latest.Omitted != 3 || string(latest.Input) != `[{"role":"user","content":"selected"}]` {
		t.Fatalf("current view was poisoned by rejected write: %+v, %v", latest, err)
	}
	latest.Input, latest.Omitted = json.RawMessage(`[]`), 5
	if err := s.SaveView(latest.Scope, "branch", latest.Hash, latest); err != nil {
		t.Fatalf("current view could not be shortened again: %v", err)
	}
}

func TestStorageFailuresNeverPublishAndFreeReservations(t *testing.T) {
	for _, phase := range []string{"prepare", "payload", "publish"} {
		t.Run(phase, func(t *testing.T) {
			s := testStore(t)
			r := example()
			original := writeFile
			defer func() { writeFile = original }()
			failure := errors.New("injected disk full")
			writeFile = func(path string, b []byte) error {
				if phase == "prepare" && strings.HasSuffix(path, ".input.gz") {
					return failure
				}
				if phase == "payload" && strings.HasSuffix(path, ".gz") && !strings.HasSuffix(path, ".input.gz") {
					return failure
				}
				if phase == "publish" && filepath.Base(path) == "index.json" {
					var st index
					if json.Unmarshal(b, &st) == nil && len(st.Records) > 0 {
						return failure
					}
				}
				return original(path, b)
			}
			p, err := s.Begin(r)
			if err == nil {
				err = p.Commit(r)
			}
			if !errors.Is(err, failure) {
				t.Fatalf("%s failure ignored: %v", phase, err)
			}
			writeFile = original
			if _, err := s.Load(r.Scope, r.Hash); !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s failure published result: %v", phase, err)
			}
			status, err := s.Stats()
			if err != nil || status.Pending != 0 || status.ReservedBytes != 0 {
				t.Fatalf("%s leaked reservation: %+v, %v", phase, status, err)
			}
		})
	}
}

func TestClearWhileWritingInvalidatesBeforePublish(t *testing.T) {
	for _, phase := range []string{"prepare", "commit"} {
		t.Run(phase, func(t *testing.T) {
			s := testStore(t)
			r := example()
			original := writeFile
			defer func() { writeFile = original }()
			writeFile = func(path string, b []byte) error {
				if err := original(path, b); err != nil {
					return err
				}
				input := strings.HasSuffix(path, ".input.gz")
				if strings.HasSuffix(path, ".gz") && ((phase == "prepare" && input) || (phase == "commit" && !input)) {
					return s.Clear()
				}
				return nil
			}
			p, err := s.Begin(r)
			if err == nil {
				err = p.Commit(r)
			}
			if !errors.Is(err, ErrStale) {
				t.Fatalf("clear during %s did not stop publishing: %v", phase, err)
			}
		})
	}
}

func TestReaderProtectsPayloadUntilMaterialized(t *testing.T) {
	s := testStore(t)
	r := put(t, s, example())
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.readIndex()
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	id := fresh()
	release, err := s.newOperation(id)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	st.Operations[id] = operation{Kind: "read", Generation: st.Generation, File: r.file}
	err = s.save(st)
	unlock()
	if err != nil {
		release()
		t.Fatal(err)
	}
	if err := s.Clear(); err != nil {
		release()
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path(r.file)); err != nil {
		release()
		t.Fatalf("clear removed a protected reader's file: %v", err)
	}
	if err := s.Check(r); !errors.Is(err, ErrStale) {
		release()
		t.Fatalf("clear did not invalidate the reader: %v", err)
	}
	release()
	if _, err := s.Stats(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path(r.file)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unprotected payload was never collected: %v", err)
	}
}

func TestDamagedAndOversizedDataFailClosed(t *testing.T) {
	s := testStore(t)
	r := put(t, s, example())
	if err := os.WriteFile(s.path(r.file), []byte("broken gzip"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(r.Scope, r.Hash); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt copy recovered: %v", err)
	}
	var bomb bytes.Buffer
	z := gzip.NewWriter(&bomb)
	if _, err := io.CopyN(z, strings.NewReader(strings.Repeat("x", maxRecordBytes+1)), maxRecordBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	path := s.path("oversize.gz")
	if err := os.WriteFile(path, bomb.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := decode(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("unbounded decompression accepted: %v", err)
	}
	if err := os.WriteFile(s.path("index.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enabled("channel"); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged index was treated as empty: %v", err)
	}
	if err := s.SaveConfig(Config{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("configuration overwrite hid corrupt index: %v", err)
	}
}

func child(t *testing.T, mode, dir, provider string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	cmd := proc.CommandContext(ctx, exe, "-test.run=^TestRecoveryProcessHelper$", "--", mode, dir, provider)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child did not reach synchronization point: %q, %v", line, err)
	}
	return cmd, stdin
}

func TestProcessReservationSurvivesDelayAndIsReclaimedAfterCrash(t *testing.T) {
	s := testStore(t)
	limit := int64(24 << 20)
	if err := s.Configure("", nil, &limit); err != nil {
		t.Fatal(err)
	}
	cmd, _ := child(t, "hold", s.Dir, "")
	status, err := s.Stats()
	if err != nil || status.Pending != 1 || status.ReservedBytes == 0 {
		t.Fatalf("other process did not reserve capacity: %+v, %v", status, err)
	}
	files, _ := filepath.Glob(s.path("*.lock"))
	old := time.Now().Add(-365 * 24 * time.Hour)
	for _, file := range files {
		if err := os.Chtimes(file, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if p, err := s.Begin(example()); !errors.Is(err, ErrCapacity) {
		if p != nil {
			p.Cancel()
		}
		t.Fatalf("active operation was collected by age or oversubscribed: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	status, err = s.Stats()
	if err != nil || status.Pending != 0 || status.ReservedBytes != 0 {
		t.Fatalf("dead process kept its reservation: %+v, %v", status, err)
	}
	p, err := s.Begin(example())
	if err != nil {
		t.Fatalf("crashed capacity was not reusable: %v", err)
	}
	p.Cancel()
}

func TestConcurrentProcessesConfigureWithoutLostUpdates(t *testing.T) {
	s := testStore(t)
	a, gateA := child(t, "configure", s.Dir, "provider-a")
	b, gateB := child(t, "configure", s.Dir, "provider-b")
	gateA.Close()
	gateB.Close()
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadConfig()
	if err != nil || !slices.Equal(c.Providers, []string{"channel", "provider-a", "provider-b"}) {
		t.Fatalf("concurrent edits lost a provider: %+v, %v", c, err)
	}
}

func TestRecoveryProcessHelper(t *testing.T) {
	args := os.Args
	i := slices.Index(args, "--")
	if i < 0 {
		return
	}
	args = args[i+1:]
	if len(args) != 3 {
		t.Fatal("wrong child arguments")
	}
	s := Store{Dir: args[1]}
	if args[0] == "hold" {
		if _, err := s.Begin(example()); err != nil {
			t.Fatal(err)
		}
	}
	fmt.Fprintln(os.Stdout, "ready")
	io.Copy(io.Discard, os.Stdin)
	if args[0] == "configure" {
		enabled := true
		if err := s.Configure(args[2], &enabled, nil); err != nil {
			t.Fatal(err)
		}
	}
}
