//go:build windows && !nogui

package gui

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"

	wintoast "git.sr.ht/~jackmordaunt/go-toast/v2/wintoast"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"
)

func xmlText(text string) string {
	var out bytes.Buffer
	_ = xml.EscapeText(&out, []byte(text))
	return out.String()
}

// Prepare before the control check; delivery uses COM without the subprocess fallback.
func prepareSub2APIToast(n *notifier, id, body string) (func(func() error), func() error, error) {
	if !n.allowed() {
		return nil, nil, fmt.Errorf("Sub2API notifications are unavailable")
	}
	payload, err := json.Marshal(struct {
		Action  string                            `json:"action"`
		Options notifications.NotificationOptions `json:"payload"`
	}{notifications.DefaultActionIdentifier, notifications.NotificationOptions{ID: id, Title: "Sub2API", Body: body}})
	if err != nil {
		return nil, nil, err
	}
	document := `<toast launch="` + xmlText(base64.StdEncoding.EncodeToString(payload)) +
		`"><visual><binding template="ToastGeneric"><text>Sub2API</text><text>` +
		xmlText(body) + `</text></binding></visual></toast>`
	queue := func(work func() error) { application.InvokeAsync(func() { _ = work() }) }
	return queue, func() error { return wintoast.Push(application.Get().Config().Name, document) }, nil
}
