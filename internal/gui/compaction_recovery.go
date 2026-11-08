package gui

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yetone/magpie/internal/gateway"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/recoverystore"
)

// Recovery copies and their opt-in stay on this computer, outside providers
// and synchronized settings. These routes share the page's private API guard.
func compactionRecoveryRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/compaction-recovery", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s, err := recoverystore.Stats()
		if err != nil {
			fail(w, err)
			return
		}
		choices, err := gateway.RecoveryChoicesChecked()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, struct {
			recoverystore.Status
			Choices []gateway.RecoveryChoice `json:"choices"`
		}{s, choices})
	})
	mux.HandleFunc("POST /api/compaction-recovery/{action}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		var in struct {
			Provider string `json:"provider"`
			Enabled  *bool  `json:"enabled"`
			MaxBytes *int64 `json:"maxBytes"`
			ID       string `json:"id"`
			Choice   string `json:"choice"`
			Confirm  bool   `json:"confirm"`
		}
		d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		d.DisallowUnknownFields()
		if err := d.Decode(&in); err != nil {
			fail(w, err)
			return
		}
		if err := d.Decode(new(any)); err != io.EOF {
			fail(w, errors.New("the request must contain one JSON object"))
			return
		}
		var err error
		switch r.PathValue("action") {
		case "configure":
			if in.Enabled != nil {
				if in.Provider == "" {
					fail(w, errors.New("give the provider ID"))
					return
				}
			}
			if in.Enabled == nil && in.MaxBytes == nil {
				fail(w, errors.New("give a recovery copy setting or storage limit"))
				return
			}
			if in.MaxBytes != nil && *in.MaxBytes <= 0 {
				fail(w, errors.New("the recovery storage limit must be a positive number of bytes"))
				return
			}
			_, err = provider.ConfigureRecovery(in.Provider, in.Enabled, in.MaxBytes)
		case "clear":
			if !in.Confirm {
				fail(w, errors.New("confirm clearing local recovery copies"))
				return
			}
			err = recoverystore.Clear()
		case "delete":
			if !in.Confirm || in.ID == "" {
				fail(w, errors.New("confirm deleting the selected local recovery copy"))
				return
			}
			err = recoverystore.DeleteID(in.ID)
		case "allow":
			err = gateway.AllowRecovery(in.ID, in.Choice)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
}
