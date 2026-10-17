package gui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"runtime"
	"time"

	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/sub2api"
	"github.com/yetone/magpie/internal/sub2apibrowser"
)

func sub2apiRoutes(mux *http.ServeMux, window Windows) {
	browser, browserReason := sub2apibrowser.Available()
	sub2apiRoutesWithBrowser(mux, window, browser, browserReason, sub2apibrowser.Login)
}

func sub2apiRoutesWithBrowser(mux *http.ServeMux, window Windows, browser bool, browserReason string,
	openBrowser func(context.Context, sub2api.BrowserRequest) (sub2api.BrowserCandidate, error)) {
	capability := struct {
		Writable      bool   `json:"writable"`
		Reason        string `json:"reason"`
		Browser       bool   `json:"browser"`
		BrowserReason string `json:"browserReason"`
	}{Writable: runtime.GOOS == "windows" && !isWeb(window), Browser: browser, BrowserReason: browserReason}
	if !capability.Writable {
		capability.Reason = "Sub2API changes require the Windows desktop app."
		capability.Browser = false
		capability.BrowserReason = capability.Reason
	}
	mux.HandleFunc("GET /api/sub2api", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		s, m, err := provider.Sub2APIServices()
		if err != nil {
			fail(w, err)
			return
		}
		accounts, err := s.Accounts()
		if err != nil {
			fail(w, err)
			return
		}
		if accounts == nil {
			accounts = []sub2api.AccountView{}
		}
		configs := make(map[string]sub2api.MonitorSettings)
		for _, a := range accounts {
			settings, err := m.Settings(a.ID)
			if err != nil {
				fail(w, err)
				return
			}
			configs[a.ID] = settings
		}
		writeJSON(w, map[string]any{"accounts": accounts, "monitor": m.Snapshot(), "configs": configs, "capability": capability})
	})
	mux.HandleFunc("POST /api/sub2api/{action}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !capability.Writable {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			writeJSON(w, map[string]string{"error": capability.Reason})
			return
		}
		action := r.PathValue("action")
		browserAction := action == "browser" || action == "browser-account"
		if browserAction && !capability.Browser {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSON(w, map[string]string{"error": capability.BrowserReason})
			return
		}
		var in struct {
			Provider           string          `json:"provider"`
			Base               string          `json:"base"`
			Email              string          `json:"email"`
			Password           string          `json:"password"`
			Remember           bool            `json:"remember"`
			Attempt            string          `json:"attempt"`
			Code               string          `json:"code"`
			Session            string          `json:"session"`
			GroupID            string          `json:"groupId"`
			GroupName          string          `json:"groupName"`
			KeyIDs             []string        `json:"keyIds"`
			KeyID              string          `json:"keyId"`
			ExpectedVersion    string          `json:"expectedVersion"`
			ExpectedRevision   string          `json:"expectedRevision"`
			ExpectedGeneration uint64          `json:"expectedGeneration"`
			Enabled            *bool           `json:"enabled"`
			Groups             []sub2api.Group `json:"groups"`
			Rate               float64         `json:"rate"`
			ExpectedRate       float64         `json:"expectedRate"`
			BindingVersion     string          `json:"bindingVersion"`
			SettingsRevision   string          `json:"settingsRevision"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		if err := decoder.Decode(&in); err != nil {
			fail(w, err)
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			fail(w, errors.New("the request must contain one JSON object"))
			return
		}
		s, m, err := provider.Sub2APIServices()
		if err != nil {
			fail(w, err)
			return
		}
		timeout := 45 * time.Second
		if browserAction {
			timeout = 5 * time.Minute
		}
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		var result any = map[string]bool{"ok": true}
		switch action {
		case "login-account", "2fa-account", "browser-account":
			if action == "browser-account" {
				result, err = s.BrowserLogin(ctx, sub2api.LoginRequest{SessionID: in.Session,
					ExpectedGeneration: in.ExpectedGeneration}, openBrowser)
			} else if action == "login-account" {
				result, err = s.Login(ctx, sub2api.LoginRequest{SessionID: in.Session,
					ExpectedGeneration: in.ExpectedGeneration, Email: in.Email,
					Password: in.Password, Remember: in.Remember})
			} else {
				if err = s.ValidateAccountAttempt(in.Attempt, in.Session, in.ExpectedGeneration); err == nil {
					result, err = s.Complete2FA(ctx, in.Attempt, in.Code)
				}
			}
			if login, ok := result.(sub2api.LoginResult); err == nil && ok && login.Account != nil {
				m.Wake(login.Account.ID)
			}
		case "login", "2fa", "browser":
			p, e := provider.Sub2APIProvider(in.Provider)
			if e != nil {
				err = e
				break
			}
			var login sub2api.LoginResult
			if action == "login" || action == "browser" {
				email, password, remember := in.Email, in.Password, in.Remember
				if browserAction {
					email, password, remember = "", "", false
				}
				request, e := provider.Sub2APILoginInput(in.Provider, in.Base, email, password, remember, in.ExpectedRevision)
				if e != nil {
					err = e
					break
				}
				if provider.Sub2APIRevision(p) != request.Revision {
					err = provider.ErrSub2APIConflict
					break
				}
				if browserAction {
					login, err = s.BrowserLogin(ctx, request, openBrowser, provider.Sub2APIBrowserValidator(p))
				} else {
					login, err = s.Login(ctx, request)
				}
			} else {
				if err = s.ValidateAttempt(in.Attempt, p.ID, provider.Sub2APIRevision(p)); err != nil {
					break
				}
				login, err = s.Complete2FA(ctx, in.Attempt, in.Code)
			}
			type bindingResult struct {
				Linked bool   `json:"linked"`
				Error  string `json:"error,omitempty"`
			}
			response := struct {
				sub2api.LoginResult
				Binding *bindingResult `json:"binding,omitempty"`
			}{LoginResult: login}
			if err == nil && login.Account != nil {
				binding := provider.Sub2APIBinding{Session: login.Account.ID}
				if p.Sub2API != nil {
					binding.Version = p.Sub2API.Version
				}
				if p.Sub2API != nil && p.Sub2API.Session == login.Account.ID {
					binding.GroupID, binding.GroupName = p.Sub2API.GroupID, p.Sub2API.GroupName
					binding.KeyIDs = append([]string(nil), p.Sub2API.KeyIDs...)
					binding.Keys = append([]provider.Sub2APIKeyBinding(nil), p.Sub2API.Keys...)
				}
				bindingErr := provider.BindSub2API(ctx, p.ID, binding, login.Revision, login.Account.Generation)
				response.Binding = &bindingResult{Linked: bindingErr == nil}
				if bindingErr == nil {
					provider.ForgetBalances()
				} else {
					response.Binding.Error = bindingErr.Error()
					m.Wake(login.Account.ID)
				}
			}
			result = response
		case "cancel":
			s.Cancel(in.Attempt)
		case "logout":
			result, err = s.LogoutExpected(ctx, in.Session, in.ExpectedGeneration)
			if err == nil {
				provider.ForgetBalances()
			}
		case "bind":
			err = provider.BindSub2API(ctx, in.Provider, provider.Sub2APIBinding{
				Session: in.Session, GroupID: in.GroupID, GroupName: in.GroupName,
				KeyIDs: in.KeyIDs, Version: in.ExpectedVersion,
			}, in.ExpectedRevision, in.ExpectedGeneration)
		case "bind-key":
			err = provider.BindSub2APIKey(ctx, in.Provider, in.Session, in.KeyID, in.GroupID, in.GroupName,
				in.ExpectedVersion, in.ExpectedRevision, in.ExpectedGeneration)
		case "unbind":
			err = provider.UnbindSub2API(in.Provider, in.ExpectedVersion, in.ExpectedRevision)
		case "refresh":
			err = m.Refresh(ctx)
		case "refresh-account":
			err = m.RefreshAccount(ctx, in.Session, in.ExpectedGeneration, in.SettingsRevision)
		case "preview":
			var targets []sub2api.Target
			targets, err = provider.Sub2APIPreviewTargets(in.Session, in.Provider, in.ExpectedRevision, in.BindingVersion, in.ExpectedGeneration)
			if err == nil {
				result, err = m.Preview(ctx, in.Session, in.ExpectedGeneration, in.SettingsRevision, targets)
			}
		case "monitor":
			if in.Enabled == nil {
				err = errors.New("the monitor switch must be explicit")
			} else {
				err = m.SetAccountExpected(in.Session, *in.Enabled, in.Groups, in.ExpectedGeneration, in.SettingsRevision)
			}
		case "apply-rate":
			err = provider.ApplySub2APIRate(ctx, in.Provider, in.Rate, in.ExpectedRate, in.BindingVersion, in.ExpectedRevision, in.ExpectedGeneration)
		case "apply-key-rate":
			err = provider.ApplySub2APIKeyRate(ctx, in.Provider, in.KeyID, in.Rate, in.ExpectedRate,
				in.BindingVersion, in.ExpectedRevision, in.ExpectedGeneration)
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			if errors.Is(err, provider.ErrSub2APIConflict) || errors.Is(err, provider.ErrConfigConflict) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				writeJSON(w, map[string]any{"error": err.Error()})
				return
			}
			var detail *sub2api.Error
			if errors.As(err, &detail) {
				w.Header().Set("Content-Type", "application/json")
				status := http.StatusBadRequest
				if detail.Kind == sub2api.KindConflict {
					status = http.StatusConflict
				}
				w.WriteHeader(status)
				writeJSON(w, map[string]any{"error": detail.Message, "detail": detail})
			} else {
				fail(w, err)
			}
			return
		}
		writeJSON(w, result)
	})
}
