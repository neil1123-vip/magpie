//go:build windows && !nogui && sub2api_webview2

package sub2apibrowser

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/sub2api"
)

func Available() (bool, string) { return true, "" }

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > maxMessage {
		return 0, fmt.Errorf("Browser sign-in result is too large")
	}
	return b.Buffer.Write(p)
}

// The inherited anonymous pipes have no discoverable endpoint or token file.
func Login(ctx context.Context, req sub2api.BrowserRequest) (candidate sub2api.BrowserCandidate, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	base, err := sub2api.NormalizeBase(req.Base)
	if err != nil {
		return candidate, err
	}
	exe, err := os.Executable()
	if err != nil {
		return candidate, fmt.Errorf("Cannot locate the browser host")
	}
	profile, cleanup, err := startPrivateProfile(exe)
	if err != nil {
		return candidate, fmt.Errorf("Cannot create a private browser profile")
	}
	defer func() {
		if cleanup() != nil {
			candidate = sub2api.BrowserCandidate{}
			err = fmt.Errorf("Browser profile cleanup failed")
		}
	}()
	reader, writer, err := os.Pipe()
	if err != nil {
		return candidate, fmt.Errorf("Cannot create the browser host pipe")
	}
	defer reader.Close()
	defer writer.Close()
	cmd := proc.CommandContext(ctx, exe, hostArg)
	cmd.Stdin = reader
	var output limitedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	// Ignore inherited WebView2 flags, debugger ports and profile overrides.
	for _, item := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(item), "WEBVIEW2_") {
			cmd.Env = append(cmd.Env, item)
		}
	}
	tree, err := startBrowserProcess(cmd)
	if err != nil {
		return candidate, fmt.Errorf("Cannot isolate the browser host")
	}
	defer tree.Close()
	_ = reader.Close()
	done := make(chan error, 1)
	go func() { done <- tree.Wait() }()
	if err := json.NewEncoder(writer).Encode(wireRequest{base, req.Proxy, req.UserAgent, profile}); err != nil {
		tree.Kill()
		<-done
		return candidate, fmt.Errorf("Cannot initialize the browser host")
	}
	select {
	case err = <-done:
	case <-ctx.Done():
		tree.Kill()
		<-done
		return candidate, ctx.Err()
	}
	if err != nil {
		return candidate, fmt.Errorf("Browser sign-in ended without a result")
	}
	c, err := decodeCandidate(output.Bytes(), base)
	if err != nil {
		return candidate, fmt.Errorf("Browser sign-in was canceled or failed")
	}
	return sub2api.BrowserCandidate{
		Origin: c.Origin, Access: c.Access, Refresh: c.Refresh,
		UserAgent: c.UserAgent, HostExited: true,
	}, nil
}

// MaybeRun is called before cli.Main, so this process starts no gateway or tray.
func MaybeRun() bool {
	if len(os.Args) != 2 {
		return false
	}
	switch os.Args[1] {
	case hostArg:
		runHost()
	case profileArg:
		if runProfileCleaner() != nil {
			os.Exit(1)
		}
	default:
		return false
	}
	return true
}

func runHost() {
	input := bufio.NewReaderSize(os.Stdin, maxMessage)
	line, err := input.ReadSlice('\n')
	if err != nil || len(line) > maxMessage {
		return
	}
	var req wireRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&req) != nil || req.Profile == "" || !filepath.IsAbs(req.Profile) {
		return
	}
	base, err := sub2api.NormalizeBase(req.Base)
	if err != nil {
		return
	}
	args := []string{}
	if req.Proxy != "" && req.Proxy != "direct" {
		proxy, err := url.Parse(req.Proxy)
		if err != nil || proxy.Host == "" || proxy.User != nil ||
			(proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5") {
			return
		}
		args = append(args, "--proxy-server="+proxy.String())
	} else {
		args = append(args, "--no-proxy-server")
	}
	if req.UserAgent != "" {
		if len(req.UserAgent) > 2048 || strings.ContainsAny(req.UserAgent, "\r\n\x00") {
			return
		}
	}
	var app *application.App
	var once sync.Once
	var frozen atomic.Bool
	var result wireCandidate
	finish := func(c wireCandidate) {
		once.Do(func() {
			frozen.Store(true)
			result = c
			app.Quit()
		})
	}
	app = application.New(application.Options{
		Name: "Sub2API sign-in", LogLevel: slog.LevelError,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		RawMessageOnly: true,
		Windows: application.WindowsOptions{
			WebviewUserDataPath: req.Profile, AdditionalBrowserArgs: args,
		},
		Assets: application.AssetOptions{Middleware: func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "Isolated sign-in host", http.StatusForbidden)
			})
		}},
		RawMessageHandler: func(_ application.Window, raw string, info *application.OriginInfo) {
			if frozen.Load() || info == nil || !sameOrigin(info.Origin, base) ||
				!sameOrigin(info.TopOrigin, base) || !strings.HasPrefix(raw, "sub2api-candidate:") {
				return
			}
			c, err := decodeCandidate([]byte(strings.TrimPrefix(raw, "sub2api-candidate:")), base)
			if err == nil {
				finish(c)
			}
		},
	})
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		if frozen.Load() {
			app.Quit()
		}
	})
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "Sub2API sign-in", URL: base + "/login",
		Width: 960, Height: 720, MinWidth: 480, MinHeight: 480,
		Windows: application.WindowsWindow{
			DisableMenu: true,
			NativeHooks: &application.WebView2NativeHooks{
				UserAgent:        req.UserAgent,
				CancelNavigation: func(raw string) bool { return frozen.Load() || !sameOrigin(raw, base) },
				BlockRequest: func(raw string, source, resource uint32) bool {
					return frozen.Load() || blockedRequest(raw, base, source, resource)
				},
				DocumentScript: captureScript(base),
				Ready: func(err error) {
					if err != nil {
						go finish(wireCandidate{})
					}
				},
			},
		},
	})
	timer := time.AfterFunc(5*time.Minute, func() { finish(wireCandidate{}) })
	defer timer.Stop()
	// Parent death or cancellation closes its pipe, including before window creation.
	go func() {
		_, _ = io.Copy(io.Discard, input)
		finish(wireCandidate{})
	}()
	if app.Run() != nil {
		return
	}
	once.Do(func() { frozen.Store(true) })
	if result.Access != "" {
		_ = json.NewEncoder(os.Stdout).Encode(result)
	}
}
