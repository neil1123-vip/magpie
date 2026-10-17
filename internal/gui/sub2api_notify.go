package gui

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/yetone/magpie/internal/sub2api"
)

func sub2apiChangeText(lang string, c sub2api.Change) string {
	site := c.Site
	if u, err := url.Parse(site); err == nil {
		site = u.Host
	}
	format := "%s: %d group or billing changes"
	switch lang {
	case "zh":
		format = "%s：检测到 %d 项分组或倍率变化"
	case "zh-TW":
		format = "%s：偵測到 %d 項分組或倍率變更"
	case "ja":
		format = "%s：グループまたは倍率の変更 %d 件"
	case "de":
		format = "%s: %d Änderungen an Gruppen oder Abrechnungsfaktoren"
	}
	lines := []string{fmt.Sprintf(format, site, len(c.Items))}
	if !c.At.IsZero() {
		lines[0] += " (" + c.At.Local().Format("15:04") + ")"
	}
	for i, item := range c.Items {
		if i == 3 {
			break
		}
		name := item.GroupName
		if name == "" {
			name = item.GroupID
		}
		if name == "" {
			name = item.SourceID
		}
		line := strings.Join(strings.Fields(name+": "+item.Before+" -> "+item.After), " ")
		if text := []rune(line); len(text) > 120 {
			line = string(text[:117]) + "..."
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}
