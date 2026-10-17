package sub2api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

const pollInterval = time.Minute

type monitorRound uint8

const (
	scheduledRound monitorRound = iota
	requestedRound
	wakeRound
)

type monitorConfig struct {
	Enabled  bool    `json:"enabled"`
	Groups   []Group `json:"groups"`
	Revision string  `json:"revision"`
}

type monitorSettingsDisk struct {
	Version  int                      `json:"version"`
	Accounts map[string]monitorConfig `json:"accounts"`
}

type cooldown struct {
	Until       time.Time     `json:"until"`
	Failures    int           `json:"failures"`
	Generation  uint64        `json:"generation,omitempty"`
	NeedsAction bool          `json:"needsAction,omitempty"`
	Status      int           `json:"status,omitempty"`
	Challenge   bool          `json:"challenge,omitempty"`
	RetryAfter  time.Duration `json:"retryAfter,omitempty"`
}

func (c cooldown) err(now time.Time, generation uint64) error {
	recovered := c.Generation != 0 && generation > c.Generation &&
		(c.NeedsAction || c.Status == 401 || c.Challenge) && c.Status != 429 && c.RetryAfter == 0
	if !now.Before(c.Until) || recovered {
		return nil
	}
	kind := KindTemporary
	if c.NeedsAction {
		kind = KindAuth
	}
	return &Error{Kind: kind, Status: c.Status, Challenge: c.Challenge, RetryAfter: c.Until.Sub(now)}
}

type monitorDisk struct {
	Version   int                        `json:"version"`
	Snapshot  Snapshot                   `json:"snapshot"`
	Cooldowns map[string]cooldown        `json:"cooldowns,omitempty"`
	Baselines map[string]monitorBaseline `json:"baselines,omitempty"`
}

type monitorBaseline struct {
	Identity    string                   `json:"identity"`
	GroupsKnown bool                     `json:"groupsKnown"`
	Groups      map[string]groupBaseline `json:"groups"`
	Keys        map[string]keyBaseline   `json:"keys"`
}

type groupBaseline struct {
	Name      string    `json:"name"`
	Visible   *bool     `json:"visible,omitempty"`
	VisibleAt time.Time `json:"visibleAt,omitempty"`
	Identity  string    `json:"identity,omitempty"`
	Rate      *float64  `json:"rate,omitempty"`
	RateAt    time.Time `json:"rateAt,omitempty"`
	Enabled   *bool     `json:"enabled,omitempty"`
	EnabledAt time.Time `json:"enabledAt,omitempty"`
}

type keyBaseline struct {
	Identity string    `json:"identity"`
	Rate     *float64  `json:"rate,omitempty"`
	At       time.Time `json:"at,omitempty"`
}

type modeCache struct {
	mode  string
	until time.Time
	err   error
}

// Monitor owns local observations only. It never changes prices, keys, or bindings.
type Monitor struct {
	mu              sync.Mutex
	dir             string
	panel           PanelRead
	raw             PanelRead
	billing         BillingRead
	targets         func() []Target
	targetReader    func() ([]Target, error)
	generationGuard func(string, uint64, func() error) error
	cancels         map[string]context.CancelFunc
	notify          func(Change)
	flight          chan struct{}
	wake            chan struct{}
	woken           map[string]bool
	lastErr         error
	snapshot        Snapshot
	configs         map[string]monitorConfig
	cool            map[string]cooldown
	modes           map[string]modeCache
	baselines       map[string]monitorBaseline
	running         bool
	sem             chan struct{}
	now             func() time.Time
}

func NewMonitor(dir string, panel PanelRead, billing BillingRead) (*Monitor, error) {
	if panel == nil || billing == nil || dir == "" {
		return nil, fmt.Errorf("Sub2API monitor requires storage and request callbacks")
	}
	m := &Monitor{
		dir: dir, panel: panel, raw: panel, billing: billing,
		configs: make(map[string]monitorConfig), cool: make(map[string]cooldown),
		modes: make(map[string]modeCache), baselines: make(map[string]monitorBaseline),
		sem: make(chan struct{}, 4), now: time.Now,
		cancels: make(map[string]context.CancelFunc),
		wake:    make(chan struct{}, 1), woken: make(map[string]bool),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	if err := m.loadConfig(); err != nil {
		return nil, err
	}
	return m, nil
}

// SetRawReader is used only by snapshot -> V1 compatibility reads, before refresh.
func (m *Monitor) SetRawReader(read PanelRead) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if read != nil {
		m.raw = read
	}
}

