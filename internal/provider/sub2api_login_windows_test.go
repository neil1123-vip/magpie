package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APISourceReloginKeepsSavedBrowserAgent(t *testing.T) {
	for _, mode := range []string{"password", "browser", "explicit-agent", "explicit-proxy", "other-panel", "signed-out"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login", "/api/v1/auth/refresh":
					fmt.Fprint(w, `{"data":{"access_token":"panel-access","refresh_token":"panel-refresh"}}`)
				case "/api/v1/auth/me":
					fmt.Fprint(w, `{"data":{"id":42,"email":"user@example.test"}}`)
				case "/api/v1/user/profile":
					fmt.Fprint(w, `{"data":{"balance":12.5}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer panel.Close()
			p := Provider{ID: "relay", Chat: panel.URL + "/v1", Key: "sk-one",
				BalanceURL: panel.URL + "/api/v1/user/profile", BalancePath: "$data.balance"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			s, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			const savedAgent = "saved-browser-agent"
			first, err := s.Login(t.Context(), sub2api.LoginRequest{
				Base: panel.URL, Email: "user@example.test", Password: "password", Proxy: "direct",
				UserAgent: savedAgent, Target: p.ID, Revision: "first",
			})
			if err != nil {
				t.Fatal(err)
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := BindSub2API(t.Context(), p.ID, Sub2APIBinding{Session: first.Account.ID},
				Sub2APIRevision(p), first.Account.Generation); err != nil {
				t.Fatal(err)
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			base, wantAgent := panel.URL, savedAgent
			switch mode {
			case "explicit-agent":
				p.Headers = map[string]string{"user-agent": "explicit-agent"}
				if err := Save(p); err != nil {
					t.Fatal(err)
				}
				wantAgent = "explicit-agent"
			case "explicit-proxy":
				p.Proxy = "http://127.0.0.1:12345"
				if err := Save(p); err != nil {
					t.Fatal(err)
				}
			case "other-panel":
				base, wantAgent = "https://other.example.test", ""
			case "signed-out":
				if err := s.Logout(t.Context(), first.Account.ID); err != nil {
					t.Fatal(err)
				}
				wantAgent = ""
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			input, err := Sub2APILoginInput(p.ID, base, "user@example.test", "password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "password" {
				if _, err := s.Login(t.Context(), input); err != nil {
					t.Fatalf("source password re-login lost the saved browser context: %v", err)
				}
			} else if mode == "browser" {
				input.Email, input.Password = "", ""
				_, err := s.BrowserLogin(t.Context(), input, func(_ context.Context, req sub2api.BrowserRequest) (sub2api.BrowserCandidate, error) {
					agent := req.UserAgent
					if agent == "" {
						agent = "new-browser-default-agent"
					}
					return sub2api.BrowserCandidate{Origin: req.Base, Access: "panel-access",
						Refresh: "panel-refresh", UserAgent: agent, HostExited: true}, nil
				}, Sub2APIBrowserValidator(p))
				if err != nil {
					t.Fatalf("source browser re-login lost the saved browser context: %v", err)
				}
			}
			if input.UserAgent != wantAgent {
				t.Fatalf("source sign-in user agent = %q, want %q", input.UserAgent, wantAgent)
			}
			if mode == "explicit-agent" && s.CheckContext(first.Account.ID, "direct", input.UserAgent) == nil {
				t.Fatal("a different explicit source agent bypassed the shared-account boundary")
			}
			if mode == "explicit-proxy" && input.Proxy != p.Proxy {
				t.Fatal("source re-login replaced an explicit source proxy")
			}
		})
	}
}
