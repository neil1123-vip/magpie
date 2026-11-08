// Package recoverystore keeps local, caller-scoped copies of the readable
// history replaced by native compaction. It never generates a second summary.
package recoverystore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
)

const (
	DefaultMaxBytes  int64 = 512 << 20
	MaxInputBytes          = 32 << 20
	MaxOutputBytes         = 8 << 20
	maxMetadataBytes       = 64 << 10
	maxIndexBytes          = 8 << 20
	maxEntries             = 4096
	maxOperations          = 256
)

var (
	ErrNotFound = errors.New("local recovery copy not found")
	ErrCapacity = errors.New("local recovery storage limit reached")
	ErrStale    = errors.New("local recovery operation was cleared or disabled")
	ErrConflict = errors.New("local recovery checkpoint has conflicting sources")
	ErrCorrupt  = errors.New("local recovery copy is damaged")
)

type Config struct {
	Providers []string `json:"providers"`
	MaxBytes  int64    `json:"maxBytes"`
}

// Store's zero value uses this machine's application directory. Dir allows
// isolated stores without redirecting any other application configuration.
type Store struct{ Dir string }

type Record struct {
	Scope    string          `json:"scope"`
	Hash     string          `json:"hash"`
	Provider string          `json:"provider"`
	Writer   string          `json:"writer"`
	Model    string          `json:"model"`
	Input    json.RawMessage `json:"input"`
	Output   json.RawMessage `json:"output,omitempty"`
	Lossy    bool            `json:"lossy,omitempty"`
	Omitted  int             `json:"omitted,omitempty"`
	Created  time.Time       `json:"created"`

	generation uint64
	id         string
	file       string
}

// Entry deliberately excludes the caller scope and conversation contents.
type Entry struct {
	ID       string    `json:"id"`
	Kind     string    `json:"kind"`
	Hash     string    `json:"hash"`
	Provider string    `json:"provider"`
	Writer   string    `json:"writer"`
	Model    string    `json:"model"`
	Lossy    bool      `json:"lossy,omitempty"`
	Omitted  int       `json:"omitted,omitempty"`
	Created  time.Time `json:"created"`
	Bytes    int64     `json:"bytes"`
	Conflict bool      `json:"conflict,omitempty"`
}

type Status struct {
	Generation    uint64  `json:"generation"`
	Config        Config  `json:"config"`
	Bytes         int64   `json:"bytes"`
	ReservedBytes int64   `json:"reservedBytes"`
	Records       []Entry `json:"records"`
	Pending       int     `json:"pending"`
}

type stored struct {
	Entry
	File        string `json:"file"`
	Fingerprint string `json:"fingerprint"`
}

type operation struct {
	Generation uint64 `json:"generation"`
	Kind       string `json:"kind"`
	File       string `json:"file,omitempty"`
	Reserved   int64  `json:"reserved,omitempty"`
}

type index struct {
	Version    int                  `json:"version"`
	Generation uint64               `json:"generation"`
	Config     Config               `json:"config"`
	Records    map[string]stored    `json:"records"`
	Operations map[string]operation `json:"operations"`
}

func LoadConfig() (Config, error)                     { return Store{}.LoadConfig() }
func SaveConfig(c Config) error                       { return Store{}.SaveConfig(c) }
func Enabled(provider string) (bool, error)           { return Store{}.Enabled(provider) }
func Stats() (Status, error)                          { return Store{}.Stats() }
func Clear() error                                    { return Store{}.Clear() }
func Directory() string                               { return Store{}.Directory() }
func DeleteID(id string) error                        { return Store{}.DeleteID(id) }
func Check(r Record) error                            { return Store{}.Check(r) }
func Begin(r Record) (*Pending, error)                { return Store{}.Begin(r) }
func Load(scope, hash string) (Record, error)         { return Store{}.Load(scope, hash) }
func View(scope, branch, hash string) (Record, error) { return Store{}.View(scope, branch, hash) }
func SaveView(scope, branch, hash string, r Record) error {
	return Store{}.SaveView(scope, branch, hash, r)
}
func Configure(provider string, enabled *bool, maxBytes *int64) error {
	return Store{}.Configure(provider, enabled, maxBytes)
}

