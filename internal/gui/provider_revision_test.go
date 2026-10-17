package gui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/provider"
)

// Ordinary editor fixtures take the revision when the form opens.
func currentProviderForm(t *testing.T, body string) string {
	t.Helper()
	var form map[string]any
	if err := json.Unmarshal([]byte(body), &form); err != nil {
		t.Fatal(err)
	}
	if form["new"] == true || form["saveRevision"] != nil {
		return body
	}
	id, _ := form["from"].(string)
	if id == "" {
		id, _ = form["id"].(string)
	}
	if p, err := provider.Find(id); err == nil {
		form["saveRevision"] = p.SaveRevision
	}
	b, err := json.Marshal(form)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestProviderEditorRejectsOldRateAndMissingRevision(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	p := provider.Provider{ID: "relay", Name: "Relay", Key: "k", Chat: "http://127.0.0.1:1/v1", PriceRate: 1}
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	oldForm := currentProviderForm(t, `{"id":"relay","from":"relay","name":"Old","chat":"http://127.0.0.1:1/v1","priceRate":1}`)
	p.PriceRate = 2
	if err := provider.Save(p); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	providerRoutes(mux, nil)
	for _, form := range []string{oldForm, `{"id":"relay","name":"Missing revision","chat":"http://127.0.0.1:1/v1"}`} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/provider/save", strings.NewReader(form)))
		if rec.Code != http.StatusConflict {
			t.Fatalf("old form: %d %s", rec.Code, rec.Body)
		}
	}
	got, err := provider.Find("relay")
	if err != nil || got.PriceRate != 2 || got.Name != "Relay" {
		t.Fatalf("old form changed the provider: %+v, %v", got, err)
	}
}
