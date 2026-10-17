package sub2api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/proc"
)

func TestMonitorExplicitSettingsCASAndPreview(t *testing.T) {
	dir := t.TempDir()
	var reads atomic.Int64
	monitor, err := NewMonitor(dir, func(ctx context.Context, session, path string) ([]byte, error) {
		reads.Add(1)
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	target.BindingVersion, target.Generation = "binding", 1
	monitor.SetTargets(func() []Target { return []Target{target} })
	settings, err := monitor.Settings(target.SessionID)
	if err != nil || settings.Configured || settings.Enabled || settings.Revision != "" {
		t.Fatalf("missing settings were enabled: %+v %v", settings, err)
	}
	if err := monitor.Refresh(t.Context()); err != nil || reads.Load() != 0 {
		t.Fatalf("unconfigured monitor sampled: %d %v", reads.Load(), err)
	}
	preview, err := monitor.Preview(t.Context(), target.SessionID, 1, "", []Target{target})
	if err != nil || preview.Sources[0].Rate == nil || len(monitor.baselines) != 0 || len(monitor.Snapshot().Changes) != 0 {
		t.Fatalf("preview failed or committed background state: %+v %v", preview, err)
	}
	for _, name := range []string{"monitor-settings.json", "monitor.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview created %s: %v", name, err)
		}
	}
	if err := monitor.SetAccountExpected(target.SessionID, true, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
	first, _ := monitor.Settings(target.SessionID)
	if first.Revision == "" {
		t.Fatal("created settings have no revision")
	}
	if err := monitor.SetAccountExpected(target.SessionID, false, nil, 1, ""); err == nil {
		t.Fatal("old page rewrote configured settings")
	}
	if err := monitor.SetAccountExpected(target.SessionID, true, nil, 1, first.Revision); err != nil {
		t.Fatal(err)
	}
	same, _ := monitor.Settings(target.SessionID)
	if same.Revision != first.Revision {
		t.Fatal("unchanged settings churned revision")
	}
	if err := os.Remove(filepath.Join(dir, "monitor-settings.json")); err != nil {
		t.Fatal(err)
	}
	if err := monitor.SetAccountExpected(target.SessionID, true, nil, 1, ""); err != nil {
		t.Fatal(err)
	}
	rebuilt, _ := monitor.Settings(target.SessionID)
	if rebuilt.Revision == first.Revision {
		t.Fatal("rebuilt settings revived an old ticket")
	}
	bad := []byte(`{"version":999,"accounts":{}}`)
	if err := os.WriteFile(filepath.Join(dir, "monitor-settings.json"), bad, 0o600); err != nil {
		t.Fatal(err)
	}
	before := reads.Load()
	if err := monitor.Refresh(t.Context()); err == nil {
		t.Fatal("unknown settings format was accepted")
	}
	if err := monitor.SetAccountExpected(target.SessionID, true, nil, 1, ""); err == nil {
		t.Fatal("unknown settings format was overwritten")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "monitor-settings.json"))
	if reads.Load() != before || !bytes.Equal(after, bad) {
		t.Fatal("unreadable control state allowed requests or was changed")
	}
}