// ConfigureChecked runs the provider's identity check under the same lock as
// its local opt-in. The check must not call back into this store.
func ConfigureChecked(provider string, enabled *bool, maxBytes *int64, check func() error) error {
	return Store{}.configure(provider, enabled, maxBytes, check)
}

// GuardProviderRename keeps recovery ownership stable until the caller releases
// the lock. Renaming is refused while either ID has opted in or still owns copies.
func GuardProviderRename(from, to string) (func(), error) {
	return Store{}.GuardProviderRename(from, to)
}

func (s Store) GuardProviderRename(from, to string) (func(), error) {
	unlock, err := s.lock()
	if err != nil {
		return nil, err
	}
	st, err := s.readIndex()
	if err == nil {
		for _, id := range []string{from, to} {
			used := slices.Contains(st.Config.Providers, id)
			for _, item := range st.Records {
				used = used || item.Provider == id
			}
			if used {
				err = fmt.Errorf("cannot change provider ID %q while it has local recovery enabled or saved copies; first disable local recovery and delete its copies, or keep the ID and change only the display name", id)
				break
			}
		}
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (s Store) path(name string) string {
	if s.Dir == "" {
		s.Dir = filepath.Join(appdir.Config(), "compaction-recovery")
	}
	return filepath.Join(s.Dir, name)
}

func (s Store) Directory() string { return s.path("") }

func (s Store) lock() (func(), error) {
	if err := privateDir(s.path("")); err != nil {
		return nil, err
	}
	unlock, held := filelock.Lock(s.path("store.lock"), filelock.Wait)
	if !held {
		return nil, errors.New("cannot lock local recovery storage")
	}
	return unlock, nil
}

func fresh() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func nextGeneration() uint64 {
	var b [8]byte
	rand.Read(b[:])
	return binary.LittleEndian.Uint64(b[:])&((1<<53)-1) | 1
}

func key(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s Store) readIndex() (index, error) {
	st := index{Version: 1, Generation: nextGeneration(), Config: Config{Providers: []string{}, MaxBytes: DefaultMaxBytes},
		Records: map[string]stored{}, Operations: map[string]operation{}}
	b, err := readBounded(s.path("index.json"), maxIndexBytes)
	if errors.Is(err, os.ErrNotExist) {
		return st, s.save(st)
	}
	if err != nil {
		return st, err
	}
	st = index{}
	if err = json.Unmarshal(b, &st); err != nil || st.Version != 1 || st.Generation == 0 || st.Records == nil || st.Operations == nil {
		return st, fmt.Errorf("%w: invalid index", ErrCorrupt)
	}
	if _, err = normalizeConfig(st.Config); err != nil || len(st.Records) > maxEntries || len(st.Operations) > maxOperations {
		return st, fmt.Errorf("%w: invalid index limits", ErrCorrupt)
	}
	for id, item := range st.Records {
		if !validID(id) || !validFile(item.File) || item.ID != id || item.Bytes < 0 || item.Bytes > 2*maxRecordBytes {
			return st, fmt.Errorf("%w: invalid record index", ErrCorrupt)
		}
	}
	for id, op := range st.Operations {
		if !validID(id) || (op.Kind != "write" && op.Kind != "read") || op.Reserved < 0 || op.Reserved > 4*maxRecordBytes || (op.File != "" && !validFile(op.File)) {
			return st, fmt.Errorf("%w: invalid operation index", ErrCorrupt)
		}
	}
	return st, nil
}

func validID(id string) bool {
	_, err := hex.DecodeString(id)
	return err == nil && (len(id) == 32 || len(id) == 64)
}

func validFile(name string) bool {
	return strings.HasSuffix(name, ".gz") && validID(strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".input"))
}

func (s Store) save(st index) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > maxIndexBytes {
		return ErrCapacity
	}
	return writeFile(s.path("index.json"), b)
}

