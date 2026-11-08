package recoverystore

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

const maxRecordBytes = MaxInputBytes + MaxOutputBytes + maxMetadataBytes

type envelope struct {
	Version  int             `json:"version"`
	Checksum string          `json:"checksum"`
	Record   json.RawMessage `json:"record"`
}

// Pending owns an operating-system lock until Commit or Cancel. A crashed
// process releases that lock; a slow upstream does not.
type Pending struct {
	mu         sync.Mutex
	store      Store
	id         string
	kind       string
	target     string
	baseID     string
	baseFile   string
	generation uint64
	release    func()
	closed     bool
	committed  Record
}

func (s Store) Begin(r Record) (*Pending, error) { return s.begin(r, "checkpoint", "") }

func (s Store) begin(r Record, kind, target string) (*Pending, error) {
	if err := validate(r, false); err != nil {
		return nil, err
	}
	r.Input = compact(r.Input)
	r.Output = nil
	if r.Created.IsZero() {
		r.Created = time.Now().UTC()
	}
	b, err := encode(r)
	if err != nil {
		return nil, err
	}
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	st, err := s.readIndex()
	if err == nil {
		err = s.collect(&st)
	}
	if err == nil && (!slices.Contains(st.Config.Providers, r.Provider) || (r.generation != 0 && r.generation != st.Generation)) {
		err = ErrStale
	}
	if err == nil && r.generation != 0 {
		base, ok := st.Records[r.id]
		if !ok || base.File != r.file {
			err = ErrStale
		} else if base.Conflict || (base.Lossy && (!r.Lossy || r.Omitted < base.Omitted)) {
			err = ErrConflict
		}
	}
	// Cover both the prepared input and the largest temporary/final output.
	// gzip overhead is bounded well below this conservative twofold allowance.
	reserved := int64(len(b) + 2*(len(r.Input)+MaxOutputBytes+maxMetadataBytes))
	var occupied int64
	for _, item := range st.Records {
		occupied += item.Bytes
	}
	for _, op := range st.Operations {
		occupied += op.Reserved
	}
	if err == nil && (len(st.Records) >= maxEntries || len(st.Operations) >= maxOperations || reserved > st.Config.MaxBytes-occupied) {
		err = ErrCapacity
	}
	if err != nil {
		unlock()
		return nil, err
	}
	id := fresh()
	release, err := s.newOperation(id)
	if err != nil {
		unlock()
		return nil, err
	}
	p := &Pending{store: s, id: id, kind: kind, target: target, baseID: r.id, baseFile: r.file, generation: st.Generation, release: release}
	st.Operations[id] = operation{Kind: "write", Generation: st.Generation, Reserved: reserved}
	err = s.save(st)
	unlock()
	if err == nil {
		err = writeFile(s.path(id+".input.gz"), b)
	}
	if err == nil {
		err = p.current()
	}
	if err != nil {
		p.Cancel()
		return nil, err
	}
	return p, nil
}

func (p *Pending) current() error {
	unlock, err := p.store.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := p.store.readIndex()
	if err != nil {
		return err
	}
	op, ok := st.Operations[p.id]
	if !ok || st.Generation != p.generation || op.Generation != p.generation {
		return ErrStale
	}
	if p.baseID != "" {
		base, ok := st.Records[p.baseID]
		if !ok || base.File != p.baseFile || base.Conflict {
			return ErrConflict
		}
	}
	return nil
}

// Check validates a prepared operation, or its committed result, immediately
// before the gateway sends upstream or publishes a compaction result.
func (p *Pending) Check() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.committed.id != "" {
		return p.store.Check(p.committed)
	}
	if p.closed {
		return ErrStale
	}
	return p.current()
}