func TestMonitorLegacyNumericSnapshotKeepsHistoryWithoutWriteTicket(t *testing.T) {
	dir := t.TempDir()
	body := []byte(`{"version":2,"snapshot":{"accounts":[{"sessionId":"session","sources":[{"id":"source","bindingVersion":7,"revision":"old-ticket","rate":2,"keys":[{"id":"key","rate":2,"lastSuccess":"2026-10-16T00:00:00Z"}]}]}]}}`)
	if err := os.WriteFile(filepath.Join(dir, "monitor.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewMonitor(dir, monitorTestPanel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	source := m.Snapshot().Accounts[0].Sources[0]
	if source.BindingVersion != "" || source.Revision != "" || source.Rate != nil ||
		source.Keys[0].Rate != nil || source.Keys[0].LastRate == nil || *source.Keys[0].LastRate != 2 {
		t.Fatalf("numeric draft ticket was usable or history lost: %+v", source)
	}
}

func TestMonitorPreviewSharesOwnerAndHardCooldownWithoutBackgroundCommit(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			dir := t.TempDir()
			var calls atomic.Int64
			target := monitorTestTarget()
			m, err := NewMonitor(dir, monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
				calls.Add(1)
				return nil, &Error{Status: status, RetryAfter: time.Hour}
			})
			if err != nil {
				t.Fatal(err)
			}
			m.SetTargets(func() []Target { return []Target{target} })
			preview, err := m.Preview(t.Context(), "session", 0, "", []Target{target})
			if err != nil || preview.BillingError == "" {
				t.Fatalf("rate-limited preview failed incorrectly: %+v %v", preview, err)
			}
			restarted, err := NewMonitor(dir, monitorTestPanel, m.billing)
			if err != nil {
				t.Fatal(err)
			}
			restarted.SetTargets(func() []Target { return []Target{target} })
			if _, err := restarted.Preview(t.Context(), "session", 0, "", []Target{target}); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || len(restarted.baselines) != 0 || len(restarted.snapshot.Accounts) != 0 || len(restarted.snapshot.Changes) != 0 {
				t.Fatalf("preview bypassed cooldown or committed background state: calls=%d %+v", calls.Load(), restarted.snapshot)
			}
			unlock, held := filelock.Lock(filepath.Join(dir, "monitor-owner.lock"), 0)
			if !held {
				t.Fatal("fixture owner was unavailable")
			}
			defer unlock()
			if _, err := restarted.Preview(t.Context(), "session", 0, "", []Target{target}); err == nil {
				t.Fatal("preview ignored existing polling owner")
			}
		})
	}
}

