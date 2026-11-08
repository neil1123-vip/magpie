package cli

import (
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

func TestCompactionRecoveryCLI(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	if err := provider.Save(provider.Provider{ID: "relay", Name: "Relay", Responses: "http://127.0.0.1:1/v1", Key: "k", NativeCompaction: true}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"enable", "relay"}, {"limit", "104857600"}, {"status"}, {"status", "--json"}, {"disable", "relay"}, {"clear", "--yes"}} {
		if _, err := stdoutOf(t, func() error { return compactionRecoveryCmd(args) }); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	cfg, err := recoverystore.LoadConfig()
	if err != nil || len(cfg.Providers) != 0 || cfg.MaxBytes != 104857600 {
		t.Fatalf("configuration: %+v %v", cfg, err)
	}
	on := true
	if err := recoverystore.Configure("removed-provider", &on, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := stdoutOf(t, func() error { return compactionRecoveryCmd([]string{"disable", "removed-provider"}) }); err != nil {
		t.Fatalf("disable a removed provider: %v", err)
	}
	cfg, err = recoverystore.LoadConfig()
	if err != nil || len(cfg.Providers) != 0 {
		t.Fatalf("removed provider kept its local opt-in: %+v %v", cfg, err)
	}
	for _, args := range [][]string{{"enable", "missing"}, {"limit", "0"}, {"limit", "1.5"}, {"clear"}, {"delete", "missing"}, {"delete", "missing", "--yes"}, {"allow", "missing", "excerpt"}, {"status", "--secret"}, {"enable"}} {
		if err := compactionRecoveryCmd(args); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	if text := helpFor([]string{"compaction-recovery", "--help"}); !strings.Contains(text, "clear --yes") || !strings.Contains(text, "X-Magpie-Recovery-Token") {
		t.Fatalf("missing safe operation instructions: %s", text)
	}
}