func (m *Monitor) SetTargets(targets func() []Target) {
	m.mu.Lock()
	m.targets = targets
	m.targetReader = nil
	m.mu.Unlock()
}

func (m *Monitor) SetTargetReader(read func() ([]Target, error)) {
	m.mu.Lock()
	m.targetReader = read
	m.mu.Unlock()
}

func (m *Monitor) SetGenerationGuard(guard func(string, uint64, func() error) error) {
	m.mu.Lock()
	m.generationGuard = guard
	m.mu.Unlock()
}

func (m *Monitor) Config(session string) (bool, []Group) {
	settings, err := m.Settings(session)
	if err != nil {
		return false, nil
	}
	return settings.Enabled, settings.Groups
}

func (m *Monitor) Settings(session string) (MonitorSettings, error) {
	targets, targetsErr := m.currentTargets()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.loadConfig(); err != nil {
		return MonitorSettings{}, err
	}
	cfg, exists := m.configs[session]
	settings := MonitorSettings{Configured: exists, Enabled: exists && cfg.Enabled,
		Groups: append([]Group(nil), cfg.Groups...), Revision: cfg.Revision}
	if targetsErr == nil {
		for _, target := range targets {
			if target.SessionID == session {
				settings.Generation = target.Generation
				break
			}
		}
	}
	return settings, nil
}

// SetAccount keeps the package's old local callers working; UI controls use CAS.
func (m *Monitor) SetAccount(session string, enabled bool, groups []Group) error {
	settings, err := m.Settings(session)
	if err != nil {
		return err
	}
	return m.SetAccountExpected(session, enabled, groups, 0, settings.Revision)
}

func (m *Monitor) SetAccountExpected(session string, enabled bool, groups []Group, generation uint64, expectedRevision string) error {
	if strings.TrimSpace(session) == "" {
		return fmt.Errorf("Sub2API account is required")
	}
	clean := make([]Group, 0, len(groups))
	seen := make(map[string]bool)
	for _, group := range groups {
		id := strings.TrimSpace(group.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, Group{ID: id, Name: strings.TrimSpace(group.Name)})
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].ID < clean[j].ID })
	changed := false
	err := m.withGeneration(session, generation, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		if err := m.loadConfig(); err != nil {
			return err
		}
		before, exists := m.configs[session]
		if before.Revision != expectedRevision {
			return monitorStale()
		}
		if exists && before.Enabled == enabled && sameGroups(before.Groups, clean) {
			return nil
		}
		revision, err := randomID()
		if err != nil {
			return err
		}
		next := make(map[string]monitorConfig, len(m.configs)+1)
		for id, cfg := range m.configs {
			next[id] = cfg
		}
		next[session] = monitorConfig{Enabled: enabled, Groups: clean, Revision: revision}
		body, err := json.Marshal(monitorSettingsDisk{Version: 1, Accounts: next})
		if err != nil {
			return err
		}
		if err := edit.WriteAtomic(filepath.Join(m.dir, "monitor-settings.json"), body); err != nil {
			return fmt.Errorf("save Sub2API monitor settings: %w", err)
		}
		m.configs = next
		changed = true
		if cancel := m.cancels[session]; cancel != nil {
			cancel()
		}
		return nil
	})
	if err == nil && changed && enabled {
		m.Wake(session)
	}
	return err
}