// Commit preserves the prepared Input and its loss information. Only the
// upstream result's Hash, Output, Writer and Model may be supplied afterwards.
func (p *Pending) Commit(result Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrStale
	}
	defer p.finish()
	r, err := decode(p.store.path(p.id + ".input.gz"))
	if err != nil {
		return err
	}
	if (result.Scope != "" && result.Scope != r.Scope) || (result.Provider != "" && result.Provider != r.Provider) ||
		(len(result.Input) > 0 && !bytes.Equal(compact(result.Input), r.Input)) {
		return ErrConflict
	}
	r.Hash, r.Output = result.Hash, compact(result.Output)
	if result.Writer != "" {
		r.Writer = result.Writer
	}
	if result.Model != "" {
		r.Model = result.Model
	}
	if err := validate(r, true); err != nil {
		return err
	}
	b, err := encode(r)
	if err != nil {
		return err
	}
	if err := p.current(); err != nil {
		return err
	}
	if err := writeFile(p.store.path(p.id+".gz"), b); err != nil {
		return err
	}
	unlock, err := p.store.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := p.store.readIndex()
	if err != nil {
		return err
	}
	op, ok := st.Operations[p.id]
	if !ok || st.Generation != p.generation || op.Generation != p.generation || !slices.Contains(st.Config.Providers, r.Provider) {
		return ErrStale
	}
	if p.baseID != "" {
		base, ok := st.Records[p.baseID]
		if !ok || base.File != p.baseFile || base.Conflict {
			return ErrConflict
		}
	}
	id := p.target
	if id == "" {
		id = key(r.Scope, r.Hash)
	}
	fingerprint := signature(r)
	if old, exists := st.Records[id]; exists {
		if old.Fingerprint != fingerprint || old.Conflict {
			if p.kind != "view" {
				old.Conflict = true
				st.Records[id] = old
				if err := p.store.save(st); err != nil {
					return err
				}
				return ErrConflict
			}
			if old.Conflict || p.baseID != id || p.baseFile != old.File {
				return ErrConflict
			}
		} else {
			r.generation, r.id, r.file = st.Generation, id, old.File
			p.committed = r
			return nil
		}
	}
	st.Records[id] = stored{Entry: Entry{ID: id, Kind: p.kind, Hash: r.Hash, Provider: r.Provider,
		Writer: r.Writer, Model: r.Model, Lossy: r.Lossy, Omitted: r.Omitted, Created: r.Created, Bytes: int64(len(b))},
		File: p.id + ".gz", Fingerprint: fingerprint}
	if err := p.store.save(st); err != nil {
		return err
	}
	r.generation, r.id, r.file = st.Generation, id, p.id+".gz"
	p.committed = r
	return nil
}

func (p *Pending) Cancel() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.finish()
}

func (p *Pending) finish() {
	if p.closed {
		return
	}
	p.closed = true
	p.release()
	// Once the operation lock is released, collect can remove its reservation
	// even if this cleanup fails or this process exits here.
	if err := p.store.clean(); err != nil {
		log.Printf("local recovery cleanup: %v", err)
	}
}

func (s Store) Load(scope, hash string) (Record, error) {
	return s.load(key(scope, hash), scope, hash)
}

func (s Store) View(scope, branch, hash string) (Record, error) {
	if branch == "" {
		return Record{}, ErrNotFound
	}
	return s.load(key(scope, branch, hash), scope, hash)
}

// SaveView writes an immutable branch version and atomically advances its
// pointer. Replacing an existing version requires the Record from that View,
// so a concurrent edit or an old checkpoint cannot restore discarded history.
func (s Store) SaveView(scope, branch, hash string, r Record) error {
	if scope == "" || branch == "" || hash == "" || r.Scope != scope || r.Hash != hash || r.generation == 0 {
		return ErrStale
	}
	if err := s.Check(r); err != nil {
		return err
	}
	p, err := s.begin(r, "view", key(scope, branch, hash))
	if err != nil {
		return err
	}
	return p.Commit(r)
}

func (s Store) load(id, scope, hash string) (Record, error) {
	if scope == "" || hash == "" {
		return Record{}, ErrNotFound
	}
	unlock, err := s.lock()
	if err != nil {
		return Record{}, err
	}
	st, err := s.readIndex()
	if err == nil {
		err = s.collect(&st)
	}
	item, ok := st.Records[id]
	if err == nil && !ok {
		err = ErrNotFound
	}
	if err == nil && item.Conflict {
		err = ErrConflict
	}
	if err == nil && !slices.Contains(st.Config.Providers, item.Provider) {
		err = ErrStale
	}
	if err == nil && len(st.Operations) >= maxOperations {
		err = ErrCapacity
	}
	if err != nil {
		unlock()
		return Record{}, err
	}
	opID := fresh()
	release, err := s.newOperation(opID)
	if err != nil {
		unlock()
		return Record{}, err
	}
	st.Operations[opID] = operation{Kind: "read", Generation: st.Generation, File: item.File, Reserved: item.Bytes}
	err = s.save(st)
	unlock()
	defer func() {
		release()
		if err := s.clean(); err != nil {
			log.Printf("local recovery read cleanup: %v", err)
		}
	}()
	if err != nil {
		return Record{}, err
	}
	r, err := decode(s.path(item.File))
	if err != nil {
		return Record{}, err
	}
	if r.Scope != scope || r.Hash != hash || signature(r) != item.Fingerprint {
		return Record{}, ErrCorrupt
	}
	if err := validate(r, true); err != nil {
		return Record{}, err
	}
	r.generation, r.id, r.file = st.Generation, id, item.File
	if err := s.Check(r); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Check detects a clear, removal or configuration change after materializing a
// copy. Call it immediately before publishing or sending the recovered input.
func (s Store) Check(r Record) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.readIndex()
	if err != nil {
		return err
	}
	item, ok := st.Records[r.id]
	if r.generation == 0 || st.Generation != r.generation || !ok || item.File != r.file || !slices.Contains(st.Config.Providers, item.Provider) {
		return ErrStale
	}
	if item.Conflict {
		return ErrConflict
	}
	return nil
}

