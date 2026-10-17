package gui

import (
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APINotificationNamesTheChangedRate(t *testing.T) {
	event := sub2api.Change{Site: "https://panel.example/base", At: time.Now(),
		Items: []sub2api.ChangeItem{{Kind: "key_rate", GroupName: "Fast", Before: "1", After: "1.5"}}}
	for _, lang := range []string{"zh", "zh-TW", "ja", "de", "en"} {
		body := sub2apiChangeText(lang, event)
		if !strings.Contains(body, "panel.example") || !strings.Contains(body, "Fast: 1 -> 1.5") ||
			!strings.Contains(body, event.At.Local().Format("15:04")) {
			t.Fatalf("%s notification omitted the site, time or rate: %s", lang, body)
		}
	}
}
