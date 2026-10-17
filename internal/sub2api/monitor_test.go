package sub2api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/yetone/magpie/internal/filelock"
)

func monitorTestTarget() Target {
	return Target{
		ID: "source", Name: "Source", Site: "https://panel.example", SessionID: "session",
		GroupID: "5", GroupName: "Group A", Context: "test-network",
		Keys: []Key{{ID: "key-fingerprint", Value: "secret-api-key", Context: "test-key-network"}},
	}
}

func monitorTestTargets(t *testing.T, monitor *Monitor, targets func() []Target) {
	t.Helper()
	for _, target := range targets() {
		settings, err := monitor.Settings(target.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !settings.Configured {
			if err := monitor.SetAccount(target.SessionID, true, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	monitor.SetTargets(targets)
}

func monitorTestPanel(_ context.Context, _ string, path string) ([]byte, error) {
	switch {
	case path == "/api/v1/groups/available":
		return []byte(`{"data":[{"id":5,"name":"Group A"}]}`), nil
	case path == "/api/v1/settings/public":
		return []byte(`{"channel_monitor_mode":"v2"}`), nil
	case strings.Contains(path, "/matrix?"):
		return []byte(`{"data":{"items":[{"group_id":5,"group_name":"Group A","metrics":{"error_rate":0.1}}]}}`), nil
	default:
		return nil, fmt.Errorf("unexpected path %s", path)
	}
}

func monitorTestBilling(_ context.Context, _ Target, _ Key) ([]byte, error) {
	return []byte(`{"effective_rate_multiplier":1.2}`), nil
}

func monitorTestObservations(t *testing.T, m *Monitor, groups []Group) map[string]observationResult {
	t.Helper()
	targets, err := m.currentTargets()
	if err != nil || len(targets) == 0 {
		t.Fatalf("missing observation test targets: %v", err)
	}
	settings, err := m.Settings(targets[0].SessionID)
	if err != nil {
		t.Fatal(err)
	}
	ticket := monitorTicket{
		session: targets[0].SessionID, settings: settings.Revision,
		generation: targets[0].Generation, targets: copyMonitorTargets(targets),
	}
	ctx := context.WithValue(t.Context(), monitorTicketKey{}, ticket)
	return m.observations(ctx, ticket.session, groups)
}

func TestMonitorAllGroupsPollWithoutKeysOrWatches(t *testing.T) {
	dir := t.TempDir()
	phase, reads, notices := 0, 0, 0
	panel := func(_ context.Context, _, path string) ([]byte, error) {
		if path != "/api/v1/groups/available" {
			t.Fatalf("unexpected request: %s", path)
		}
		reads++
		return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A","rate_multiplier":1,"enabled":true},{"id":6,"name":"Group B","rate_multiplier":%d,"enabled":%t}]}`, phase+1, phase%2 == 0)), nil
	}
	billing := func(context.Context, Target, Key) ([]byte, error) {
		t.Fatal("an account without linked keys must not query billing")
		return nil, nil
	}
	target := Target{ID: "watch:session", SessionID: "session", Site: "https://panel.example", Context: "test-network"}
	targets := func() []Target { return []Target{target} }
	monitor, err := NewMonitor(dir, panel, billing)
	if err != nil {
		t.Fatal(err)
	}
	monitor.SetTargets(targets)
	refresh := func() {
		t.Helper()
		if err := monitor.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	if reads != 0 {
		t.Fatal("an unconfigured account was polled")
	}
	if err := monitor.SetAccount(target.SessionID, true, nil); err != nil {
		t.Fatal(err)
	}
	monitor.notify = func(Change) { notices++ }
	refresh()
	snapshot := monitor.Snapshot()
	if reads != 1 || len(snapshot.Accounts) != 1 || len(snapshot.Accounts[0].Groups) != 2 || notices != 0 {
		t.Fatalf("all groups need a silent initial baseline: reads=%d notices=%d snapshot=%+v", reads, notices, snapshot)
	}
	for phase = 1; phase <= 2; phase++ {
		if phase == 2 {
			monitor, err = NewMonitor(dir, panel, billing)
			if err != nil {
				t.Fatal(err)
			}
			monitor.SetTargets(targets)
			monitor.notify = func(Change) { notices++ }
		}
		refresh()
		snapshot = monitor.Snapshot()
		if reads != phase+1 || notices != phase || len(snapshot.Changes) != phase {
			t.Fatalf("all group changes must survive restart without watches: reads=%d notices=%d changes=%+v", reads, notices, snapshot.Changes)
		}
		items := snapshot.Changes[phase-1].Items
		if len(items) != 2 || items[0].Kind != "group_rate" || items[1].Kind != "group_enabled" ||
			items[0].GroupID != "6" || items[1].GroupID != "6" {
			t.Fatalf("an unlinked group lost its rate or enabled change: %+v", items)
		}
	}
	if err := monitor.SetAccount(target.SessionID, false, nil); err != nil {
		t.Fatal(err)
	}
	refresh()
	if reads != 3 || notices != 2 {
		t.Fatal("disabling account monitoring did not stop polling")
	}
}

func TestMonitorRefreshAndPreviewReadOnlyGroupsAndBilling(t *testing.T) {
	for _, preview := range []bool{false, true} {
		t.Run(fmt.Sprintf("preview=%t", preview), func(t *testing.T) {
			target := monitorTestTarget()
			target.Keys = append(target.Keys, Key{ID: "second-key", Value: "second-secret", Context: "second-key-network"})
			var groups, billing, optional atomic.Int64
			m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
				if path == "/api/v1/groups/available" {
					groups.Add(1)
				} else {
					optional.Add(1)
				}
				return monitorTestPanel(ctx, session, path)
			}, func(ctx context.Context, source Target, key Key) ([]byte, error) {
				billing.Add(1)
				return monitorTestBilling(ctx, source, key)
			})
			if err != nil {
				t.Fatal(err)
			}
			monitorTestTargets(t, m, func() []Target { return []Target{target} })
			var account MonitorAccount
			if preview {
				settings, err := m.Settings(target.SessionID)
				if err != nil {
					t.Fatal(err)
				}
				account, err = m.Preview(t.Context(), target.SessionID, target.Generation, settings.Revision, []Target{target})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err := m.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				account = m.Snapshot().Accounts[0]
			}
			if groups.Load() != 1 || billing.Load() != 2 || optional.Load() != 0 ||
				!account.Complete || account.Sources[0].Rate == nil {
				t.Fatalf("first release sampling must read only groups and billing: groups=%d billing=%d optional=%d account=%+v",
					groups.Load(), billing.Load(), optional.Load(), account)
			}
		})
	}
}

type monitorWaitContext struct {
	context.Context
	once sync.Once
	seen chan struct{}
}

func (c *monitorWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.seen) })
	return c.Context.Done()
}

