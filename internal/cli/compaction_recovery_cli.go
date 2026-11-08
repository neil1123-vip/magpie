package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

const compactionRecoveryUsage = `magpie compaction-recovery status [--json]
  Local recovery copies, disk usage, and requests awaiting a one-time choice.
magpie compaction-recovery enable <provider>
magpie compaction-recovery disable <provider>
  Save readable recovery copies on this computer, off by default. Enable
  native compaction on the provider first. Copies and this choice aren't synced.
magpie compaction-recovery limit <bytes>
  Set the total disk limit (512 MiB by default); no copies are silently discarded.
magpie compaction-recovery clear --yes
  Delete all local recovery copies. Conversations depending on them may no
  longer recover when their original account is unavailable.
magpie compaction-recovery delete <id> --yes
  Delete one copy listed by status; active dependencies are protected.
magpie compaction-recovery allow <id> native-only|excerpt
  Allow the listed choice for this request once, then retry in the client.
  native-only has no recovery copy; excerpt leaves some history out.

With no reliable branch identity, allow its id first, then include
X-Magpie-Recovery-Token: <id> on the exact retry request.
No additional summary model call is made; replaying history can use more tokens.
`

func compactionRecoveryCmd(args []string) error {
	if len(args) == 0 {
		args = []string{"status"}
	}
	switch args[0] {
	case "status":
		if len(args) > 2 || (len(args) == 2 && args[1] != "--json") {
			return fmt.Errorf("%s", compactionRecoveryUsage)
		}
		s, err := recoverystore.Stats()
		if err != nil {
			return err
		}
		choices, err := gateway.RecoveryChoicesChecked()
		if err != nil {
			return err
		}
		if len(args) == 2 {
			return json.NewEncoder(os.Stdout).Encode(struct {
				recoverystore.Status
				Choices []gateway.RecoveryChoice `json:"choices"`
			}{s, choices})
		}
		fmt.Printf("Local recovery copies: %d bytes used, %d bytes reserved, limit %d bytes\n", s.Bytes, s.ReservedBytes, s.Config.MaxBytes)
		fmt.Printf("Enabled providers: %s\n", strings.Join(s.Config.Providers, ", "))
		fmt.Printf("%d copies; %d compactions pending\n", len(s.Records), s.Pending)
		for _, r := range s.Records {
			fmt.Printf("%s  %s  %s  %s  %d bytes  lossy=%t\n", r.ID, r.Provider, r.Model, r.Created.Format("2006-01-02 15:04:05"), r.Bytes, r.Lossy)
		}
		for _, c := range choices {
			fmt.Printf("%s  %s  %s  %s  %s\n", c.ID, c.Model, c.Branch, c.Reason, strings.Join(c.Actions, "|"))
		}
		return nil
	case "enable", "disable":
		if len(args) != 2 {
			return fmt.Errorf("%s", compactionRecoveryUsage)
		}
		on := args[0] == "enable"
		id, err := provider.ConfigureRecovery(args[1], &on, nil)
		if err != nil {
			return err
		}
		fmt.Printf("%s: local recovery copies %s on this computer\n", id, args[0]+"d")
		return nil
	case "limit":
		if len(args) != 2 {
			return fmt.Errorf("%s", compactionRecoveryUsage)
		}
		n, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil || n <= 0 {
			return fmt.Errorf("the recovery storage limit must be a positive number of bytes")
		}
		return recoverystore.Configure("", nil, &n)
	case "clear":
		if len(args) != 2 || args[1] != "--yes" {
			return fmt.Errorf("clearing copies prevents their future recovery; use magpie compaction-recovery clear --yes")
		}
		return recoverystore.Clear()
	case "delete":
		if len(args) != 3 || args[2] != "--yes" {
			return fmt.Errorf("deleting a copy prevents its future recovery; use magpie compaction-recovery delete <id> --yes")
		}
		return recoverystore.DeleteID(args[1])
	case "allow":
		if len(args) != 3 {
			return fmt.Errorf("%s", compactionRecoveryUsage)
		}
		if err := gateway.AllowRecovery(args[1], args[2]); err != nil {
			return err
		}
		fmt.Println("Allowed once for this request. Retry in the client.")
		return nil
	default:
		return fmt.Errorf("%s", compactionRecoveryUsage)
	}
}