func normalizeConfig(c Config) (Config, error) {
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultMaxBytes
	}
	if c.MaxBytes < 0 || c.MaxBytes > 1<<40 || len(c.Providers) > 1024 {
		return c, errors.New("invalid local recovery storage limit")
	}
	providers := make([]string, 0, len(c.Providers))
	for _, p := range c.Providers {
		if strings.TrimSpace(p) != p || p == "" || len(p) > 1024 {
			return c, errors.New("invalid local recovery provider")
		}
		providers = append(providers, p)
	}
	slices.Sort(providers)
	c.Providers = slices.Compact(providers)
	return c, nil
}

func (s Store) LoadConfig() (Config, error) {
	unlock, err := s.lock()
	if err != nil {
		return Config{}, err
	}
	defer unlock()
	st, err := s.readIndex()
	return st.Config, err
}

func (s Store) Enabled(provider string) (bool, error) {
	c, err := s.LoadConfig()
	return err == nil && slices.Contains(c.Providers, provider), err
}

func (s Store) SaveConfig(c Config) error {
	c, err := normalizeConfig(c)
	if err != nil {
		return err
	}
	return s.changeConfig(func(Config) (Config, error) { return c, nil })
}

// Configure changes only the supplied fields while holding the process-shared
// lock, so a provider toggle cannot overwrite another window's capacity edit.
func (s Store) Configure(provider string, enabled *bool, maxBytes *int64) error {
	return s.configure(provider, enabled, maxBytes, nil)
}

func (s Store) configure(provider string, enabled *bool, maxBytes *int64, check func() error) error {
	return s.changeConfig(func(c Config) (Config, error) {
		if check != nil {
			if err := check(); err != nil {
				return c, err
			}
		}
		if enabled != nil {
			if provider == "" {
				return c, errors.New("a local recovery provider is required")
			}
			c.Providers = slices.DeleteFunc(c.Providers, func(p string) bool { return p == provider })
			if *enabled {
				c.Providers = append(c.Providers, provider)
			}
		}
		if maxBytes != nil {
			c.MaxBytes = *maxBytes
		}
		return normalizeConfig(c)
	})
}

func (s Store) changeConfig(change func(Config) (Config, error)) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.readIndex()
	if err != nil {
		return err
	}
	c, err := change(st.Config)
	if err != nil {
		return err
	}
	if c.MaxBytes == st.Config.MaxBytes && slices.Equal(c.Providers, st.Config.Providers) {
		return nil
	}
	st.Config, st.Generation = c, nextGeneration()
	return s.save(st)
}

func (s Store) Stats() (Status, error) {
	unlock, err := s.lock()
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	st, err := s.readIndex()
	if err != nil {
		return Status{}, err
	}
	if err := s.collect(&st); err != nil {
		return Status{}, err
	}
	status := Status{Generation: st.Generation, Config: st.Config, Records: []Entry{}}
	for _, r := range st.Records {
		status.Bytes += r.Bytes
		status.Records = append(status.Records, r.Entry)
	}
	for _, op := range st.Operations {
		status.ReservedBytes += op.Reserved
		if op.Kind == "write" {
			status.Pending++
		}
	}
	slices.SortFunc(status.Records, func(a, b Entry) int { return b.Created.Compare(a.Created) })
	return status, nil
}

func (s Store) Clear() error { return s.remove("") }

func (s Store) DeleteID(id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	return s.remove(id)
}

func (s Store) remove(id string) error {
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()
	st, err := s.readIndex()
	if err != nil {
		return err
	}
	if id == "" {
		st.Records = map[string]stored{}
	} else {
		if _, ok := st.Records[id]; !ok {
			return ErrNotFound
		}
		delete(st.Records, id)
	}
	st.Generation = nextGeneration()
	if err := s.save(st); err != nil {
		return err
	}
	return s.collect(&st)
}

func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = fmt.Errorf("%w: file exceeds size limit", ErrCorrupt)
	}
	return b, err
}

// writeFile is replaced only by fault-injection tests.
var writeFile = atomicWrite

func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return replaceFile(f.Name(), path)
}