func TestMonitorBaselineChangeRestartAndFailure(t *testing.T) {
	dir := t.TempDir()
	rate := atomic.Int64{}
	rate.Store(12)
	fail := atomic.Bool{}
	monitor, err := NewMonitor(dir, monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
		if fail.Load() {
			return nil, errors.New("connection reset secret-api-key")
		}
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%g}`, float64(rate.Load())/10)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	var notified atomic.Int64
	monitor.notify = func(Change) { notified.Add(1) }
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 0 || notified.Load() != 0 || *snapshot.Accounts[0].Sources[0].Rate != 1.2 {
		t.Fatalf("first baseline = %#v; notifications = %d", snapshot, notified.Load())
	}
	rate.Store(15)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot = monitor.Snapshot()
	if len(snapshot.Changes) != 1 || len(snapshot.Changes[0].Items) != 1 || snapshot.Changes[0].Items[0].Kind != "key_rate" || notified.Load() != 1 {
		t.Fatalf("rate change = %#v; notifications = %d", snapshot.Changes, notified.Load())
	}
	restarted, err := NewMonitor(dir, monitorTestPanel, monitor.billing)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, restarted, func() []Target { return []Target{monitorTestTarget()} })
	fail.Store(true)
	if err := restarted.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot = restarted.Snapshot()
	key := snapshot.Accounts[0].Sources[0].Keys[0]
	if key.Rate != nil || key.LastRate == nil || *key.LastRate != 1.5 || key.Error == "" || key.LastRateAt.IsZero() || len(snapshot.Changes) != 1 || snapshot.Accounts[0].Sources[0].Rate != nil {
		t.Fatalf("failed read lost last success or exposed applicable stale rate: %#v", snapshot)
	}
	body, err := os.ReadFile(filepath.Join(dir, "monitor.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "secret-api-key") {
		t.Fatalf("snapshot leaked key: %s", body)
	}
}

func TestMonitorKeysKeepIndependentGroupsAndBaselines(t *testing.T) {
	target := monitorTestTarget()
	target.GroupID, target.GroupName = "", ""
	target.Keys[0].GroupID, target.Keys[0].GroupName = "5", "Group A"
	target.Keys = append(target.Keys, Key{
		ID: "second", GroupID: "6", GroupName: "Group B",
		Value: "second-secret", Context: "second-network",
	})
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	first := now
	rates := map[string]int{target.Keys[0].ID: 1, "second": 2}
	groupRates := [3]int{1, 2, 3}
	monitor, err := NewMonitor(t.TempDir(), func(context.Context, string, string) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A","rate_multiplier":%d},{"id":6,"name":"Group B","rate_multiplier":%d},{"id":7,"name":"Group C","rate_multiplier":%d}]}`,
			groupRates[0], groupRates[1], groupRates[2])), nil
	}, func(_ context.Context, _ Target, key Key) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rates[key.ID])), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitor.now = func() time.Time { return now }
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	var notices []Change
	monitor.notify = func(change Change) { notices = append(notices, change) }
	refresh := func() SourceSnapshot {
		t.Helper()
		if err := monitor.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return monitor.Snapshot().Accounts[0].Sources[0]
	}
	source := refresh()
	if source.Conflict || source.Rate != nil || len(source.Keys) != 2 ||
		source.Keys[0].GroupID != "5" || source.Keys[0].GroupName != "Group A" ||
		source.Keys[1].GroupID != "6" || source.Keys[1].GroupName != "Group B" ||
		source.Keys[0].Rate == nil || *source.Keys[0].Rate != 1 ||
		source.Keys[1].Rate == nil || *source.Keys[1].Rate != 2 || len(notices) != 0 {
		t.Fatalf("different key groups or rates were merged: source=%+v keys=%+v notices=%+v", source, source.Keys, notices)
	}

	now = now.Add(time.Minute)
	groupRates[0], groupRates[1] = 4, 5
	rates[target.Keys[0].ID], rates["second"] = 3, 4
	refresh()
	if len(notices) != 1 || len(notices[0].Items) != 4 {
		t.Fatalf("each key group and billing change must be monitored: %+v", notices)
	}
	for _, item := range notices[0].Items {
		if item.Kind == "key_rate" && ((item.KeyID == target.Keys[0].ID && (item.GroupID != "5" || item.GroupName != "Group A")) ||
			(item.KeyID == "second" && (item.GroupID != "6" || item.GroupName != "Group B"))) {
			t.Fatalf("key notification used another key's group: %+v", item)
		}
		if item.BeforeAt != first || item.AfterAt != now {
			t.Fatalf("independent group or key history was lost: %+v", item)
		}
	}

	prior := now
	now = now.Add(time.Minute)
	target.Keys[0].GroupID, target.Keys[0].GroupName = "7", "Group C"
	rates[target.Keys[0].ID], rates["second"] = 8, 9
	source = refresh()
	if len(notices) != 2 || len(notices[1].Items) != 1 {
		t.Fatalf("rebinding one key reset another or compared unrelated groups: %+v", notices)
	}
	item := notices[1].Items[0]
	if item.Kind != "key_rate" || item.KeyID != "second" || item.GroupID != "6" || item.GroupName != "Group B" ||
		item.Before != "4" || item.After != "9" || item.BeforeAt != prior || item.AfterAt != now ||
		source.Keys[0].GroupID != "7" || source.Conflict {
		t.Fatalf("rebinding did not preserve the other key's history: item=%+v source=%+v", item, source)
	}
}