func TestMonitorBusyRefreshAccountPreservesOwnerCooldown(t *testing.T) {
	target := monitorTestTarget()
	first := target.Keys[0]
	target.Keys = append(target.Keys, Key{ID: "second", Value: "second-secret", Context: "second-network"})
	var phase, firstCalls atomic.Int64
	var blocked atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	m, err := NewMonitor(t.TempDir(), monitorTestPanel, func(ctx context.Context, _ Target, key Key) ([]byte, error) {
		if key.ID == first.ID {
			firstCalls.Add(1)
			if phase.Load() > 0 {
				return nil, &Error{Status: 429, RetryAfter: time.Hour}
			}
		}
		if key.ID == "second" && phase.Load() == 1 && blocked.CompareAndSwap(false, true) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return []byte(`{"effective_rate_multiplier":1}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC) }
	monitorTestTargets(t, m, func() []Target { return copyMonitorTargets([]Target{target}) })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	phase.Store(1)
	done := make(chan error, 1)
	go func() { done <- m.Refresh(t.Context()) }()
	wait := sync.OnceValue(func() error { return <-done })
	t.Cleanup(func() { unblock(); _ = wait() })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("owner did not reach the blocked second billing request")
	}
	settings, err := m.Settings(target.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var detail *Error
	if err := m.RefreshAccount(t.Context(), target.SessionID, target.Generation, settings.Revision); !errors.As(err, &detail) || detail.Code != "MONITOR_BUSY" {
		t.Fatalf("contending account refresh did not reject the active owner: %v", err)
	}
	unblock()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	phase.Store(2)
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := firstCalls.Load(); got != 2 {
		t.Fatalf("busy account refresh erased the owner's one-hour Retry-After: first billing calls = %d, want 2", got)
	}
}

func TestMonitorPreviewAuthCooldownKeepsRequiredAction(t *testing.T) {
	for _, detail := range []*Error{{Kind: KindAuth}, {Kind: KindChallenge}, {Kind: KindUnknown, Code: "REFRESH_UNCERTAIN"}} {
		t.Run(detail.Kind, func(t *testing.T) {
			var calls atomic.Int64
			target := monitorTestTarget()
			target.Generation = 1
			monitor, err := NewMonitor(t.TempDir(), func(context.Context, string, string) ([]byte, error) {
				calls.Add(1)
				return nil, detail
			}, monitorTestBilling)
			if err != nil {
				t.Fatal(err)
			}
			targets := func() []Target { return []Target{target} }
			monitor.SetTargets(targets)
			if _, err := monitor.Preview(t.Context(), target.SessionID, 1, "", targets()); err != nil {
				t.Fatal(err)
			}
			monitor, err = NewMonitor(monitor.dir, monitor.panel, monitorTestBilling)
			if err != nil {
				t.Fatal(err)
			}
			monitor.SetTargets(targets)
			if err := monitor.SetAccount(target.SessionID, true, nil); err != nil {
				t.Fatal(err)
			}
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || !monitor.Snapshot().Accounts[0].NeedsAction {
				t.Fatalf("preview cooldown lost the required action: calls = %d, account = %+v", calls.Load(), monitor.Snapshot().Accounts[0])
			}
		})
	}
}

func TestMonitorControlProcessHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "--monitor-settings" {
			continue
		}
		if len(os.Args) != i+2 {
			t.Fatal("missing monitor fixture directory")
		}
		m, err := NewMonitor(os.Args[i+1], monitorTestPanel, monitorTestBilling)
		if err != nil {
			t.Fatal(err)
		}
		settings, err := m.Settings("session")
		if err != nil {
			t.Fatal(err)
		}
		if err := m.SetAccountExpected("session", false, nil, 0, settings.Revision); err != nil {
			t.Fatal(err)
		}
		settings, _ = m.Settings("session")
		if err := m.SetAccountExpected("session", true, nil, 0, settings.Revision); err != nil {
			t.Fatal(err)
		}
		return
	}
}

func TestMonitorAnotherProcessDisableReenableRejectsOldRound(t *testing.T) {
	dir := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	var block atomic.Bool
	var billing atomic.Int64
	rate := atomic.Int64{}
	rate.Store(1)
	m, err := NewMonitor(dir, func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" && block.Load() {
			close(entered)
			<-release
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		billing.Add(1)
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate.Load())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, m, func() []Target { return []Target{monitorTestTarget()} })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "monitor.json"))
	block.Store(true)
	rate.Store(2)
	done := make(chan error, 1)
	go func() { done <- m.Refresh(t.Context()) }()
	t.Cleanup(func() { unblock(); <-done })
	<-entered
	child := proc.CommandContext(t.Context(), os.Args[0], "-test.run=^TestMonitorControlProcessHelper$", "--", "--monitor-settings", dir)
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("monitor control subprocess: %v\n%s", err, output)
	}
	unblock()
	err = <-done
	done <- err
	if err == nil {
		t.Fatal("old result survived disable and reenable in another process")
	}
	after, _ := os.ReadFile(filepath.Join(dir, "monitor.json"))
	if !bytes.Equal(before, after) || billing.Load() != 1 || len(m.Snapshot().Changes) != 0 {
		t.Fatal("late round changed snapshot/baseline/history or continued billing")
	}
	settings, _ := m.Settings("session")
	if !settings.Enabled {
		t.Fatal("durable enabled state was not reread")
	}
}

func TestMonitorRebindAndGenerationRejectLateCandidates(t *testing.T) {
	for _, mode := range []string{"binding", "key-group", "generation", "reader-error"} {
		t.Run(mode, func(t *testing.T) {
			target := monitorTestTarget()
			target.Generation, target.BindingVersion = 1, "original"
			var targetMu sync.Mutex
			readerErr := false
			entered, release := make(chan struct{}), make(chan struct{})
			unblock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(unblock)
			var block atomic.Bool
			m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
				if block.Load() && path == "/api/v1/groups/available" {
					close(entered)
					<-release
				}
				return monitorTestPanel(ctx, session, path)
			}, monitorTestBilling)
			if err != nil {
				t.Fatal(err)
			}
			m.SetTargetReader(func() ([]Target, error) {
				targetMu.Lock()
				defer targetMu.Unlock()
				if readerErr {
					return nil, errors.New("configuration is unreadable")
				}
				return copyMonitorTargets([]Target{target}), nil
			})
			m.SetGenerationGuard(func(session string, expected uint64, commit func() error) error {
				return WithControl(m.dir, func() error {
					targetMu.Lock()
					current := target.Generation
					targetMu.Unlock()
					if current != expected {
						return monitorStale()
					}
					return commit()
				})
			})
			if err := m.SetAccountExpected("session", true, nil, 1, ""); err != nil {
				t.Fatal(err)
			}
			if err := m.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(m.dir, "monitor.json"))
			block.Store(true)
			done := make(chan error, 1)
			go func() { done <- m.Refresh(t.Context()) }()
			<-entered
			targetMu.Lock()
			switch mode {
			case "binding":
				target.BindingVersion = "rebound"
			case "key-group":
				target.Keys[0].GroupID, target.Keys[0].GroupName = "6", "Group B"
			case "generation":
				target.Generation++
			default:
				readerErr = true
			}
			targetMu.Unlock()
			unblock()
			if err := <-done; err == nil {
				t.Fatal("late candidate passed changed durable control")
			}
			after, _ := os.ReadFile(filepath.Join(m.dir, "monitor.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("invalid candidate advanced committed state")
			}
		})
	}
}

func TestMonitorPausedKeyKeepsBaselineAndPanelListIsIndependent(t *testing.T) {
	target := monitorTestTarget()
	target.Keys[0].Context = "independent-billing"
	target.RetainedKeyIDs = []string{target.Keys[0].ID}
	rate, groups := 1, `[{"id":5,"name":"Group A"}]`
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			return []byte(`{"data":` + groups + `}`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, m, func() []Target { return []Target{target} })
	refresh := func() {
		t.Helper()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	key := target.Keys[0]
	target.Keys = nil
	rate = 3
	refresh()
	if len(m.baselines["session"].Keys) != 1 {
		t.Fatal("pausing a retained key erased its baseline")
	}
	target.Keys = []Key{key}
	refresh()
	if changes := m.Snapshot().Changes; len(changes) != 1 || changes[0].Items[0].Before != "1" || changes[0].Items[0].After != "3" {
		t.Fatalf("resumed key did not report net change: %+v", changes)
	}
	target.Keys[0].Context = "new-billing-context"
	groups = `[{"id":5,"name":"Group A"},{"id":6,"name":"Fixture group B"}]`
	rate = 4
	refresh()
	changes := m.Snapshot().Changes
	if len(changes) != 2 || len(changes[1].Items) != 1 || changes[1].Items[0].Kind != "group_added" {
		t.Fatalf("key context reset panel list or reused key baseline: %+v", changes)
	}
}

func TestMonitorPanelChallengeKeepsBillingAndActionNotificationOnce(t *testing.T) {
	var phase atomic.Int64
	var billing atomic.Int64
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			switch phase.Load() {
			case 0:
				return nil, &Error{Kind: KindChallenge, Status: 403, Challenge: true}
			case 1:
				return nil, &Error{Kind: KindTemporary, Status: 503}
			}
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		billing.Add(1)
		return []byte(`{"effective_rate_multiplier":2}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now }
	monitorTestTargets(t, m, func() []Target { return []Target{monitorTestTarget()} })
	notifications := 0
	m.notify = func(change Change) {
		notifications++
		change.ReportNotification(errors.New("private transport detail"))
	}
	for _, value := range []int64{0, 0, 1, 2, 0} {
		phase.Store(value)
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(20 * time.Minute)
	}
	snapshot := m.Snapshot()
	if billing.Load() != 5 || notifications != 2 || len(snapshot.Changes) != 2 || !snapshot.Accounts[0].NeedsAction {
		t.Fatalf("panel blocked billing or action state repeated: billing=%d notify=%d %+v", billing.Load(), notifications, snapshot)
	}
	if snapshot.Accounts[0].NotificationError == "" || strings.Contains(snapshot.Accounts[0].NotificationError, "private") {
		t.Fatal("notification failure is missing or exposes its raw error")
	}
}

func TestMonitorNotificationRechecksAfterPreparationAndNeverReplays(t *testing.T) {
	rate := 1
	target := monitorTestTarget()
	m, err := NewMonitor(t.TempDir(), monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, m, func() []Target { return []Target{target} })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var pending Change
	m.notify = func(change Change) { pending = change }
	rate = 2
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if pending.WithCurrent == nil {
		t.Fatal("notification lost its final control ticket")
	}
	settings, _ := m.Settings("session")
	if err := m.SetAccountExpected("session", false, nil, 0, settings.Revision); err != nil {
		t.Fatal(err)
	}
	settings, _ = m.Settings("session")
	if err := m.SetAccountExpected("session", true, nil, 0, settings.Revision); err != nil {
		t.Fatal(err)
	}
	sent := false
	if err := pending.WithCurrent(func() error { sent = true; return nil }); err == nil || sent {
		t.Fatal("prepared notification passed disable/reenable")
	}
	pending.ReportNotification(errors.New("late failure"))
	if m.Snapshot().Accounts[0].NotificationError != "" {
		t.Fatal("late notification changed current status")
	}
	restarted, err := NewMonitor(m.dir, monitorTestPanel, m.billing)
	if err != nil {
		t.Fatal(err)
	}
	restarted.SetTargets(func() []Target { return []Target{target} })
	restarted.notify = func(Change) { t.Error("historical change was replayed") }
	if err := restarted.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMonitorOptionalBudgetAndPerAccountCommit(t *testing.T) {
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	var optional atomic.Int64
	target := monitorTestTarget()
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path != "/api/v1/groups/available" {
			optional.Add(1)
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		now = now.Add(pollInterval)
		return []byte(`{"effective_rate_multiplier":1}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now }
	monitorTestTargets(t, m, func() []Target { return []Target{target} })
	if err := m.Refresh(t.Context()); err != nil || optional.Load() != 0 {
		t.Fatalf("optional observations ran after key budget expired: %d %v", optional.Load(), err)
	}

	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unblock)
	committed := make(chan struct{})
	slow, fast := monitorTestTarget(), monitorTestTarget()
	slow.SessionID, fast.SessionID = "slow", "fast"
	m2, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if session == "slow" && path == "/api/v1/groups/available" {
			close(entered)
			<-release
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, m2, func() []Target { return []Target{slow, fast} })
	m2.SetGenerationGuard(func(session string, _ uint64, commit func() error) error {
		return WithControl(m2.dir, func() error {
			err := commit()
			if session == "fast" {
				m2.mu.Lock()
				hasFast := false
				for _, account := range m2.snapshot.Accounts {
					hasFast = hasFast || account.SessionID == "fast" && !account.LastSuccess.IsZero()
				}
				m2.mu.Unlock()
				if hasFast {
					select {
					case <-committed:
					default:
						close(committed)
					}
				}
			}
			return err
		})
	})
	slow.Generation, fast.Generation = 1, 1
	done := make(chan error, 1)
	go func() { done <- m2.Refresh(t.Context()) }()
	<-entered
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		unblock()
		<-done
		t.Fatal("fast account waited for slow account before commit")
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(m2.Snapshot().Accounts) != 2 || len(m2.baselines) != 2 {
		t.Fatal("shared snapshot merge lost another account")
	}
}
