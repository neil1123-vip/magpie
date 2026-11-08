package provider

import (
	"fmt"

	"github.com/yetone/magpie/internal/recoverystore"
)

// ConfigureRecovery validates the current provider while its local opt-in is
// locked against an ID rename. Disabling remains possible for a removed ID.
func ConfigureRecovery(id string, enabled *bool, maxBytes *int64) (string, error) {
	if enabled == nil || !*enabled {
		return id, recoverystore.Configure(id, enabled, maxBytes)
	}
	p, err := Find(id)
	if err != nil {
		return id, err
	}
	return p.ID, configureRecovery(*p, enabled, maxBytes)
}

func configureRecovery(p Provider, enabled *bool, maxBytes *int64) error {
	return recoverystore.ConfigureChecked(p.ID, enabled, maxBytes, func() error {
		// Another process can rename while this request holds a cached catalog.
		Changed()
		now, err := Find(p.ID)
		if err != nil {
			return err
		}
		if now.ID != p.ID || now.SaveRevision != p.SaveRevision {
			return fmt.Errorf("%s changed; reopen it before enabling local recovery", p.ID)
		}
		if !now.NativeCompaction || !now.CarriesCompaction() {
			return fmt.Errorf("%s: local recovery copies require native compaction on a supported provider", p.ID)
		}
		return nil
	})
}
