package sub2api

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/yetone/magpie/internal/edit"
	"github.com/yetone/magpie/internal/filelock"
)

type monitorTicket struct {
	session, settings string
	generation        uint64
	targets           []Target
	preview           bool
}

type monitorTicketKey struct{}

func monitorStale() error {
	return &Error{Kind: KindConflict, Code: "STALE_MONITOR", Message: "Sub2API monitoring changed; reload its current state"}
}

func monitorBusy() error {
	return &Error{Kind: KindTemporary, Code: "MONITOR_BUSY", Message: "Sub2API is already being sampled"}
}

func sameGroups(left, right []Group) bool {
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	return string(a) == string(b)
}

func (m *Monitor) withGeneration(session string, generation uint64, commit func() error) error {
	m.mu.Lock()
	guard := m.generationGuard
	m.mu.Unlock()
	if guard != nil {
		if generation == 0 {
			return monitorStale()
		}
		return guard(session, generation, commit)
	}
	return WithControl(m.dir, commit)
}

func (m *Monitor) currentTargets() ([]Target, error) {
	m.mu.Lock()
	reader, targets := m.targetReader, m.targets
	m.mu.Unlock()
	if reader != nil {
		return reader()
	}
	if targets != nil {
		return targets(), nil
	}
	return nil, fmt.Errorf("Sub2API targets are unavailable")
}

func monitorTargetsIdentity(targets []Target) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		keys := make([]string, 0, len(target.Keys))
		for _, key := range target.Keys {
			keys = append(keys, monitorIdentity(key.ID, key.GroupID, key.GroupName, key.Value, key.Context))
		}
		sort.Strings(keys)
		active := append([]string(nil), target.ActiveKeyIDs...)
		retained := append([]string(nil), target.RetainedKeyIDs...)
		sort.Strings(active)
		sort.Strings(retained)
		body, _ := json.Marshal(struct {
			ID, Site, Session, Group, Name, Binding, Revision, Context string
			Generation                                                 uint64
			Keys, Active, Retained                                     []string
		}{target.ID, target.Site, target.SessionID, target.GroupID, target.GroupName,
			target.BindingVersion, target.Revision, target.Context, target.Generation, keys, active, retained})
		parts = append(parts, string(body))
	}
	sort.Strings(parts)
	return monitorIdentity(parts...)
}

func targetsForSession(targets []Target, session string) []Target {
	filtered := make([]Target, 0)
	for _, target := range targets {
		if target.SessionID == session {
			filtered = append(filtered, target)
		}
	}
	return filtered
}

func copyMonitorTargets(targets []Target) []Target {
	out := append([]Target(nil), targets...)
	for i := range out {
		out[i].Keys = append([]Key(nil), targets[i].Keys...)
		out[i].ActiveKeyIDs = append([]string(nil), targets[i].ActiveKeyIDs...)
		out[i].RetainedKeyIDs = append([]string(nil), targets[i].RetainedKeyIDs...)
		out[i].WatchGroups = append([]Group(nil), targets[i].WatchGroups...)
	}
	return out
}

func (m *Monitor) CancelAccount(session string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel := m.cancels[session]; cancel != nil {
		cancel()
	}
}

// withTicket is a short durable control check. HTTP and notification preparation stay outside it.
func (m *Monitor) withTicket(ticket monitorTicket, commit func() error) error {
	return m.withGeneration(ticket.session, ticket.generation, func() error {
		m.mu.Lock()
		err := m.loadConfig()
		cfg, configured := m.configs[ticket.session]
		m.mu.Unlock()
		if err != nil {
			return err
		}
		if cfg.Revision != ticket.settings || !ticket.preview && (!configured || !cfg.Enabled) {
			return monitorStale()
		}
		current, err := m.currentTargets()
		if err != nil {
			return err
		}
		current = targetsForSession(current, ticket.session)
		if ticket.preview {
			expected := make([]Target, 0, len(ticket.targets))
			for _, target := range ticket.targets {
				found := false
				for _, actual := range current {
					if target.ID == "" {
						if actual.Site == target.Site && actual.Context == target.Context && actual.Generation == target.Generation {
							expected = append(expected, target)
							found = true
							break
						}
					} else if actual.ID == target.ID {
						expected = append(expected, actual)
						found = true
						break
					}
				}
				if !found {
					return monitorStale()
				}
			}
			current = expected
		}
		if monitorTargetsIdentity(current) != monitorTargetsIdentity(ticket.targets) {
			return monitorStale()
		}
		if commit != nil {
			return commit()
		}
		return nil
	})
}

func (m *Monitor) checkRequest(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ticket, exists := ctx.Value(monitorTicketKey{}).(monitorTicket)
	if !exists {
		return monitorStale()
	}
	return m.withTicket(ticket, nil)
}

func (m *Monitor) RefreshAccount(ctx context.Context, session string, generation uint64, settingsRevision string) error {
	targets, err := m.currentTargets()
	if err != nil {
		return err
	}
	targets = targetsForSession(targets, session)
	ticket := monitorTicket{session: session, generation: generation, settings: settingsRevision, targets: copyMonitorTargets(targets)}
	if len(targets) == 0 {
		return monitorStale()
	}
	if err := m.withTicket(ticket, nil); err != nil {
		return err
	}
	return m.round(context.WithValue(ctx, monitorTicketKey{}, ticket), targets, true, nil)
}

func (m *Monitor) readCommitted() (*Monitor, error) {
	disk := &Monitor{dir: m.dir, cool: make(map[string]cooldown), baselines: make(map[string]monitorBaseline)}
	if err := disk.load(); err != nil {
		return nil, err
	}
	return disk, nil
}