func TestMonitorPartialGroupsDoNotRemoveAndEmptyCompleteDoes(t *testing.T) {
	var phase atomic.Int64
	panel := func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			switch phase.Load() {
			case 0:
				return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":6,"name":"Group B"}]}`), nil
			case 1:
				return []byte(`{"data":[{"id":5,"name":"Group A"}],"has_more":true,"total":2}`), nil
			default:
				return []byte(`{"data":[]}`), nil
			}
		}
		return monitorTestPanel(ctx, session, path)
	}
	monitor, err := NewMonitor(t.TempDir(), panel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	for _, value := range []int64{0, 1} {
		phase.Store(value)
		if err := monitor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	account := monitor.Snapshot().Accounts[0]
	if account.Complete || len(account.Groups) != 2 || account.Groups[1].Visible != nil || account.Groups[1].LastVisible == nil || !*account.Groups[1].LastVisible || len(monitor.Snapshot().Changes) != 0 {
		t.Fatalf("partial page removed unseen group: %#v", account)
	}
	phase.Store(2)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 1 || len(snapshot.Changes[0].Items) != 2 || *snapshot.Accounts[0].Groups[1].Visible {
		t.Fatalf("complete empty list did not record no-longer-visible: %#v", snapshot)
	}
}

func TestMonitorSharedBillingUnknownAndZero(t *testing.T) {
	var phase atomic.Int64
	var calls atomic.Int64
	monitor, err := NewMonitor(t.TempDir(), monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
		calls.Add(1)
		if phase.Load() == 1 {
			return []byte(`{"data":{}}`), nil
		}
		return []byte(`{"effective_rate_multiplier":0,"resolved_rate_multiplier":2}`), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target {
		a, b := monitorTestTarget(), monitorTestTarget()
		b.ID = "second"
		return []Target{a, b}
	})
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || monitor.Snapshot().Accounts[0].Sources[0].Rate == nil || *monitor.Snapshot().Accounts[0].Sources[0].Rate != 0 {
		t.Fatalf("shared billing or real zero was lost: %d %#v", calls.Load(), monitor.Snapshot())
	}
	phase.Store(1)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 0 || snapshot.Accounts[0].Sources[0].Rate != nil || snapshot.Accounts[0].Sources[0].Keys[0].Rate != nil || snapshot.Accounts[0].Sources[0].Keys[0].LastRate == nil || *snapshot.Accounts[0].Sources[0].Keys[0].LastRate != 0 {
		t.Fatalf("missing rate became successful zero: %#v", snapshot)
	}
}

func TestMonitorMissingGroupFieldsAreNotFreshOldValues(t *testing.T) {
	var phase atomic.Bool
	panel := func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			if phase.Load() {
				return []byte(`{"data":[{"id":5,"name":"Group A"}]}`), nil
			}
			return []byte(`{"data":[{"id":5,"name":"Group A","rate_multiplier":1.5,"is_enabled":true}]}`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}
	monitor, err := NewMonitor(t.TempDir(), panel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := monitor.Snapshot().Accounts[0].Groups[0]
	if before.Rate == nil || before.Enabled == nil {
		t.Fatal("fixture did not supply confirmed group fields")
	}
	phase.Store(true)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	after := snapshot.Accounts[0].Groups[0]
	if after.Rate != nil || after.Enabled != nil || len(snapshot.Changes) != 0 {
		t.Fatalf("missing fields were displayed as current confirmed values: %+v", after)
	}
}

func TestMonitorSnapshot401UsesRawV1BeforeRefresh(t *testing.T) {
	var normalV1, rawV1, snapshots atomic.Int64
	panel := func(ctx context.Context, session, path string) ([]byte, error) {
		if strings.Contains(path, "/matrix?") {
			return nil, &Error{Status: 404}
		}
		if path == "/api/v1/channel-monitors" {
			normalV1.Add(1)
			return nil, errors.New("normal V1 must not refresh a working token")
		}
		return monitorTestPanel(ctx, session, path)
	}
	monitor, err := NewMonitor(t.TempDir(), panel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitor.SetRawReader(func(_ context.Context, _, path string) ([]byte, error) {
		if strings.Contains(path, "/snapshot?") {
			snapshots.Add(1)
			if !strings.Contains(path, "group_id=5") {
				t.Errorf("snapshot omitted group filter: %s", path)
			}
			return nil, &Error{Status: 401}
		}
		if path == "/api/v1/channel-monitors" {
			rawV1.Add(1)
			return []byte(`{"data":[{"id":999,"group_id":5,"name":"Group A","availability_7d":98,"latency_ms":123,"ping_latency_ms":1}]}`), nil
		}
		return nil, fmt.Errorf("unexpected raw path %s", path)
	})
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	result := monitorTestObservations(t, monitor, []Group{{ID: "5", Name: "Group A"}})["5"]
	observation := result.value
	if result.err != nil || observation == nil || snapshots.Load() != 1 || rawV1.Load() != 1 || normalV1.Load() != 0 || observation.Source != "v1" || observation.Match != "id" || observation.Window != "7d" || *observation.Availability != .98 || *observation.ProbeLatencyMS != 123 || observation.TTFTP50MS != nil {
		t.Fatalf("401 compatibility = %#v; snapshot/raw/normal=%d/%d/%d", observation, snapshots.Load(), rawV1.Load(), normalV1.Load())
	}
}

func TestMonitorMatrix429AndChallengeDoNotFanOut(t *testing.T) {
	for _, detail := range []*Error{{Status: 429, RetryAfter: 10 * time.Minute}, {Status: 403, Challenge: true}} {
		t.Run(fmt.Sprint(detail.Status), func(t *testing.T) {
			var matrix, fallback atomic.Int64
			monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
				if strings.Contains(path, "/matrix?") {
					matrix.Add(1)
					return nil, detail
				}
				if strings.Contains(path, "/snapshot?") || path == "/api/v1/channel-monitors" {
					fallback.Add(1)
					return nil, fmt.Errorf("fallback must not happen")
				}
				return monitorTestPanel(ctx, session, path)
			}, monitorTestBilling)
			if err != nil {
				t.Fatal(err)
			}
			monitorTestTargets(t, monitor, func() []Target {
				a, b := monitorTestTarget(), monitorTestTarget()
				b.ID, b.GroupID, b.GroupName = "second", "6", "Group B"
				return []Target{a, b}
			})
			monitorTestObservations(t, monitor, []Group{{ID: "5", Name: "Group A"}, {ID: "6", Name: "Group B"}})
			monitorTestObservations(t, monitor, []Group{{ID: "5", Name: "Group A"}, {ID: "6", Name: "Group B"}})
			if matrix.Load() != 1 || fallback.Load() != 0 {
				t.Fatalf("matrix=%d fallback=%d", matrix.Load(), fallback.Load())
			}
		})
	}
}

func TestMonitorOwnerAndConcurrentRefreshAreStrict(t *testing.T) {
	dir := t.TempDir()
	var calls atomic.Int64
	entered, release := make(chan struct{}), make(chan struct{})
	monitor, err := NewMonitor(dir, func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" && calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	unlock, held := filelock.Lock(filepath.Join(dir, "monitor-owner.lock"), 0)
	if !held {
		t.Fatal("could not take fixture owner lock")
	}
	if err := monitor.Refresh(context.Background()); err == nil {
		t.Fatal("manual non-owner refresh reported success")
	}
	if calls.Load() != 0 {
		t.Fatal("non-owner sent requests")
	}
	unlock()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(1)
	go func() { defer wg.Done(); errs <- monitor.Refresh(context.Background()) }()
	<-entered
	waiting := &monitorWaitContext{Context: context.Background(), seen: make(chan struct{})}
	wg.Add(1)
	go func() { defer wg.Done(); errs <- monitor.Refresh(waiting) }()
	<-waiting.seen
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent refresh sent %d groups requests, want one", calls.Load())
	}
}

func TestMonitorRequestConcurrencyBound(t *testing.T) {
	var active, peak atomic.Int64
	entered, release := make(chan struct{}, 16), make(chan struct{})
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		n := active.Add(1)
		for current := peak.Load(); n > current; current = peak.Load() {
			if peak.CompareAndSwap(current, n) {
				break
			}
		}
		defer active.Add(-1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target {
		targets := make([]Target, 8)
		for i := range targets {
			targets[i] = monitorTestTarget()
			targets[i].SessionID = fmt.Sprint(i)
		}
		return targets
	})
	done := make(chan error, 1)
	go func() { done <- monitor.Refresh(context.Background()) }()
	for range 4 {
		<-entered
	}
	if got := peak.Load(); got != 4 {
		t.Fatalf("peak=%d, want 4", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if peak.Load() > 4 {
		t.Fatalf("request concurrency exceeded four: %d", peak.Load())
	}
}

func TestMonitorParsingPreservesIdentityAndLatestBucket(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"id":5,"name":"Group A"}],"hasMore":true}`,
		`{"data":[{"id":5,"name":"Group A"}],"total":"unknown"}`,
		`{"data":[{"id":5,"name":"Group A"}],"offset":0}`,
	} {
		if groups, complete, err := parseGroups([]byte(body)); err != nil || complete || len(groups) != 1 {
			t.Fatalf("unconfirmed pagination became complete: %s => %#v %v %v", body, groups, complete, err)
		}
	}
	body := []byte(`{"metrics":{"error_rate":0.9},"buckets":[
		{"bucket_start":"2026-10-16T01:00:00Z","metrics":{"error_rate":0.1,"cache_rate":0.8,"cache_rate_trimmed":0.2,"ttft":{"p50_ms":45}}},
		{"bucket_start":"2026-10-15T01:00:00Z","metrics":{"error_rate":0.7}},
		{"metrics":{"error_rate":0.6}}]}`)
	row, err := monitorObject(body)
	if err != nil {
		t.Fatal(err)
	}
	result := parseV2(row, "v2-matrix", "group", "id")
	if *result.Availability != .9 || *result.CacheRate != .8 || *result.TTFTP50MS != 45 || result.SampledAt.Day() != 16 {
		t.Fatalf("latest timestamped bucket not used: %#v", result)
	}
	for _, body := range []string{
		`{"items":[{"group_id":5},{"group_id":5}]}`,
		`{"items":[{"id":5,"name":"Group A"}]}`,
	} {
		if _, err := parseMatrix([]byte(body)); err == nil {
			t.Fatalf("ambiguous matrix accepted: %s", body)
		}
	}
	// A record's own id is never a group id; the contradictory explicit group wins.
	v1 := []byte(`{"data":[{"id":5,"group_id":6,"name":"Group A","availability_7d":99},{"id":7,"name":"Group A","latency_ms":0}]}`)
	result, err = parseV1(v1, Group{ID: "5", Name: "Group A"})
	if err != nil || result.Match != "approximate" || result.ProbeLatencyMS == nil || *result.ProbeLatencyMS != 0 || result.Availability != nil {
		t.Fatalf("record identity or real zero lost: %#v, %v", result, err)
	}
	if _, err := parseV1([]byte(`[{"name":"Group A one","latency_ms":1},{"name":"Group A two","latency_ms":2}]`), Group{Name: "Group A"}); err == nil {
		t.Fatal("ambiguous approximate name was accepted")
	}
}

func TestMonitorUnreadableStateIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "monitor.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"snapshot":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewMonitor(dir, monitorTestPanel, monitorTestBilling); err == nil {
		t.Fatal("corrupt baseline was treated as empty")
	}
	body, err := os.ReadFile(path)
	if err != nil || json.Valid(body) {
		t.Fatalf("original unreadable baseline changed: %s, %v", body, err)
	}
}

func TestMonitorRunUsesTargetsWithoutPageAndStops(t *testing.T) {
	monitor, err := NewMonitor(t.TempDir(), monitorTestPanel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	if err := monitor.SetAccount("session", true, []Group{{ID: "5", Name: "Group A"}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	committed := make(chan struct{})
	var once sync.Once
	// Run reaches the callback without a GUI refresh or display preference.
	monitorTestTargets(t, monitor, func() []Target {
		once.Do(func() { close(committed) })
		return []Target{monitorTestTarget()}
	})
	go func() {
		monitor.Run(ctx, nil, nil)
		close(done)
	}()
	<-committed
	// Join its current round, so cancellation cannot hide an unfinished request.
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Accounts) != 1 || len(snapshot.Accounts[0].Watched) != 1 || snapshot.Accounts[0].LastSuccess.IsZero() {
		t.Fatalf("background result = %#v", snapshot)
	}
	cancel()
	<-done
}

func TestMonitorRunKeepsMinuteRoundsAfterStagger(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int64
		monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
			if path == "/api/v1/groups/available" {
				reads.Add(1)
				time.Sleep(3 * time.Second)
			}
			return monitorTestPanel(ctx, session, path)
		}, monitorTestBilling)
		if err != nil {
			t.Fatal(err)
		}
		monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			monitor.Run(ctx, nil, nil)
		}()
		defer func() {
			cancel()
			<-done
		}()
		time.Sleep(2*pollInterval + 20*time.Second)
		synctest.Wait()
		if got := reads.Load(); got != 3 {
			t.Fatalf("minute sampling skipped a round after account stagger: groups reads = %d, want startup plus two rounds", got)
		}
	})
}

func TestMonitorEnableWakesRunner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads atomic.Int64
		entered, release := make(chan struct{}), make(chan struct{})
		monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
			if reads.Add(1) == 2 {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return monitorTestPanel(ctx, session, path)
		}, monitorTestBilling)
		if err != nil {
			t.Fatal(err)
		}
		monitor.SetTargets(func() []Target { return []Target{monitorTestTarget()} })
		if err := monitor.SetAccount("session", false, nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			monitor.Run(ctx, nil, nil)
		}()
		defer func() {
			cancel()
			<-done
		}()
		synctest.Wait()
		if reads.Load() != 0 {
			t.Fatal("disabled monitoring sent a request")
		}
		if err := monitor.SetAccount("session", true, nil); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if got := reads.Load(); got != 1 {
			t.Fatalf("enabling monitoring did not sample before the next minute: groups reads = %d, want 1", got)
		}
		monitor.Wake("session")
		<-entered
		for range 20 {
			monitor.Wake("session")
		}
		close(release)
		synctest.Wait()
		if got := reads.Load(); got != 2 {
			t.Fatalf("account wake did not merge reads before NextPoll: groups reads = %d, want 2", got)
		}
		if err := monitor.SetAccount("session", false, nil); err != nil {
			t.Fatal(err)
		}
		monitor.Wake("session")
		synctest.Wait()
		if got := reads.Load(); got != 2 {
			t.Fatalf("account wake enabled disabled monitoring: groups reads = %d, want 2", got)
		}
	})
}

func TestMonitorWakeDuringReadUsesNewTargetsOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		target := monitorTestTarget()
		entered, release := make(chan struct{}), make(chan struct{})
		var reads atomic.Int64
		monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
			if reads.Add(1) == 1 {
				close(entered)
				<-release
			}
			return monitorTestPanel(ctx, session, path)
		}, monitorTestBilling)
		if err != nil {
			t.Fatal(err)
		}
		monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			monitor.Run(ctx, nil, nil)
		}()
		defer func() {
			cancel()
			<-done
		}()
		<-entered
		target.BindingVersion = "new-confirmation"
		for range 20 {
			monitor.Wake("session")
		}
		synctest.Wait()
		if reads.Load() != 1 {
			t.Fatal("account wake overlapped an active read")
		}
		close(release)
		synctest.Wait()
		if got := reads.Load(); got != 2 {
			t.Fatalf("target change during read was not sampled once: groups reads = %d, want 2", got)
		}
		snapshot := monitor.Snapshot()
		if len(snapshot.Accounts) != 1 || len(snapshot.Accounts[0].Sources) != 1 ||
			snapshot.Accounts[0].Sources[0].BindingVersion != target.BindingVersion {
			t.Fatalf("wake did not commit the newly confirmed targets: %+v", snapshot)
		}
	})
}

func TestMonitorWakeKeepsMinuteRoundsForAllAccounts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var first, second atomic.Int64
		entered, release := make(chan struct{}), make(chan struct{})
		monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
			if session == "session" {
				if first.Add(1) == 2 {
					close(entered)
					select {
					case <-release:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}
			} else {
				second.Add(1)
			}
			return monitorTestPanel(ctx, session, path)
		}, monitorTestBilling)
		if err != nil {
			t.Fatal(err)
		}
		a, b := monitorTestTarget(), monitorTestTarget()
		b.SessionID = "second"
		monitorTestTargets(t, monitor, func() []Target { return []Target{a, b} })
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			monitor.Run(ctx, nil, nil)
		}()
		defer func() { cancel(); <-done }()
		time.Sleep(30 * time.Second)
		synctest.Wait()
		monitor.Wake(a.SessionID)
		<-entered
		for range 20 {
			monitor.Wake(a.SessionID)
		}
		close(release)
		synctest.Wait()
		if first.Load() != 2 || second.Load() != 1 {
			t.Fatalf("wake sampled unrelated accounts: reads = %d/%d, want 2/1", first.Load(), second.Load())
		}
		time.Sleep(45 * time.Second)
		synctest.Wait()
		if first.Load() != 3 || second.Load() != 2 {
			t.Fatalf("wake skipped or delayed a minute round: reads = %d/%d, want 3/2", first.Load(), second.Load())
		}
	})
}

func TestMonitorReloginRecoversAuthWithoutClearingHardLimits(t *testing.T) {
	for _, detail := range []*Error{
		{Status: 401}, {Status: 403, Challenge: true},
		{Kind: KindAuth}, {Kind: KindChallenge}, {Kind: KindUnknown, Code: "REFRESH_UNCERTAIN"},
		{Status: 429}, {Status: 401, RetryAfter: time.Hour},
		{Status: 403, Challenge: true, RetryAfter: time.Hour},
	} {
		t.Run(fmt.Sprintf("%s/%d/%t/%s", detail.Kind, detail.Status, detail.Challenge, detail.RetryAfter), func(t *testing.T) {
			target := monitorTestTarget()
			target.Generation = 1
			phase, groups, billing := 0, 0, 0
			dir := t.TempDir()
			panel := func(ctx context.Context, session, path string) ([]byte, error) {
				groups++
				if phase == 1 {
					return nil, detail
				}
				return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A","rate_multiplier":%d}]}`, phase+1)), nil
			}
			keyRead := func(context.Context, Target, Key) ([]byte, error) {
				billing++
				return nil, &Error{Status: 401}
			}
			monitor, err := NewMonitor(dir, panel, keyRead)
			if err != nil {
				t.Fatal(err)
			}
			targets := func() []Target { return []Target{target} }
			monitorTestTargets(t, monitor, targets)
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			phase = 1
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			// Persisted cooldowns must recover on a new login, even after restart.
			monitor, err = NewMonitor(dir, panel, keyRead)
			if err != nil {
				t.Fatal(err)
			}
			monitor.SetTargets(targets)
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			if groups != 2 || billing != 1 {
				t.Fatalf("same login bypassed cooldowns: groups/billing = %d/%d", groups, billing)
			}
			target.Generation, phase = 2, 2
			if detail.Status == 429 || detail.RetryAfter > 0 {
				target.Context = "new-panel-network"
			}
			if err := monitor.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			wantGroups := 3
			if detail.Status == 429 || detail.RetryAfter > 0 {
				wantGroups = 2
			}
			if groups != wantGroups || billing != 1 {
				t.Fatalf("relogin recovery or hard limit failed: groups/billing = %d/%d, want %d/1", groups, billing, wantGroups)
			}
			if wantGroups == 3 {
				changes := monitor.Snapshot().Changes
				if len(changes) != 2 || len(changes[1].Items) != 1 || changes[1].Items[0].Kind != "group_rate" ||
					changes[1].Items[0].Before != "1" || changes[1].Items[0].After != "3" {
					t.Fatalf("relogin lost the valid group baseline: %+v", changes)
				}
			}
		})
	}
}