// Wake merges account changes into the next round without enabling monitoring.
func (m *Monitor) Wake(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.woken[session] = true
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Monitor) Snapshot() Snapshot {
	targets, targetsErr := m.currentTargets()
	m.mu.Lock()
	defer m.mu.Unlock()
	settingsErr := m.loadConfig()
	// These are small, secret-free DTOs; serialization also copies nested pointers.
	committed := m.snapshot
	if disk, err := m.readCommitted(); err == nil {
		committed = disk.snapshot
	}
	body, _ := json.Marshal(committed)
	var snapshot Snapshot
	_ = json.Unmarshal(body, &snapshot)
	if snapshot.Accounts == nil {
		snapshot.Accounts = []MonitorAccount{}
	}
	if snapshot.Changes == nil {
		snapshot.Changes = []Change{}
	}
	snapshot.Running, snapshot.Polling = m.running, m.flight != nil
	if targetsErr == nil {
		existing := make(map[string]bool)
		for _, account := range snapshot.Accounts {
			existing[account.SessionID] = true
		}
		for _, target := range targets {
			if !existing[target.SessionID] {
				snapshot.Accounts = append(snapshot.Accounts, MonitorAccount{
					SessionID: target.SessionID, Site: target.Site, Generation: target.Generation,
				})
				existing[target.SessionID] = true
			}
		}
	}
	for i := range snapshot.Accounts {
		account := &snapshot.Accounts[i]
		account.Configured, account.Enabled = false, false
		if cfg, exists := m.configs[account.SessionID]; exists && settingsErr == nil {
			account.Configured, account.Enabled = true, cfg.Enabled
			snapshot.Accounts[i].Watched = append([]Group(nil), cfg.Groups...)
		} else if settingsErr != nil {
			account.Error = "Sub2API monitor settings are unavailable"
		}
	}
	sort.Slice(snapshot.Accounts, func(i, j int) bool { return snapshot.Accounts[i].SessionID < snapshot.Accounts[j].SessionID })
	return snapshot
}

// Run is tied to the gateway, not to any page or usage-display preference.
func (m *Monitor) Run(ctx context.Context, targets func() []Target, notify func(Change)) {
	m.mu.Lock()
	if targets != nil {
		m.targets = targets
		m.targetReader = nil
	}
	m.notify = notify
	m.running = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
	_ = m.refresh(ctx, scheduledRound)
	// Wake rounds do not move the schedule for other accounts.
	timer := time.NewTimer(pollInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			_ = m.refresh(ctx, scheduledRound)
			timer.Reset(pollInterval)
		case <-m.wake:
			_ = m.refresh(ctx, wakeRound)
		}
	}
}

func (m *Monitor) Refresh(ctx context.Context) error {
	return m.refresh(ctx, requestedRound)
}