func (m *Monitor) commitCandidate(ctx context.Context, ticket monitorTicket, account MonitorAccount, baseline monitorBaseline, change Change) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := m.withTicket(ticket, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		disk, err := m.readCommitted()
		if err != nil {
			return err
		}
		found := false
		for i := range disk.snapshot.Accounts {
			if disk.snapshot.Accounts[i].SessionID == ticket.session {
				account.NotificationError = disk.snapshot.Accounts[i].NotificationError
				disk.snapshot.Accounts[i], found = account, true
				break
			}
		}
		if !found {
			disk.snapshot.Accounts = append(disk.snapshot.Accounts, account)
		}
		sort.Slice(disk.snapshot.Accounts, func(i, j int) bool {
			return disk.snapshot.Accounts[i].SessionID < disk.snapshot.Accounts[j].SessionID
		})
		disk.baselines[ticket.session] = baseline
		if len(change.Items) > 0 {
			disk.snapshot.Changes = append(disk.snapshot.Changes, change)
			if len(disk.snapshot.Changes) > 100 {
				disk.snapshot.Changes = disk.snapshot.Changes[len(disk.snapshot.Changes)-100:]
			}
		}
		disk.snapshot.UpdatedAt = m.now()
		body, err := json.Marshal(monitorDisk{Version: 2, Snapshot: disk.snapshot, Cooldowns: m.cool, Baselines: disk.baselines})
		if err != nil {
			return err
		}
		if err := edit.WriteAtomic(filepath.Join(m.dir, "monitor.json"), body); err != nil {
			return fmt.Errorf("save Sub2API monitor snapshot: %w", err)
		}
		m.snapshot, m.baselines = disk.snapshot, disk.baselines
		// This ticket also satisfies wakes received during the same read.
		delete(m.woken, ticket.session)
		return nil
	})
	if err != nil {
		return err
	}
	if len(change.Items) > 0 {
		m.mu.Lock()
		notify := m.notify
		m.mu.Unlock()
		change.WithCurrent = func(send func() error) error { return m.withTicket(ticket, send) }
		change.reportNotification = func(err error) { m.reportNotification(ticket, change.ID, err) }
		if notify != nil && m.withTicket(ticket, nil) == nil {
			notify(change)
		}
	}
	return nil
}

func (m *Monitor) reportNotification(ticket monitorTicket, changeID string, sendErr error) {
	_ = m.withTicket(ticket, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		disk, err := m.readCommitted()
		if err != nil {
			return err
		}
		for i := len(disk.snapshot.Changes) - 1; i >= 0; i-- {
			if disk.snapshot.Changes[i].SessionID == ticket.session {
				if disk.snapshot.Changes[i].ID != changeID {
					return nil
				}
				break
			}
		}
		for i := range disk.snapshot.Accounts {
			if disk.snapshot.Accounts[i].SessionID == ticket.session {
				disk.snapshot.Accounts[i].NotificationError = ""
				if sendErr != nil {
					disk.snapshot.Accounts[i].NotificationError = "Sub2API notification could not be delivered"
				}
			}
		}
		body, err := json.Marshal(monitorDisk{Version: 2, Snapshot: disk.snapshot, Cooldowns: disk.cool, Baselines: disk.baselines})
		if err != nil {
			return err
		}
		if err := edit.WriteAtomic(filepath.Join(m.dir, "monitor.json"), body); err != nil {
			return err
		}
		m.snapshot = disk.snapshot
		return nil
	})
}

// Preview holds sampling ownership, but commits only necessary hard cooldowns.
func (m *Monitor) Preview(ctx context.Context, session string, generation uint64, settingsRevision string, targets []Target) (MonitorAccount, error) {
	ticket := monitorTicket{session: session, settings: settingsRevision, generation: generation, targets: copyMonitorTargets(targets), preview: true}
	if len(targets) == 0 || len(targetsForSession(targets, session)) != len(targets) {
		return MonitorAccount{}, monitorStale()
	}
	unlock, held := filelock.Lock(filepath.Join(m.dir, "monitor-owner.lock"), 0)
	defer unlock()
	if !held {
		return MonitorAccount{}, monitorBusy()
	}
	if err := m.withTicket(ticket, nil); err != nil {
		return MonitorAccount{}, err
	}
	m.mu.Lock()
	err := m.load()
	var previous MonitorAccount
	for _, account := range m.snapshot.Accounts {
		if account.SessionID == session {
			previous = account
		}
	}
	m.mu.Unlock()
	if err != nil {
		return MonitorAccount{}, err
	}
	previous.SessionID, previous.Site, previous.Generation = session, targets[0].Site, generation
	previous.SettingsRevision = settingsRevision
	ctx = context.WithValue(ctx, monitorTicketKey{}, ticket)
	account, _, _ := m.pollAccount(ctx, previous, targets, nil)
	if err := ctx.Err(); err != nil {
		return MonitorAccount{}, err
	}
	err = m.withTicket(ticket, func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		disk, err := m.readCommitted()
		if err != nil {
			return err
		}
		hard := disk.cool
		for key, value := range m.cool {
			if value.Status == 429 || value.Challenge || value.Status == 401 || value.NeedsAction || value.RetryAfter > 0 {
				hard[key] = value
			}
		}
		if len(hard) == 0 {
			return nil
		}
		body, err := json.Marshal(monitorDisk{Version: 2, Snapshot: disk.snapshot, Cooldowns: hard, Baselines: disk.baselines})
		if err != nil {
			return err
		}
		return edit.WriteAtomic(filepath.Join(m.dir, "monitor.json"), body)
	})
	if err != nil {
		return MonitorAccount{}, err
	}
	return account, nil
}
