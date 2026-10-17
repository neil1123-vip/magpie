package sub2api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestMonitorSynchronizationAdoptsTicketWithoutEnablingPreview(t *testing.T) {
	target := monitorTestTarget()
	target.GroupID, target.GroupName = "", ""
	target.Revision = "initial"
	var mu sync.Mutex
	rate := .14
	failSync := false
	monitor, err := NewMonitor(t.TempDir(), func(ctx context.Context, session, path string) ([]byte, error) {
		if strings.HasPrefix(path, "/api/v1/keys?") {
			return []byte(`[{"key":"secret-api-key","group_id":5}]`), nil
		}
		return monitorTestPanel(ctx, session, path)
	}, func(context.Context, Target, Key) ([]byte, error) {
		return fmt.Appendf(nil, `{"effective_rate_multiplier":%g}`, rate), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	currentTargets := func() []Target {
		mu.Lock()
		defer mu.Unlock()
		return copyMonitorTargets([]Target{target})
	}
	monitor.SetTargets(currentTargets)
	monitor.SetSynchronizer(func(_ context.Context, targets []Target, account *MonitorAccount) ([]Target, error) {
		if failSync {
			return nil, errors.New("local write failed")
		}
		mu.Lock()
		defer mu.Unlock()
		target = targets[0]
		target.Revision = fmt.Sprint(rate)
		key := &account.Sources[0].Keys[0]
		if !key.GroupVerified || key.GroupID != "5" || key.GroupName != "Group A" {
			t.Fatalf("synchronizer received an unverified group: %+v", key)
		}
		target.Keys[0].GroupID, target.Keys[0].GroupName = key.GroupID, key.GroupName
		account.Sources[0].Revision = target.Revision
		local := rate
		key.LocalRate = &local
		return copyMonitorTargets([]Target{target}), nil
	})
	preview, err := monitor.Preview(t.Context(), target.SessionID, 0, "", currentTargets())
	if err != nil || preview.Sources[0].Keys[0].LocalRate == nil {
		t.Fatalf("preview did not synchronize: %+v %v", preview, err)
	}
	cfg, err := monitor.Settings(target.SessionID)
	if err != nil || cfg.Enabled || cfg.Configured || len(monitor.Snapshot().Changes) != 0 || len(monitor.baselines) != 0 {
		t.Fatalf("preview enabled or committed monitoring: %+v %v", cfg, err)
	}
	if err := monitor.SetAccount(target.SessionID, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	var notice Change
	monitor.notify = func(change Change) { notice = change }
	rate = .2
	if err := monitor.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot := monitor.Snapshot()
	if len(snapshot.Changes) != 1 || len(notice.Items) != 1 || notice.Items[0].Kind != "key_rate" ||
		notice.Items[0].Before != "0.14" || notice.Items[0].After != "0.2" {
		t.Fatalf("synchronized ticket lost its baseline or notification: %+v", snapshot)
	}
	if notice.WithCurrent == nil || notice.WithCurrent(func() error { return nil }) != nil {
		t.Fatal("automatic revision change invalidated its own notification")
	}
	failSync, rate = true, .3
	if err := monitor.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	account := monitor.Snapshot().Accounts[0]
	key := account.Sources[0].Keys[0]
	if account.Error == "" || key.SyncError == "" || key.LocalRate != nil || key.Rate == nil || *key.Rate != .3 {
		t.Fatalf("failed local write was displayed as synchronized: %+v", account)
	}
	mu.Lock()
	defer mu.Unlock()
	if target.Revision != "0.2" {
		t.Fatal("failed synchronization replaced the current targets")
	}
}