func validate(r Record, complete bool) error {
	if r.Scope == "" || r.Provider == "" || r.Omitted < 0 || len(r.Scope)+len(r.Provider)+len(r.Writer)+len(r.Model)+len(r.Hash) > maxMetadataBytes {
		return errors.New("invalid local recovery metadata")
	}
	if len(r.Input) > MaxInputBytes || len(r.Output) > MaxOutputBytes {
		return ErrCapacity
	}
	if !array(r.Input) || (complete && (r.Hash == "" || !array(r.Output))) {
		return errors.New("local recovery requires complete JSON input and output arrays")
	}
	return nil
}

func array(b []byte) bool {
	b = bytes.TrimSpace(b)
	return len(b) > 0 && b[0] == '[' && json.Valid(b)
}

func compact(b []byte) json.RawMessage {
	var out bytes.Buffer
	if json.Compact(&out, b) != nil {
		return nil
	}
	return out.Bytes()
}

func signature(r Record) string {
	r.Created = time.Time{}
	b, _ := recordJSON(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func encode(r Record) ([]byte, error) {
	b, err := recordJSON(r)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	b, err = recordJSON(envelope{Version: 1, Checksum: hex.EncodeToString(sum[:]), Record: b})
	if err != nil {
		return nil, err
	}
	if len(b) > maxRecordBytes {
		return nil, ErrCapacity
	}
	var compressed bytes.Buffer
	w := gzip.NewWriter(&compressed)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func recordJSON(value any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte{'\n'}), nil
}

func decode(path string) (Record, error) {
	b, err := readBounded(path, 2*maxRecordBytes)
	if err != nil {
		return Record{}, fmt.Errorf("%w: cannot read payload: %v", ErrCorrupt, err)
	}
	z, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return Record{}, fmt.Errorf("%w: invalid gzip", ErrCorrupt)
	}
	b, err = io.ReadAll(io.LimitReader(z, maxRecordBytes+1))
	closeErr := z.Close()
	if err != nil || closeErr != nil || len(b) > maxRecordBytes {
		return Record{}, fmt.Errorf("%w: invalid or oversized payload", ErrCorrupt)
	}
	var e envelope
	if json.Unmarshal(b, &e) != nil || e.Version != 1 {
		return Record{}, ErrCorrupt
	}
	sum := sha256.Sum256(e.Record)
	if hex.EncodeToString(sum[:]) != e.Checksum {
		return Record{}, ErrCorrupt
	}
	var r Record
	if json.Unmarshal(e.Record, &r) != nil {
		return Record{}, ErrCorrupt
	}
	return r, nil
}

func (s Store) newOperation(id string) (func(), error) {
	f, err := os.OpenFile(s.path(id+".lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	held, err := filelock.TryLock(f)
	if err != nil || !held {
		f.Close()
		return nil, errors.New("cannot protect local recovery operation")
	}
	return func() { filelock.Unlock(f); f.Close() }, nil
}

func (s Store) clean() error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.readIndex()
	if err != nil {
		return err
	}
	return s.collect(&st)
}

// collect runs under store.lock. It never guesses whether an upstream is still
// running from a timestamp: only an obtainable operation lock proves its end.
func (s Store) collect(st *index) error {
	changed := false
	for id := range st.Operations {
		f, err := os.OpenFile(s.path(id+".lock"), os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrNotExist) {
			delete(st.Operations, id)
			changed = true
			continue
		}
		if err != nil {
			return err
		}
		held, err := filelock.TryLock(f)
		if held {
			delete(st.Operations, id)
			changed = true
			filelock.Unlock(f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	if changed {
		if err := s.save(*st); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	for _, item := range st.Records {
		keep[item.File] = true
	}
	for _, op := range st.Operations {
		keep[op.File] = true
	}
	files, err := os.ReadDir(s.path(""))
	if err != nil {
		return err
	}
	for _, file := range files {
		name := file.Name()
		prefix, _, _ := strings.Cut(name, ".")
		_, active := st.Operations[prefix]
		owned := len(prefix) == 32 && validID(prefix) && (name == prefix+".lock" || name == prefix+".gz" || name == prefix+".input.gz" ||
			strings.HasPrefix(name, prefix+".gz.tmp-") || strings.HasPrefix(name, prefix+".input.gz.tmp-"))
		if file.IsDir() || keep[name] || active || (!owned && !strings.HasPrefix(name, "index.json.tmp-")) {
			continue
		}
		if err := os.Remove(s.path(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