func (m *Monitor) refresh(ctx context.Context, mode monitorRound) error {
	m.mu.Lock()
	if done := m.flight; done != nil {
		m.mu.Unlock()
		select {
		case <-done:
			m.mu.Lock()
			err := m.lastErr
			m.mu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	m.flight = done
	woken := m.woken
	m.woken = make(map[string]bool)
	select {
	case <-m.wake:
	default:
	}
	m.mu.Unlock()
	var err error
	targets, readErr := m.currentTargets()
	if readErr != nil {
		err = readErr
	} else {
		if mode == wakeRound {
			filtered := make([]Target, 0, len(targets))
			for _, target := range targets {
				if woken[target.SessionID] {
					filtered = append(filtered, target)
				}
			}
			targets = filtered
		}
		err = m.round(ctx, targets, mode == requestedRound, woken)
	}
	m.mu.Lock()
	m.lastErr = err
	m.flight = nil
	close(done)
	if len(m.woken) > 0 {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	} else {
		select {
		case <-m.wake:
		default:
		}
	}
	m.mu.Unlock()
	return err
}

func (m *Monitor) round(ctx context.Context, targets []Target, force bool, woken map[string]bool) error {
	unlock, held := filelock.Lock(filepath.Join(m.dir, "monitor-owner.lock"), 0)
	defer unlock()
	if !held {
		if force {
			return monitorBusy()
		}
		return nil
	}
	m.mu.Lock()
	if err := m.load(); err != nil {
		m.mu.Unlock()
		return err
	}
	if err := m.loadConfig(); err != nil {
		m.mu.Unlock()
		return err
	}
	old := make(map[string]MonitorAccount)
	for _, account := range m.snapshot.Accounts {
		old[account.SessionID] = account
	}
	configs := make(map[string]monitorConfig, len(m.configs))
	for session, cfg := range m.configs {
		configs[session] = cfg
	}
	m.mu.Unlock()

	bySession := make(map[string][]Target)
	for _, target := range targets {
		if target.SessionID != "" {
			bySession[target.SessionID] = append(bySession[target.SessionID], target)
		}
	}
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	var roundErr error
	recordError := func(err error) {
		if err != nil {
			resultMu.Lock()
			if roundErr == nil {
				roundErr = err
			}
			resultMu.Unlock()
		}
	}
	for session, sources := range bySession {
		cfg, configured := configs[session]
		previous := old[session]
		previous.SessionID, previous.Site, previous.Enabled = session, sources[0].Site, cfg.Enabled
		previous.Configured, previous.SettingsRevision, previous.Generation = configured, cfg.Revision, sources[0].Generation
		previous.Watched = cfg.Groups
		if !configured || !cfg.Enabled {
			continue
		}
		ticket := monitorTicket{session: session, settings: cfg.Revision, generation: sources[0].Generation, targets: copyMonitorTargets(sources)}
		if expected, exists := ctx.Value(monitorTicketKey{}).(monitorTicket); exists {
			ticket = expected
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A deterministic stagger prevents every account from hitting a panel at once.
			if !force && !woken[session] {
				delay := time.Duration(sha256.Sum256([]byte(session))[0]%20) * 50 * time.Millisecond
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
			pollCtx := context.WithValue(ctx, monitorTicketKey{}, ticket)
			if err := m.withTicket(ticket, nil); err != nil {
				recordError(err)
				return
			}
			account, baseline, change := m.pollAccount(pollCtx, previous, sources, cfg.Groups)
			if err := m.commitCandidate(pollCtx, ticket, account, baseline, change); err != nil {
				recordError(err)
			}
		}()
	}
	wg.Wait()
	if roundErr != nil {
		return roundErr
	}
	return ctx.Err()
}

func (m *Monitor) load() error {
	body, err := os.ReadFile(filepath.Join(m.dir, "monitor.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Sub2API monitor snapshot: %w", err)
	}
	var disk monitorDisk
	if err := json.Unmarshal(body, &disk); err != nil || (disk.Version != 1 && disk.Version != 2) {
		return fmt.Errorf("Sub2API monitor snapshot is unreadable; original file preserved")
	}
	m.snapshot = disk.Snapshot
	m.cool = disk.Cooldowns
	m.baselines = disk.Baselines
	if m.baselines == nil {
		m.baselines = make(map[string]monitorBaseline)
	}
	if disk.Version == 1 {
		// Old snapshots have neither sampling identities nor field baselines.
		m.baselines = make(map[string]monitorBaseline)
		for i := range m.snapshot.Accounts {
			account := &m.snapshot.Accounts[i]
			account.GroupsKnown, account.Complete = false, false
			account.LastObservation, account.LastObservationAt = account.Observation, account.LastSuccess
			if account.Observation != nil && monitorHasMetrics(account.Observation) {
				account.ObservationHistory = monitorObservationHistory(nil, account.Observation, account.LastObservationAt)
			}
			account.Observation = nil
			for j := range account.Groups {
				group := &account.Groups[j]
				group.LastVisible, group.LastVisibleAt = group.Visible, group.LastSuccess
				group.LastRate, group.LastRateAt = group.Rate, group.LastSuccess
				group.LastEnabled, group.LastEnabledAt = group.Enabled, group.LastSuccess
				group.LastObservation, group.LastObservationAt = group.Observation, group.ObservedAt
				if group.Observation != nil && monitorHasMetrics(group.Observation) {
					group.ObservationHistory = monitorObservationHistory(nil, group.Observation, group.LastObservationAt)
				}
				group.Visible, group.Rate, group.Enabled, group.Observation = nil, nil, nil, nil
				group.ObservedAt = time.Time{}
			}
			for j := range account.Sources {
				source := &account.Sources[j]
				source.Rate, source.BindingVersion, source.Revision = nil, "", ""
				for k := range source.Keys {
					key := &source.Keys[k]
					key.LastRate, key.LastRateAt = key.Rate, key.LastSuccess
					key.Rate = nil
				}
			}
		}
	}
	if m.cool == nil {
		m.cool = make(map[string]cooldown)
	}
	return nil
}

func (m *Monitor) loadConfig() error {
	body, err := os.ReadFile(filepath.Join(m.dir, "monitor-settings.json"))
	if errors.Is(err, os.ErrNotExist) {
		m.configs = make(map[string]monitorConfig)
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Sub2API monitor settings: %w", err)
	}
	var disk monitorSettingsDisk
	if err := json.Unmarshal(body, &disk); err != nil || disk.Version != 1 || disk.Accounts == nil {
		return fmt.Errorf("Sub2API monitor settings are unreadable; original file preserved")
	}
	for _, config := range disk.Accounts {
		if config.Revision == "" {
			return fmt.Errorf("Sub2API monitor settings have no control revision; original file preserved")
		}
	}
	m.configs = disk.Accounts
	return nil
}

func (m *Monitor) request(ctx context.Context, session, resource string, read func(context.Context) ([]byte, error)) ([]byte, error) {
	if err := m.checkRequest(ctx); err != nil {
		return nil, err
	}
	now := m.now()
	m.mu.Lock()
	c := m.cool[session+"\x00"+resource]
	accountLimit := m.cool[session+"\x00limited"]
	m.mu.Unlock()
	ticket, _ := ctx.Value(monitorTicketKey{}).(monitorTicket)
	generation := ticket.generation
	isBilling := strings.HasPrefix(resource, "billing:")
	if isBilling {
		generation = 0
	}
	if !isBilling {
		if err := accountLimit.err(now, generation); err != nil {
			return nil, err
		}
	}
	if err := c.err(now, generation); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	select {
	case m.sem <- struct{}{}:
	case <-callCtx.Done():
		return nil, callCtx.Err()
	}
	if err := callCtx.Err(); err != nil {
		<-m.sem
		return nil, err
	}
	if err := m.withTicket(ticket, func() error {
		m.mu.Lock()
		m.cancels[session] = cancel
		m.mu.Unlock()
		return nil
	}); err != nil {
		<-m.sem
		return nil, err
	}
	body, err := read(callCtx)
	<-m.sem
	m.mu.Lock()
	delete(m.cancels, session)
	m.mu.Unlock()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if checkErr := m.withTicket(ticket, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		key := session + "\x00" + resource
		if err == nil {
			delete(m.cool, key)
			if resource == "/api/v1/groups/available" {
				delete(m.cool, session+"\x00limited")
			}
		} else {
			m.failedLocked(session, resource, ticket.generation, err)
		}
		return nil
	}); checkErr != nil {
		return nil, checkErr
	}
	return body, err
}

func (m *Monitor) failed(ctx context.Context, session, resource string, err error) {
	ticket, ok := ctx.Value(monitorTicketKey{}).(monitorTicket)
	if !ok || ctx.Err() != nil {
		return
	}
	_ = m.withTicket(ticket, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.failedLocked(session, resource, ticket.generation, err)
		return nil
	})
}

func (m *Monitor) failedLocked(session, resource string, generation uint64, err error) {
	key := session + "\x00" + resource
	c := m.cool[key]
	c.Failures++
	c.Generation = generation
	c.NeedsAction = monitorNeedsAction(err)
	c.Status, c.Challenge, c.RetryAfter = 0, false, 0
	delay := pollInterval * time.Duration(1<<min(c.Failures, 4))
	if delay > 15*time.Minute {
		delay = 15 * time.Minute
	}
	if resource == "/api/v1/settings/public" {
		delay = 30 * time.Second
	}
	var detail *Error
	if errors.As(err, &detail) {
		c.Status, c.Challenge, c.RetryAfter = detail.Status, detail.Challenge, detail.RetryAfter
		if detail.RetryAfter > delay {
			delay = detail.RetryAfter
		}
	}
	c.Until = m.now().Add(delay)
	m.cool[key] = c
	// Key and optional monitor failures are scoped to that resource.
	if resource == "/api/v1/groups/available" && (c.Status == 401 || c.Challenge) {
		m.cool[session+"\x00limited"] = c
	}
}

func (m *Monitor) readPanel(ctx context.Context, session, path string, raw bool) ([]byte, error) {
	m.mu.Lock()
	read := m.panel
	if raw {
		read = m.raw
	}
	m.mu.Unlock()
	resource := path
	if raw {
		resource += "|raw"
	}
	return m.request(ctx, session, resource, func(ctx context.Context) ([]byte, error) {
		return read(ctx, session, path)
	})
}

func monitorError(err error) string {
	if err == nil {
		return ""
	}
	var detail *Error
	if errors.As(err, &detail) {
		if detail.Challenge {
			return "Sub2API requires browser verification"
		}
		if detail.Status > 0 {
			return "Sub2API request failed (HTTP " + strconv.Itoa(detail.Status) + ")"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Sub2API request timed out"
	}
	return "Sub2API observation is unavailable"
}

func monitorChangeID(session string, at time.Time, items []ChangeItem) string {
	body, _ := json.Marshal(items)
	digest := sha256.Sum256(append([]byte(session+at.Format(time.RFC3339Nano)), body...))
	return hex.EncodeToString(digest[:12])
}
