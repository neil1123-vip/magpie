package sub2api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMonitorKedayaMatrixKeepsUnambiguousGroups(t *testing.T) {
	snapshots := 0
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		switch {
		case path == "/api/v1/groups/available":
			return []byte(`{"data":[{"id":5,"name":"Composite"},{"id":6,"name":"Single"}]}`), nil
		case strings.Contains(path, "/matrix?"):
			// The matrix key is (platform, group_id), not group_id alone.
			return []byte(`{"code":0,"data":{"group_by":"platform_group","items":[
				{"platform":"anthropic","group_id":5,"group_name":"Composite","metrics":{"cache_rate":0.2}},
				{"platform":"openai","group_id":5,"group_name":"Composite","metrics":{"cache_rate":0.7}},
				{"platform":"openai","group_id":6,"group_name":"Single","metrics":{"cache_rate":0.8,"error_rate":0.01,"ttft":{"p50_ms":120}}}
			]}}`), nil
		case strings.Contains(path, "/snapshot?"):
			snapshots++
			if !strings.Contains(path, "group_id=5") {
				t.Fatalf("a valid matrix group requested a fallback: %s", path)
			}
			// Snapshot has no echoed group identity, so it remains account-scoped.
			return []byte(`{"code":0,"data":{"config":{},"metrics":{"cache_rate":0.4},"trend":[]}}`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	m.SetTargets(func() []Target { return []Target{target} })
	account, err := m.Preview(t.Context(), target.SessionID, target.Generation, "", []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if snapshots != 1 || account.ObservationError != "" || len(account.Groups) != 2 ||
		account.Groups[0].Observation != nil || account.Observation == nil || account.Observation.Scope != "global" {
		t.Fatalf("cross-platform rows broke sibling groups or invented group metrics: %+v", account)
	}
	observation := account.Groups[1].Observation
	if observation == nil || observation.Scope != "group" || observation.CacheRate == nil || *observation.CacheRate != .8 ||
		observation.Availability == nil || *observation.Availability != .99 ||
		observation.TTFTP50MS == nil || *observation.TTFTP50MS != 120 {
		t.Fatalf("unambiguous matrix group lost its metrics: %+v", observation)
	}
}

func TestMonitorObservationsKeepDatedFieldsAndResetChangedIdentity(t *testing.T) {
	target := monitorTestTarget()
	phase, matrix, billing := 0, 0, 0
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	first := now
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if strings.Contains(path, "/matrix?") {
			matrix++
			switch phase {
			case 0:
				return []byte(`{"data":{"items":[{"group_id":5,"name":"Group A","availability_7d":98,"probe_latency_ms":35,"metrics":{"cache_rate":0.4,"ttft":{"trimmed_avg_ms":250}}}]}}`), nil
			case 1:
				return []byte(`{"data":{"items":[{"group_id":5,"name":"Group A","metrics":{"cache_rate":0.7}}]}}`), nil
			default:
				return nil, &Error{Status: 403, Challenge: true}
			}
		}
		if strings.Contains(path, "/snapshot?") || path == "/api/v1/channel-monitors" {
			t.Fatal("challenge must not fan out to compatibility requests")
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		billing++
		return []byte(fmt.Sprintf(`{"effective_rate_multiplier":%d}`, phase+1)), nil
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
	account := refresh()
	value := account.Groups[0].Observation
	if value == nil || value.Scope != "group" || value.Source != "v2-matrix" ||
		value.Availability == nil || *value.Availability != .98 ||
		value.CacheRate == nil || *value.CacheRate != .4 ||
		value.ProbeLatencyMS == nil || *value.ProbeLatencyMS != 35 ||
		value.TTFTP50MS == nil || *value.TTFTP50MS != 250 || value.TTFTStatistic != "trimmed_mean" {
		t.Fatalf("poll did not collect the panel metrics: %+v", value)
	}
	phase, now = 1, now.Add(time.Minute)
	account = refresh()
	group := account.Groups[0]
	if group.Observation == nil || group.Observation.Availability != nil || group.Observation.TTFTP50MS != nil ||
		group.Observation.CacheRate == nil || *group.Observation.CacheRate != .7 ||
		!group.ObservationHistory["availability"].ObservedAt.Equal(first) ||
		!group.ObservationHistory["cacheRate"].ObservedAt.Equal(now) ||
		group.ObservationHistory["ttftP50Ms"].TTFTStatistic != "trimmed_mean" {
		t.Fatalf("partial observation replaced field provenance: %+v", group)
	}
	phase, now = 2, now.Add(time.Minute)
	account = refresh()
	if account.ObservationError == "" || account.Groups[0].Observation != nil || !account.NeedsAction ||
		account.Sources[0].Rate == nil || *account.Sources[0].Rate != 3 ||
		!account.Groups[0].ObservationHistory["availability"].ObservedAt.Equal(first) {
		t.Fatalf("optional observation error stopped billing or erased history: %+v", account)
	}
	now = now.Add(time.Minute)
	refresh()
	if matrix != 3 || billing != 4 {
		t.Fatalf("challenge retry ignored its own cooldown or stopped billing: matrix=%d billing=%d", matrix, billing)
	}
	target.Context, now = "changed-panel-network", now.Add(20*time.Minute)
	account = refresh()
	if account.Groups[0].LastObservation != nil || len(account.Groups[0].ObservationHistory) != 0 ||
		account.LastObservation != nil || len(account.ObservationHistory) != 0 {
		t.Fatalf("new panel identity inherited old metric history: %+v", account)
	}
}

func TestMonitorPreviewKeepsGlobalFallbackSeparateFromLaterGroup(t *testing.T) {
	var snapshots int
	panel := func(ctx context.Context, session, path string) ([]byte, error) {
		switch {
		case path == "/api/v1/groups/available":
			return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":6,"name":"Group B"}]}`), nil
		case strings.Contains(path, "/matrix?"):
			return nil, &Error{Status: 404}
		case strings.Contains(path, "/snapshot?"):
			snapshots++
			if strings.Contains(path, "group_id=5") {
				return []byte(`{"metrics":{"success_rate":0.5}}`), nil
			}
			if strings.Contains(path, "group_id=6") {
				return []byte(`{"group_id":6,"metrics":{"cache_rate":0.9,"ttft":{"p50_ms":120,"trimmed_avg_ms":80}}}`), nil
			}
			t.Fatalf("unexpected snapshot filter: %s", path)
		}
		return monitorTestPanel(ctx, session, path)
	}
	m, err := NewMonitor(t.TempDir(), panel, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	target.Keys = append(target.Keys, Key{ID: "second", Value: "second-key", Context: "second-context"})
	m.SetTargets(func() []Target { return []Target{target} })
	account, err := m.Preview(t.Context(), target.SessionID, target.Generation, "", []Target{target})
	if err != nil {
		t.Fatal(err)
	}
	if snapshots != 2 || account.Observation == nil || account.Observation.Scope != "global" ||
		account.Observation.Availability == nil || *account.Observation.Availability != .5 ||
		account.Groups[0].Observation != nil || account.Groups[1].Observation == nil ||
		account.Groups[1].Observation.Scope != "group" ||
		*account.Groups[1].Observation.CacheRate != .9 ||
		*account.Groups[1].Observation.TTFTP50MS != 120 || account.Groups[1].Observation.TTFTStatistic != "p50" {
		t.Fatalf("global fallback masked a later group or multiplied by keys: %+v; snapshots=%d", account, snapshots)
	}
	if cfg, err := m.Settings(target.SessionID); err != nil || cfg.Enabled || cfg.Configured || len(m.baselines) != 0 {
		t.Fatalf("preview committed background monitoring: %+v %v", cfg, err)
	}
}

func TestMonitorV1PollSharesChannelListAndKeepsProbeLatency(t *testing.T) {
	var reads int
	m, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		switch path {
		case "/api/v1/groups/available":
			return []byte(`{"data":[{"id":5,"name":"Group A"},{"id":6,"name":"Group B"}]}`), nil
		case "/api/v1/settings/public":
			return []byte(`{"channel_monitor_mode":"v1"}`), nil
		case "/api/v1/channel-monitors":
			reads++
			return []byte(`{"data":[{"group_id":5,"name":"Group A","availability_7d":98,"probe_latency_ms":120},{"group_id":6,"name":"Group B","availability":0.75,"latency_ms":250}]}`), nil
		default:
			t.Fatalf("V1 unexpectedly queried %s", path)
			return nil, nil
		}
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	target := monitorTestTarget()
	monitorTestTargets(t, m, func() []Target { return []Target{target} })
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	account := m.Snapshot().Accounts[0]
	first, second := account.Groups[0].Observation, account.Groups[1].Observation
	if reads != 1 || first == nil || second == nil || first.Source != "v1" || !first.Approximate ||
		first.Availability == nil || *first.Availability != .98 ||
		first.ProbeLatencyMS == nil || *first.ProbeLatencyMS != 120 || first.TTFTP50MS != nil ||
		second.Availability == nil || *second.Availability != .75 {
		t.Fatalf("V1 poll did not preserve per-group approximate probe readings: %+v, reads=%d", account, reads)
	}
}

func TestMonitorV1MissingGroupKeepsKeySyncAndDatedMetrics(t *testing.T) {
	target := monitorTestTarget()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	first := now
	channels := `{"data":{"items":[{"group_id":5,"name":"Group A","availability_7d":98}]}}`
	reads, syncs := 0, 0
	m, err := NewMonitor(t.TempDir(), func(_ context.Context, _, path string) ([]byte, error) {
		switch path {
		case "/api/v1/groups/available":
			return []byte(`{"data":[{"id":5,"name":"Group A","rate_multiplier":0.14}]}`), nil
		case "/api/v1/settings/public":
			return []byte(`{"channel_monitor_mode":"v1"}`), nil
		case "/api/v1/keys?page=1&page_size=100":
			return []byte(`[{"key":"secret-api-key","group_id":5}]`), nil
		case "/api/v1/channel-monitors":
			reads++
			return []byte(channels), nil
		default:
			t.Fatalf("V1 unexpectedly queried %s", path)
			return nil, nil
		}
	}, monitorTestBilling)
	if err != nil {
		t.Fatal(err)
	}
	m.now = func() time.Time { return now }
	m.SetTargets(func() []Target { return []Target{target} })
	m.SetSynchronizer(func(_ context.Context, targets []Target, account *MonitorAccount) ([]Target, error) {
		syncs++
		key := &account.Sources[0].Keys[0]
		if !key.GroupVerified || key.GroupID != "5" || key.GroupName != "Group A" || key.Rate == nil {
			t.Fatalf("V1 observations prevented key identification or billing: %+v", key)
		}
		key.LocalRate = key.Rate
		return targets, nil
	})
	if err := m.SetAccount(target.SessionID, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if m.Snapshot().Accounts[0].Groups[0].Observation == nil {
		t.Fatal("fixture did not establish a dated observation")
	}
	for _, body := range []string{
		`{"data":{"items":[{"group_id":6,"name":"Unrelated channel","availability_7d":99}]}}`,
		`{"data":{"items":[]}}`,
	} {
		channels, now = body, now.Add(time.Minute)
		settings, err := m.Settings(target.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		preview, err := m.Preview(t.Context(), target.SessionID, target.Generation, settings.Revision, []Target{target})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		for _, account := range []MonitorAccount{preview, m.Snapshot().Accounts[0]} {
			group, key := account.Groups[0], account.Sources[0].Keys[0]
			if account.Error != "" || account.ObservationError != "" || account.NeedsAction ||
				group.Error != "" || group.Observation != nil || group.Rate == nil ||
				!account.LastSuccess.Equal(now) || !account.ObservationLastSuccess.Equal(first) ||
				!group.ObservationHistory["availability"].ObservedAt.Equal(first) ||
				key.LocalRate == nil || !key.GroupVerified {
				t.Fatalf("missing V1 channel became a failed query or erased dated data: %+v; group=%+v; key=%+v", account, group, key)
			}
		}
	}
	if reads != 5 || syncs != 5 {
		t.Fatalf("preview and background did not sample and synchronize independently: reads=%d syncs=%d", reads, syncs)
	}
}

func TestMonitorV1MissingAndAmbiguousGroups(t *testing.T) {
	for _, body := range []string{`[]`, `{"data":[]}`, `{"items":[]}`, `{"data":{"items":[]}}`,
		`{"data":[{"group_id":5,"name":"Group A"}]}`,
		`{"data":[{"group_id":6,"name":"Unrelated channel","latency_ms":100}]}`} {
		if value, err := parseV1([]byte(body), Group{ID: "5", Name: "Group A"}); err != nil || value != nil {
			t.Errorf("absent V1 group should have no current metrics: %s => %+v, %v", body, value, err)
		}
	}
	for _, body := range []string{
		`{"group_id":5,"name":"Group A","latency_ms":100}`,
		`{"channels":[{"group_id":5,"name":"Group A","latency_ms":100}]}`,
	} {
		if value, err := parseV1([]byte(body), Group{ID: "5", Name: "Group A"}); err != nil || value == nil || value.Match != "id" {
			t.Errorf("existing nested V1 records stopped matching: %s => %+v, %v", body, value, err)
		}
	}
	for _, body := range []string{`{`, `{}`, `{"data":null}`, `{"data":{"message":"unavailable"}}`, `[null]`, `[{}]`, `{"data":[1]}`,
		`{"data":[{"group_id":5,"name":"A","latency_ms":1},{"group_id":5,"name":"B","latency_ms":2}]}`,
		`[{"name":"Group A one","latency_ms":1},{"name":"Group A two","latency_ms":2}]`} {
		if _, err := parseV1([]byte(body), Group{ID: "5", Name: "Group A"}); err == nil {
			t.Errorf("invalid or ambiguous V1 response was accepted: %s", body)
		}
	}
}