func TestMonitorDisableStopsRemainingRequestsAndPersists(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var billing atomic.Int64
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return monitorTestPanel(ctx, session, path)
	}, func(ctx context.Context, target Target, key Key) ([]byte, error) {
		billing.Add(1)
		return monitorTestBilling(ctx, target, key)
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	done := make(chan error, 1)
	go func() { done <- monitor.Refresh(context.Background()) }()
	<-entered
	if err := monitor.SetAccount("session", false, []Group{{ID: "5", Name: "Group A"}}); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err == nil {
		t.Fatal("disabled in-flight round reported a current result")
	}
	if billing.Load() != 0 || monitor.Snapshot().Accounts[0].Enabled {
		t.Fatalf("disabled account continued billing: %d %#v", billing.Load(), monitor.Snapshot())
	}
	restarted, err := NewMonitor(monitor.dir, monitorTestPanel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	enabled, groups := restarted.Config("session")
	if enabled || len(groups) != 1 || groups[0].ID != "5" || groups[0].Observation != nil {
		t.Fatalf("settings were not preserved as minimal references: %v %#v", enabled, groups)
	}
}

func TestMonitorKeepsLastHundredChangesAndNotifiesOnce(t *testing.T) {
	var rate atomic.Int64
	monitor, err := NewMonitor(t.TempDir(), monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate.Load())), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	var notifications atomic.Int64
	monitor.notify = func(Change) { notifications.Add(1) }
	for i := range 103 {
		rate.Store(int64(i))
		if err := monitor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 100 || notifications.Load() != 102 || snapshot.Changes[0].Items[0].After != "3" || snapshot.Changes[99].Items[0].After != "102" {
		t.Fatalf("change retention or duplicate notification: %d %d", len(snapshot.Changes), notifications.Load())
	}
}

func TestMonitorUnboundKeysDoNotInferAGroup(t *testing.T) {
	var extra atomic.Int64
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path != "/api/v1/groups/available" {
			extra.Add(1)
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	target.GroupID, target.GroupName = "", ""
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := monitor.Snapshot().Accounts[0]
	if extra.Load() != 0 || len(account.Groups) != 1 || account.Groups[0].Observation != nil || account.Sources[0].GroupID != "" || account.Sources[0].Rate == nil {
		t.Fatalf("unbound source inferred a group or skipped billing: %#v; extra=%d", account, extra.Load())
	}
}

func TestMonitorSnapshotAndV1UnauthorizedRenewOnlyAfterRawFallback(t *testing.T) {
	var normalV1, rawV1 atomic.Int64
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if strings.Contains(path, "/matrix?") {
			return nil, &Error{Status: 404}
		}
		if path == "/api/v1/channel-monitors" {
			normalV1.Add(1)
			return []byte(`[{"group_id":5,"name":"Group A","availability_7d":99}]`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	monitor.SetRawReader(func(_ context.Context, _, path string) ([]byte, error) {
		if path == "/api/v1/channel-monitors" {
			rawV1.Add(1)
		}
		return nil, &Error{Status: 401}
	})
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	result := monitorTestObservations(t, monitor, []Group{{ID: "5", Name: "Group A"}})["5"]
	if rawV1.Load() != 1 || normalV1.Load() != 1 || result.value == nil || result.err != nil {
		t.Fatalf("renewal did not follow raw compatibility check: raw/normal=%d/%d %+v", rawV1.Load(), normalV1.Load(), result)
	}
}

func TestMonitorGlobalSnapshotIsNotCopiedIntoAGroup(t *testing.T) {
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if strings.Contains(path, "/matrix?") {
			return nil, &Error{Status: 404}
		}
		if strings.Contains(path, "/snapshot?") {
			if strings.Contains(path, "group_id=") {
				t.Error("name-only binding invented a group filter")
			}
			return []byte(`{"metrics":{"success_rate":0.5}}`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	target.GroupID, target.GroupName = "", "Unconfirmed group name"
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	result := monitorTestObservations(t, monitor, []Group{{Name: target.GroupName}})["name:"+target.GroupName]
	if result.value == nil || result.err != nil || result.value.Scope != "global" {
		t.Fatalf("global metrics were represented as group metrics: %+v", result)
	}
}

func TestMonitorFieldHistoryAndRecoveryUseLastValidBaseline(t *testing.T) {
	var phase int
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	first := now
	panel := func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			switch phase {
			case 0:
				return []byte(`{"data":[{"id":5,"name":"Group A","rate_multiplier":1,"is_enabled":true},{"id":6,"name":"Group B","rate_multiplier":1,"enabled":true}]}`), nil
			case 1:
				return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":6,"name":"Group B","rate_multiplier":2,"enabled":false}]}`), nil
			default:
				return []byte(`{"data":[{"id":5,"name":"Group A","rate_multiplier":2,"is_enabled":false},{"id":6,"name":"Group B","rate_multiplier":3,"enabled":true}]}`), nil
			}
		}
		return monitorTestPanel(ctx, session, path)
	}
	monitor, err := NewMonitor(t.TempDir(), panel, func(context.Context, Target, Key) ([]byte, error) {
		if phase == 1 {
			return []byte(`{}`), nil
		}
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, 1+phase)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitor.now = func() time.Time { return now }
	monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	phase, now = 1, now.Add(time.Minute)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	account := monitor.Snapshot().Accounts[0]
	group, key := account.Groups[0], account.Sources[0].Keys[0]
	if group.Rate != nil || group.Enabled != nil || group.LastRate == nil || *group.LastRate != 1 ||
		group.LastEnabled == nil || !*group.LastEnabled || !group.LastRateAt.Equal(first) || !group.LastEnabledAt.Equal(first) ||
		key.Rate != nil || key.LastRate == nil || *key.LastRate != 1 || !key.LastRateAt.Equal(first) {
		t.Fatalf("missing fields replaced current/history distinction: group=%+v key=%+v", group, key)
	}
	if changes := monitor.Snapshot().Changes; len(changes) != 1 || len(changes[0].Items) != 2 ||
		changes[0].Items[0].GroupID != "6" || changes[0].Items[1].GroupID != "6" {
		t.Fatalf("missing fields notified or unlinked group changes lost: %+v", changes)
	}
	phase, now = 2, now.Add(20*time.Minute)
	if err := monitor.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	account = snapshot.Accounts[0]
	if len(snapshot.Changes) != 2 || len(snapshot.Changes[1].Items) != 5 {
		t.Fatalf("recovery did not compare last valid fields: %+v", snapshot.Changes)
	}
	for _, item := range snapshot.Changes[1].Items {
		before := first
		if item.GroupID == "6" {
			before = first.Add(time.Minute)
		}
		if !item.BeforeAt.Equal(before) || !item.AfterAt.Equal(now) {
			t.Fatalf("change lost actual baseline times: %+v", item)
		}
	}
	if account.Sources[0].Rate == nil || account.BillingError != "" || account.ObservationError != "" ||
		account.Groups[0].Observation != nil {
		t.Fatalf("successful recovery lost billing or introduced deferred observations: %+v", account)
	}
}

func TestMonitorCompleteListBaselineSurvivesPartialRound(t *testing.T) {
	for _, firstPartial := range []bool{false, true} {
		t.Run(fmt.Sprint(firstPartial), func(t *testing.T) {
			phase := 0
			monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
				if path == "/api/v1/groups/available" {
					if phase == 0 {
						return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A"},{"id":6,"name":"Old"}],"has_more":%t}`, firstPartial)), nil
					}
					if phase == 1 {
						return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":7,"name":"New"}],"has_more":true}`), nil
					}
					return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":7,"name":"New"}]}`), nil
				}
				return monitorTestPanel(ctx, session, path)
			}, monitorTestBilling)
			if err != nil {
				t.Fatal(err)
			}
			monitorTestTargets(t, monitor, func() []Target { return []Target{monitorTestTarget()} })
			for phase = 0; phase < 3; phase++ {
				if err := monitor.Refresh(context.Background()); err != nil {
					t.Fatal(err)
				}
				snapshot := monitor.Snapshot()
				if phase < 2 && len(snapshot.Changes) != 0 {
					t.Fatalf("partial list generated changes: %+v", snapshot.Changes)
				}
			}
			snapshot := monitor.Snapshot()
			if firstPartial {
				if len(snapshot.Changes) != 0 {
					t.Fatalf("first complete list did not establish a silent baseline: %+v", snapshot.Changes)
				}
			} else if len(snapshot.Changes) != 1 || len(snapshot.Changes[0].Items) != 2 {
				t.Fatalf("partial round erased added/unavailable baseline: %+v", snapshot.Changes)
			}
		})
	}
}

func TestMonitorKeyLimitIsIndependentAndVersionsAreSampled(t *testing.T) {
	var groups, good atomic.Int64
	target := monitorTestTarget()
	target.Keys = append(target.Keys, Key{ID: "good", Value: "other-secret", Context: "other-key-network"})
	target.BindingVersion, target.Revision = "binding-7", "sampled"
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			groups.Add(1)
		}
		return monitorTestPanel(ctx, session, path)
	}, func(ctx context.Context, source Target, key Key) ([]byte, error) {
		if key.ID == "key-fingerprint" {
			return nil, &Error{Status: 429, RetryAfter: 10 * time.Minute}
		}
		good.Add(1)
		return monitorTestBilling(ctx, source, key)
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	for range 2 {
		if err := monitor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	target.BindingVersion, target.Revision = "binding-8", "new-current-target"
	account := monitor.Snapshot().Accounts[0]
	source := account.Sources[0]
	if groups.Load() != 2 || good.Load() != 2 ||
		source.BindingVersion != "binding-7" || source.Revision != "sampled" || source.Rate != nil ||
		source.Keys[1].Rate == nil || account.GroupsError != "" || account.ObservationError != "" || account.BillingError == "" {
		t.Fatalf("one key paused independent resources or versions were replaced: %+v; calls=%d/%d", account, groups.Load(), good.Load())
	}
}

func TestMonitorNewBindingsAndRequestIdentitiesStartSilently(t *testing.T) {
	target := monitorTestTarget()
	target.GroupID, target.GroupName = "", ""
	rate := 1
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if path == "/api/v1/groups/available" {
			return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A","rate_multiplier":%d,"enabled":true},{"id":6,"name":"Group B","rate_multiplier":%d,"enabled":false}]}`, rate, rate)), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	refresh := func() {
		t.Helper()
		if err := monitor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	target.GroupID, target.GroupName, rate = "5", "Group A", 2
	refresh()
	target.GroupID, target.GroupName, rate = "6", "Group B", 3
	refresh()
	if changes := monitor.Snapshot().Changes; len(changes) != 2 {
		t.Fatalf("rebindings lost independently monitored group changes: %+v", changes)
	} else {
		for _, change := range changes {
			if len(change.Items) != 2 {
				t.Fatalf("rebindings compared unrelated key samples: %+v", change)
			}
			for _, item := range change.Items {
				if item.Kind != "group_rate" {
					t.Fatalf("rebindings compared unrelated key samples: %+v", item)
				}
			}
		}
	}
	target.Name, target.Revision, target.BindingVersion, rate = "Renamed source", "price-edited", "binding-5", 4
	refresh()
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 3 || len(snapshot.Changes[2].Items) != 3 {
		t.Fatalf("source revision reset protocol baselines: %+v", snapshot.Changes)
	}
	for _, item := range snapshot.Changes[2].Items {
		if item.Kind == "key_rate" && item.GroupID != "6" {
			t.Fatalf("key change used the previous group: %+v", item)
		}
	}
	target.Keys[0].Value, rate = "rotated-key", 5
	// Groups change independently, while the rotated key starts a new baseline.
	refresh()
	last := monitor.Snapshot().Changes
	if len(last) != 4 || len(last[3].Items) != 2 ||
		last[3].Items[0].Kind != "group_rate" || last[3].Items[1].Kind != "group_rate" {
		t.Fatalf("rotated key retained previous identity: %+v", last)
	}
	target.Keys[0].Context, rate = "different-key-proxy", 6
	refresh()
	last = monitor.Snapshot().Changes
	if len(last) != 5 || len(last[4].Items) != 2 ||
		last[4].Items[0].Kind != "group_rate" || last[4].Items[1].Kind != "group_rate" {
		t.Fatalf("key request context retained previous identity: %+v", last)
	}
	target.Context, rate = "different-panel-network", 7
	refresh()
	if len(monitor.Snapshot().Changes) != 6 || len(monitor.Snapshot().Changes[5].Items) != 1 ||
		monitor.Snapshot().Changes[5].Items[0].Kind != "key_rate" {
		t.Fatalf("panel identity did not rebuild baseline: changes=%+v", monitor.Snapshot().Changes)
	}
}

func TestMonitorUnknownContextPreservesBaselineUntilIdentityReturns(t *testing.T) {
	target := monitorTestTarget()
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	first := now
	rate, unavailable := 1, false
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if unavailable {
			return nil, errors.New("session context is unavailable")
		}
		if path == "/api/v1/groups/available" {
			return []byte(fmt.Sprintf(`{"data":[{"id":5,"name":"Group A","rate_multiplier":%d,"enabled":true}]}`, rate)), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		if unavailable {
			return nil, errors.New("session context is unavailable")
		}
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	monitor.now = func() time.Time { return now }
	monitorTestTargets(t, monitor, func() []Target { return []Target{target} })
	coolKey := target.SessionID + "\x00/unused-resource"
	monitor.cool[coolKey] = cooldown{Until: now.Add(time.Hour)}
	refresh := func() MonitorAccount {
		t.Helper()
		if err := monitor.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		return monitor.Snapshot().Accounts[0]
	}
	refresh()
	baseline := monitor.baselines[target.SessionID]

	target.Context, target.Keys[0].Context, unavailable = "", "", true
	now = now.Add(pollInterval)
	account := refresh()
	key := account.Sources[0].Keys[0]
	if !account.GroupsKnown || account.GroupsError == "" || account.BillingError == "" ||
		key.Rate != nil || key.LastRate == nil || *key.LastRate != 1 || !key.LastRateAt.Equal(first) {
		t.Fatalf("unknown identity lost history or hid current errors: %+v; key=%+v", account, key)
	}
	if monitor.baselines[target.SessionID].Identity != baseline.Identity ||
		monitor.cool[coolKey].Until.IsZero() || len(monitor.Snapshot().Changes) != 0 {
		t.Fatalf("unknown identity replaced baseline or cleared cooldown: %+v", monitor.baselines[target.SessionID])
	}

	unavailable, rate = false, 2
	now = now.Add(3 * pollInterval)
	account = refresh()
	if account.Sources[0].Rate == nil || *account.Sources[0].Rate != 2 ||
		monitor.baselines[target.SessionID].Groups["5"].Rate == nil ||
		*monitor.baselines[target.SessionID].Groups["5"].Rate != 1 || len(monitor.Snapshot().Changes) != 0 {
		t.Fatalf("unknown identity committed a temporary sample: %+v", account)
	}

	target.Context, target.Keys[0].Context, rate = "test-network", "test-key-network", 3
	now = now.Add(pollInterval)
	refresh()
	changes := monitor.Snapshot().Changes
	if len(changes) != 1 || len(changes[0].Items) != 2 {
		t.Fatalf("returning identity lost changes: changes=%+v", changes)
	}
	for _, item := range changes[0].Items {
		if item.Before != "1" || item.After != "3" || !item.BeforeAt.Equal(first) || !item.AfterAt.Equal(now) {
			t.Fatalf("recovery compared an unknown-context sample: %+v", item)
		}
	}
}

func TestMonitorUnknownKeyContextPreservesBaselineUntilIdentityReturns(t *testing.T) {
	target := monitorTestTarget()
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	first := now
	rate, unavailable := 1, false
	m, err := NewMonitor(t.TempDir(), monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
		if unavailable {
			return nil, &Error{Kind: KindConflict, Message: "invalid source headers"}
		}
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now }
	monitorTestTargets(t, m, func() []Target { return []Target{target} })
	refresh := func() MonitorAccount {
		t.Helper()
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return m.Snapshot().Accounts[0]
	}
	refresh()
	target.Keys[0].Context, unavailable = "", true
	now = now.Add(pollInterval)
	failed := refresh()
	key := failed.Sources[0].Keys[0]
	if key.Error == "" || key.Rate != nil || key.LastRate == nil || *key.LastRate != 1 || !key.LastRateAt.Equal(first) {
		t.Errorf("an unknown key context discarded its dated last rate: %+v", key)
	}
	target.Keys[0].Context, unavailable, rate = "test-key-network", false, 3
	now = now.Add(3 * pollInterval)
	refresh()
	changes := m.Snapshot().Changes
	if len(changes) != 1 || len(changes[0].Items) != 1 || changes[0].Items[0].Kind != "key_rate" ||
		changes[0].Items[0].Before != "1" || changes[0].Items[0].After != "3" ||
		!changes[0].Items[0].BeforeAt.Equal(first) || !changes[0].Items[0].AfterAt.Equal(now) {
		t.Errorf("restoring the same key context lost its net rate change: %+v", changes)
	}
}

func TestMonitorReenabledKeyRestoresMatchingHistory(t *testing.T) {
	for _, preview := range []bool{false, true} {
		for _, state := range []string{"retained", "removed", "changed context", "unknown context"} {
			t.Run(fmt.Sprintf("preview=%t/%s", preview, state), func(t *testing.T) {
				target := monitorTestTarget()
				secondary := target.Keys[0]
				target.Keys = append([]Key{{ID: "primary", Value: "primary-secret", Context: "primary-network"}}, secondary)
				target.RetainedKeyIDs = []string{"primary", secondary.ID}
				now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
				first := now
				failSecondary, rate := false, 1
				billing := func(_ context.Context, _ Target, key Key) ([]byte, error) {
					if key.ID == secondary.ID {
						if failSecondary {
							return nil, errors.New("temporary billing failure")
						}
						return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
					}
					return []byte(`{"effective_rate_multiplier":1}`), nil
				}
				m, err := NewMonitor(t.TempDir(), monitorTestPanel, billing)
				if err != nil {
					t.Fatal(err)
				}
				m.now = func() time.Time { return now }
				monitorTestTargets(t, m, func() []Target { return []Target{target} })
				refresh := func() MonitorAccount {
					t.Helper()
					if err := m.Refresh(t.Context()); err != nil {
						t.Fatal(err)
					}
					account := m.Snapshot().Accounts[0]
					now = now.Add(20 * time.Minute)
					return account
				}
				refresh()
				target.Keys = target.Keys[:1]
				if state == "removed" {
					target.RetainedKeyIDs = []string{"primary"}
				}
				if disabled := refresh(); len(disabled.Sources[0].Keys) != 1 {
					t.Fatal("disabled key was still sampled")
				}
				m, err = NewMonitor(m.dir, monitorTestPanel, billing)
				if err != nil {
					t.Fatal(err)
				}
				m.now = func() time.Time { return now }
				monitorTestTargets(t, m, func() []Target { return []Target{target} })
				switch state {
				case "changed context":
					secondary.Context = "new-key-network"
				case "unknown context":
					secondary.Context = ""
				}
				target.Keys = append(target.Keys, secondary)
				target.RetainedKeyIDs = []string{"primary", secondary.ID}
				failSecondary = true
				sample := refresh
				if preview {
					if err := m.SetAccount(target.SessionID, false, nil); err != nil {
						t.Fatal(err)
					}
					sample = func() MonitorAccount {
						t.Helper()
						settings, err := m.Settings(target.SessionID)
						if err != nil {
							t.Fatal(err)
						}
						account, err := m.Preview(t.Context(), target.SessionID, target.Generation, settings.Revision, []Target{target})
						if err != nil {
							t.Fatal(err)
						}
						now = now.Add(20 * time.Minute)
						return account
					}
				}
				checkFailed := func(account MonitorAccount, wantHistory bool) {
					t.Helper()
					key := account.Sources[0].Keys[1]
					if key.Error == "" || key.Rate != nil || account.Sources[0].Rate != nil {
						t.Fatalf("failed read became a current rate: %+v", account.Sources[0])
					}
					if wantHistory {
						if key.LastRate == nil || *key.LastRate != 1 || !key.LastRateAt.Equal(first) || !key.LastSuccess.Equal(first) {
							t.Errorf("re-enabled key lost retained dated history: %+v", key)
						}
					} else if key.LastRate != nil || !key.LastRateAt.IsZero() || !key.LastSuccess.IsZero() {
						t.Errorf("key inherited history without a matching retained identity: %+v", key)
					}
				}
				checkFailed(sample(), state == "retained")
				if state == "unknown context" {
					target.Keys[1].Context = "test-key-network"
					checkFailed(sample(), true)
				}
				failSecondary, rate = false, 3
				if preview {
					if err := m.SetAccount(target.SessionID, true, nil); err != nil {
						t.Fatal(err)
					}
				}
				refresh()
				changes := m.Snapshot().Changes
				if state == "retained" || state == "unknown context" {
					if len(changes) != 1 || len(changes[0].Items) != 1 {
						t.Fatalf("retained key did not report exactly one rate change: %+v", changes)
					}
					item := changes[0].Items[0]
					if item.Kind != "key_rate" || item.KeyID != secondary.ID || item.Before != "1" || item.After != "3" ||
						!item.BeforeAt.Equal(first) {
						t.Errorf("retained key lost its original change baseline: %+v", item)
					}
				} else if len(changes) != 0 {
					t.Errorf("removed or changed key notified from an old baseline: %+v", changes)
				}
			})
		}
	}
}

func TestMonitorKeyActionsFollowCurrentSamples(t *testing.T) {
	for _, retain := range []bool{true, false} {
		name := "disabled"
		if !retain {
			name = "removed"
		}
		t.Run(name, func(t *testing.T) {
			target := monitorTestTarget()
			secondary := target.Keys[0]
			target.Keys = append([]Key{{ID: "primary", Value: "good-primary", Context: "primary-network"}}, secondary)
			target.RetainedKeyIDs = []string{"primary", secondary.ID}
			now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
			first := now
			failKey, failGroups, rate := false, false, 1
			m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
				if failGroups {
					return nil, &Error{Kind: KindChallenge, Status: 403, Challenge: true}
				}
				return monitorTestPanel(ctx, session, path)
			}, func(_ context.Context, _ Target, key Key) ([]byte, error) {
				if key.ID == secondary.ID {
					if failKey {
						return nil, &Error{Kind: KindAuth, Status: 401}
					}
					return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, rate)), nil
				}
				return []byte(`{"effective_rate_multiplier":1}`), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			m.now = func() time.Time { return now }
			monitorTestTargets(t, m, func() []Target { return []Target{target} })
			var notices []Change
			m.notify = func(change Change) { notices = append(notices, change) }
			refresh := func() MonitorAccount {
				t.Helper()
				if err := m.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				now = now.Add(20 * time.Minute)
				return m.Snapshot().Accounts[0]
			}
			refresh()
			failKey = true
			if account := refresh(); !account.NeedsAction || len(notices) != 1 {
				t.Fatalf("key failure did not request action once: %+v, notices=%+v", account, notices)
			}
			target.Keys = target.Keys[:1]
			if !retain {
				target.RetainedKeyIDs = []string{"primary"}
			}
			if account := refresh(); account.NeedsAction || account.Error != "" || len(account.Sources[0].Keys) != 1 {
				t.Errorf("an unsampled key kept an action after all current reads succeeded: %+v", account)
			}
			failGroups = true
			if account := refresh(); account.GroupsError == "" || len(notices) != 2 {
				t.Errorf("new panel verification was not notified: %+v, notices=%+v", account, notices)
			}
			failKey, failGroups, rate = false, false, 3
			target.Keys = append(target.Keys, secondary)
			target.RetainedKeyIDs = []string{"primary", secondary.ID}
			refresh()
			want := 2
			if retain {
				want = 3
			}
			if len(notices) != want {
				t.Fatalf("resumed key baseline was not kept only while disabled: %+v", notices)
			}
			if retain {
				items := notices[2].Items
				if len(items) != 1 || items[0].Kind != "key_rate" || items[0].Before != "1" ||
					items[0].After != "3" || !items[0].BeforeAt.Equal(first) {
					t.Fatalf("resumed key lost its dated rate baseline: %+v", items)
				}
			}
		})
	}
	t.Run("active without baseline", func(t *testing.T) {
		target := monitorTestTarget()
		now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
		var failure error = &Error{Kind: KindAuth, Status: 401}
		m, err := NewMonitor(t.TempDir(), monitorTestPanel, func(context.Context, Target, Key) ([]byte, error) {
			if failure != nil {
				return nil, failure
			}
			return monitorTestBilling(t.Context(), target, target.Keys[0])
		})
		if err != nil {
			t.Fatal(err)
		}
		m.now = func() time.Time { return now }
		monitorTestTargets(t, m, func() []Target { return []Target{target} })
		notices := 0
		m.notify = func(Change) { notices++ }
		for phase := range 3 {
			if phase == 1 {
				target.Keys[0].Context, failure = "", errors.New("network settings unavailable")
			} else if phase == 2 {
				target.Keys[0].Context, failure = "test-key-network", nil
			}
			if err := m.Refresh(t.Context()); err != nil {
				t.Fatal(err)
			}
			account := m.Snapshot().Accounts[0]
			if account.NeedsAction != (phase < 2) || notices != 1 {
				t.Errorf("active key action changed without successful verification at phase %d: %+v, notices=%d", phase, account, notices)
			}
			now = now.Add(20 * time.Minute)
		}
	})
}

func TestMonitorMetricHistoryKeepsPerFieldProvenance(t *testing.T) {
	first := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	availability, ttft, cache := .5, 20.0, .4
	history := monitorObservationHistory(nil, &Observation{
		Source: "v2-snapshot", Scope: "global", Window: "90m", Availability: &availability, TTFTP50MS: &ttft,
	}, first)
	now := first.Add(20 * time.Minute)
	history = monitorObservationHistory(history, &Observation{
		Source: "v2-snapshot", Scope: "global", Window: "90m", CacheRate: &cache,
	}, now)
	old, current := history["availability"], history["cacheRate"]
	if old.Availability == nil || *old.Availability != .5 || old.Scope != "global" ||
		!old.ObservedAt.Equal(first) || old.CacheRate != nil || !history["ttftP50Ms"].ObservedAt.Equal(first) ||
		current.CacheRate == nil || !current.ObservedAt.Equal(now) || current.Availability != nil {
		t.Fatalf("observations lost per-field provenance: %+v", history)
	}
}

func TestBillingRate(t *testing.T) {
	for _, body := range []string{
		`{}`, `[]`, `{"data":{"effective_rate_multiplier":null}}`,
		`{"effective_rate_multiplier":-1}`, `{"effective_rate_multiplier":"NaN"}`,
		`{"effective_rate_multiplier":"Inf"}`, `{"effective_rate_multiplier":1e999}`,
		`{"effective_rate_multiplier":1.5`, `{"effective_rate_multiplier":1.5} trailing`,
		`{"effective_rate_multiplier":1.5} {"effective_rate_multiplier":2}`,
	} {
		if rate, err := BillingRate([]byte(body)); err == nil || rate != nil {
			t.Fatalf("invalid billing became a rate: %s: %v / %v", body, rate, err)
		}
	}
	for _, tc := range []struct {
		body string
		want float64
	}{
		{`{"data":{"effective_rate_multiplier":0,"group_rate_multiplier":2}}`, 0},
		{`{"effective_rate_multiplier":"0"}`, 0},
		{`{"effective_rate_multiplier":" 1.5 ","group_rate_multiplier":2}`, 1.5},
		{`{"effective_rate_multiplier":1.5,"resolved_rate_multiplier":2,"group_rate_multiplier":3}`, 1.5},
		{`{"effective_rate_multiplier":null,"resolved_rate_multiplier":2}`, 2},
		{`{"group_rate_multiplier":3}`, 3},
		{`{"effective_rate_multiplier":1.5,"effective_rate_multiplier":3}`, 3},
		{`{"data":{"effective_rate_multiplier":1.5},"data":{"effective_rate_multiplier":3}}`, 3},
	} {
		rate, err := BillingRate([]byte(tc.body))
		if err != nil || rate == nil || *rate != tc.want {
			t.Fatalf("billing %s: rate=%v error=%v, want %g", tc.body, rate, err, tc.want)
		}
	}
}

func TestMonitorLegacySnapshotsStayHistorical(t *testing.T) {
	first := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	value := 2.0
	visible := true
	legacy := monitorDisk{Version: 1, Snapshot: Snapshot{
		Running: true, Polling: true,
		Accounts: []MonitorAccount{{
			SessionID: "session", LastSuccess: first, GroupsKnown: true, Complete: true,
			Groups:  []Group{{ID: "5", Name: "Group A", Rate: &value, Visible: &visible, LastSuccess: first}},
			Sources: []SourceSnapshot{{ID: "source", Rate: &value, Keys: []KeySnapshot{{ID: "key", Rate: &value, LastSuccess: first}}}},
		}},
	}}
	body, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "monitor.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewMonitor(dir, monitorTestPanel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := restarted.Snapshot()
	account := snapshot.Accounts[0]
	if snapshot.Running || snapshot.Polling || account.GroupsKnown || account.Complete || account.Groups[0].Rate != nil ||
		account.Groups[0].LastRate == nil || account.Sources[0].Rate != nil || account.Sources[0].BindingVersion != "" ||
		account.Sources[0].Keys[0].Rate != nil || account.Sources[0].Keys[0].LastRate == nil {
		t.Fatalf("unversioned disk values became current/applicable: %+v", snapshot)
	}
}
