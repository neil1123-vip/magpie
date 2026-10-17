package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/filelock"
	"github.com/yetone/magpie/internal/sub2api"
)

func TestSub2APICancelBeforeProviderCommit(t *testing.T) {
	for _, apply := range []bool{false, true} {
		name, path := "bind", "/wallet"
		if apply {
			name, path = "apply", "/v1/sub2api/billing"
		}
		t.Run(name, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			arrived, release := make(chan struct{}), make(chan struct{})
			letResponse := sync.OnceFunc(func() { close(release) })
			var hold atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if hold.Load() && r.URL.Path == path {
					close(arrived)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					w.Write([]byte(`{"balance":12.5}`))
				case "/v1/sub2api/billing":
					w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer panel.Close()
			defer letResponse()
			p := Provider{ID: "panel", Chat: panel.URL + "/v1", Key: "sk-one",
				BalanceURL: panel.URL + "/wallet", BalancePath: "$balance", BalanceToken: "legacy-token"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			session, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			login, err := session.Login(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			binding := Sub2APIBinding{Session: login.Account.ID, GroupID: "group", KeyIDs: []string{keyID(p.Key)}}
			if apply {
				if err := BindSub2API(t.Context(), p.ID, binding, login.Revision, login.Account.Generation); err != nil {
					t.Fatal(err)
				}
				p, err = panelProvider(p.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			read := make(chan struct{})
			// Cancel after the complete HTTP response, before its provider commit can take control.
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{PutIdleConn: func(error) {
				cancel()
				close(read)
			}})
			hold.Store(true)
			done := make(chan error, 1)
			go func() {
				if apply {
					done <- ApplySub2APIRate(ctx, p.ID, 1.5, p.PriceRate, p.Sub2API.Version,
						Sub2APIRevision(p), login.Account.Generation)
				} else {
					done <- BindSub2API(ctx, p.ID, binding, login.Revision, login.Account.Generation)
				}
			}()
			<-arrived
			controlHeld, releaseControl := make(chan struct{}), make(chan struct{})
			controlDone := make(chan error, 1)
			go func() {
				controlDone <- sub2api.WithControl(filepath.Join(appdir.Config(), "sub2api-local"), func() error {
					close(controlHeld)
					<-releaseControl
					return nil
				})
			}()
			<-controlHeld
			letResponse()
			<-read
			close(releaseControl)
			controlErr, operationErr := <-controlDone, <-done
			if controlErr != nil {
				t.Fatal(controlErr)
			}
			if !errors.Is(operationErr, context.Canceled) {
				t.Errorf("the canceled %s returned %v", name, operationErr)
			}
			got, err := panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if Sub2APIRevision(got) != Sub2APIRevision(p) || got.PriceRate != p.PriceRate {
				t.Fatalf("the canceled %s changed the binding, balance token, or price", name)
			}
		})
	}
}

func TestSub2APIApplyRejectsTruncatedBilling(t *testing.T) {
	isolate(t)
	priceHome(t)
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/wallet":
			w.Write([]byte(`{"balance":12.5}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"effective_rate_multiplier":1.5`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	p := Provider{ID: "panel", Chat: panel.URL + "/v1", Key: "sk-one",
		BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	p, err = panelProvider(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "password", false, Sub2APIRevision(p))
	if err != nil {
		t.Fatal(err)
	}
	login, err := session.Login(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	binding := Sub2APIBinding{Session: login.Account.ID, GroupID: "group", KeyIDs: []string{keyID(p.Key)}}
	if err := BindSub2API(t.Context(), p.ID, binding, login.Revision, login.Account.Generation); err != nil {
		t.Fatal(err)
	}
	p, err = panelProvider(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplySub2APIRate(t.Context(), p.ID, 1.5, p.PriceRate, p.Sub2API.Version,
		Sub2APIRevision(p), login.Account.Generation); err == nil {
		t.Fatal("truncated billing JSON authorized a price change")
	}
	got, err := panelProvider(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PriceRate != p.PriceRate {
		t.Fatalf("truncated billing JSON changed the source price: %g -> %g", p.PriceRate, got.PriceRate)
	}
}

func TestSub2APIApplyRechecksTheObservedBillingRate(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		before     float64
		after      float64
	}{
		{"duplicate rate", `{"effective_rate_multiplier":1.5,"effective_rate_multiplier":3}`, 1.5, 3},
		{"duplicate envelope", `{"data":{"effective_rate_multiplier":1.5},"data":{"effective_rate_multiplier":3}}`, 1.5, 3},
		{"string effective rate", `{"effective_rate_multiplier":"1.5","group_rate_multiplier":2}`, 2, 1.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			var changed atomic.Bool
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					w.Write([]byte(`{"balance":12.5}`))
				case "/api/v1/groups/available":
					w.Write([]byte(`[{"id":"group","name":"Group"}]`))
				case "/v1/sub2api/billing":
					if changed.Load() {
						w.Write([]byte(tc.body))
					} else {
						json.NewEncoder(w).Encode(map[string]float64{"effective_rate_multiplier": tc.before})
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer panel.Close()
			p := Provider{ID: "panel", Chat: panel.URL + "/v1", Key: "sk-one",
				BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			session, monitor, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			login, err := session.Login(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			binding := Sub2APIBinding{Session: login.Account.ID, GroupID: "group", KeyIDs: []string{keyID(p.Key)}}
			if err := BindSub2API(t.Context(), p.ID, binding, login.Revision, login.Account.Generation); err != nil {
				t.Fatal(err)
			}
			p, err = panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			settings, err := monitor.Settings(login.Account.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := monitor.SetAccountExpected(login.Account.ID, true, nil, login.Account.Generation, settings.Revision); err != nil {
				t.Fatal(err)
			}
			refresh := func(want float64) {
				t.Helper()
				if err := monitor.Refresh(t.Context()); err != nil {
					t.Fatal(err)
				}
				rate := monitor.Snapshot().Accounts[0].Sources[0].Rate
				if rate == nil || *rate != want {
					t.Fatalf("monitor rate = %v, want %g", rate, want)
				}
			}
			refresh(tc.before)
			changed.Store(true)
			refresh(tc.after)
			apply := func(rate float64) error {
				return ApplySub2APIRate(t.Context(), p.ID, rate, p.PriceRate, p.Sub2API.Version,
					Sub2APIRevision(p), login.Account.Generation)
			}
			if err := apply(tc.before); err == nil {
				t.Errorf("the previous rate %g still authorized a price write after the monitor observed %g", tc.before, tc.after)
			}
			got, err := panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.PriceRate != p.PriceRate {
				t.Fatalf("a stale rate changed PriceRate: %g -> %g", p.PriceRate, got.PriceRate)
			}
			if err := apply(tc.after); err != nil {
				t.Fatal(err)
			}
			got, err = panelProvider(p.ID)
			if err != nil || got.PriceRate != tc.after {
				t.Fatalf("the observed rate was not applied: price=%g err=%v", got.PriceRate, err)
			}
		})
	}
}

func TestSub2APIBillingChecksFullTarget(t *testing.T) {
	isolate(t)
	priceHome(t)
	var billingCalls, inferenceCalls atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/wallet":
			w.Write([]byte(`{"balance":12.5}`))
		case "/v1/sub2api/billing":
			billingCalls.Add(1)
			if r.Header.Get("User-Agent") != "Panel test UA" || r.Header.Get("Authorization") != "Bearer sk-one" {
				t.Errorf("billing sent another identity: %+v", r.Header)
			}
			w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
		default:
			t.Errorf("unexpected panel path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer panel.Close()
	inference := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inferenceCalls.Add(1)
		w.Write([]byte(`{"effective_rate_multiplier":999}`))
	}))
	defer inference.Close()
	p := Provider{ID: "relay", Chat: panel.URL + "/v1", Key: "sk-one",
		BalanceURL: panel.URL + "/wallet", BalancePath: "$balance",
		Headers: map[string]string{"User-Agent": "Panel test UA"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "password", false, Sub2APIRevision(p))
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.Login(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	binding := Sub2APIBinding{Session: result.Account.ID, GroupID: "group", KeyIDs: []string{keyID("sk-one")}}
	if err := BindSub2API(context.Background(), p.ID, binding, result.Revision, result.Account.Generation); err != nil {
		t.Fatal(err)
	}
	p, _ = panelProvider(p.ID)
	target := sub2apiTarget(p, *result.Account)
	key := target.Keys[0]
	if _, err := readSub2APIBilling(context.Background(), target, key); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*sub2api.Target, *sub2api.Key){
		func(v *sub2api.Target, _ *sub2api.Key) { v.Site = inference.URL },
		func(v *sub2api.Target, _ *sub2api.Key) { v.BindingVersion = "old-binding" },
		func(v *sub2api.Target, _ *sub2api.Key) { v.Revision = "old-source" },
		func(v *sub2api.Target, _ *sub2api.Key) { v.GroupID = "other-group" },
		func(v *sub2api.Target, _ *sub2api.Key) { v.Generation++ },
		func(_ *sub2api.Target, v *sub2api.Key) { v.Context = "" },
		func(_ *sub2api.Target, v *sub2api.Key) { v.Context = "old-context" },
		func(_ *sub2api.Target, v *sub2api.Key) { v.ID = "another-key" },
	} {
		oldTarget, oldKey := target, key
		change(&oldTarget, &oldKey)
		if _, err := readSub2APIBilling(context.Background(), oldTarget, oldKey); err == nil {
			t.Fatal("billing accepted a stale target")
		}
	}
	if billingCalls.Load() != 1 || inferenceCalls.Load() != 0 {
		t.Fatal("a stale target sent a request or billing followed the inference URL")
	}
	p.Sub2API.GroupID = ""
	onlyBalance := sub2apiTarget(p, *result.Account)
	if len(onlyBalance.Keys) != 0 || len(onlyBalance.ActiveKeyIDs) != 1 {
		t.Fatal("a balance-only binding treated its keys as a confirmed group")
	}
}

func TestSub2APIMonitorInvalidSourceDoesNotBlockOtherAccount(t *testing.T) {
	isolate(t)
	priceHome(t)
	var healthyGroups, healthyBilling, invalidBilling atomic.Int32
	session, monitor, err := Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	accounts := make(map[string]string)
	for _, id := range []string{"invalid", "healthy"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch r.URL.Path {
			case "/api/v1/auth/login":
				w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
			case "/api/v1/auth/me":
				w.Write([]byte(`{"data":{"id":42}}`))
			case "/wallet":
				w.Write([]byte(`{"balance":12.5}`))
			case "/api/v1/groups/available":
				if id == "healthy" {
					healthyGroups.Add(1)
				}
				w.Write([]byte(`{"data":[{"id":"group","name":"Group"}]}`))
			case "/v1/sub2api/billing":
				if id == "healthy" {
					healthyBilling.Add(1)
				} else {
					invalidBilling.Add(1)
				}
				w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		p := Provider{ID: id, Chat: srv.URL + "/v1", Key: "sk-" + id,
			BalanceURL: srv.URL + "/wallet", BalancePath: "$balance"}
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
		p, _ = panelProvider(id)
		input, err := Sub2APILoginInput(id, srv.URL, "user@example.test", "password", false, Sub2APIRevision(p))
		if err != nil {
			t.Fatal(err)
		}
		login, err := session.Login(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		accounts[id] = login.Account.ID
		binding := Sub2APIBinding{Session: login.Account.ID, GroupID: "group", KeyIDs: []string{keyID(p.Key)}}
		if err := BindSub2API(t.Context(), id, binding, login.Revision, login.Account.Generation); err != nil {
			t.Fatal(err)
		}
		if err := monitor.SetAccountExpected(login.Account.ID, true, nil, login.Account.Generation, ""); err != nil {
			t.Fatal(err)
		}
	}
	p, err := panelProvider("invalid")
	if err != nil {
		t.Fatal(err)
	}
	p.Headers = map[string]string{"Authorization": "Bearer another-identity"}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	if err := monitor.Refresh(t.Context()); err != nil {
		t.Fatalf("one invalid source stopped the monitoring round: %v", err)
	}
	if healthyGroups.Load() != 1 || healthyBilling.Load() != 1 || invalidBilling.Load() != 0 {
		t.Fatalf("requests escaped their source: healthy groups/billing = %d/%d, invalid billing = %d",
			healthyGroups.Load(), healthyBilling.Load(), invalidBilling.Load())
	}
	status := make(map[string]sub2api.MonitorAccount)
	for _, account := range monitor.Snapshot().Accounts {
		status[account.SessionID] = account
	}
	if status[accounts["healthy"]].Error != "" || status[accounts["healthy"]].LastSuccess.IsZero() ||
		status[accounts["invalid"]].BillingError == "" {
		t.Fatalf("source errors were hidden or crossed accounts: %+v", status)
	}
}

func TestSub2APICommitRejectsReloginDuringRead(t *testing.T) {
	for _, action := range []string{"bind", "apply-rate"} {
		t.Run(action, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			started, release := make(chan struct{}), make(chan struct{})
			var hold atomic.Bool
			var releaseOnce sync.Once
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if hold.Load() && (action == "bind" && r.URL.Path == "/wallet" ||
					action == "apply-rate" && r.URL.Path == "/v1/sub2api/billing") {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"data":{"access_token":"panel-access","refresh_token":"panel-refresh"}}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					w.Write([]byte(`{"balance":12.5}`))
				case "/v1/sub2api/billing":
					w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			defer releaseOnce.Do(func() { close(release) })
			p := Provider{ID: "relay", Chat: srv.URL + "/v1", Key: "sk-one",
				BalanceURL: srv.URL + "/wallet", BalancePath: "$balance", BalanceToken: "old-static"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			s, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			login := func() sub2api.LoginResult {
				t.Helper()
				current, err := panelProvider(p.ID)
				if err != nil {
					t.Fatal(err)
				}
				input, err := Sub2APILoginInput(p.ID, srv.URL, "user@example.test", "password", false, Sub2APIRevision(current))
				if err != nil {
					t.Fatal(err)
				}
				result, err := s.Login(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				return result
			}
			first := login()
			binding := Sub2APIBinding{Session: first.Account.ID, GroupID: "group",
				KeyIDs: []string{keyID("sk-one")}}
			if action == "apply-rate" {
				if err := BindSub2API(ctx, p.ID, binding, first.Revision, first.Account.Generation); err != nil {
					t.Fatal(err)
				}
			}
			current, err := panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			done, finished := make(chan error, 1), make(chan struct{})
			hold.Store(true)
			go func() {
				defer close(finished)
				if action == "bind" {
					done <- BindSub2API(ctx, p.ID, binding, Sub2APIRevision(current), first.Account.Generation)
				} else {
					done <- ApplySub2APIRate(ctx, p.ID, 1.5, current.PriceRate,
						current.Sub2API.Version, Sub2APIRevision(current), first.Account.Generation)
				}
			}()
			defer func() {
				releaseOnce.Do(func() { close(release) })
				select {
				case <-finished:
				case <-ctx.Done():
					t.Error("operation outlived its test")
				}
			}()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("operation ended before its read: %v", err)
			case <-ctx.Done():
				t.Fatal("operation did not reach the server")
			}
			if _, err := s.LogoutExpected(ctx, first.Account.ID, first.Account.Generation); err != nil {
				t.Fatal(err)
			}
			second := login()
			if second.Account.Generation == first.Account.Generation {
				t.Fatal("relogin did not invalidate the old operation")
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case err := <-done:
				var detail *sub2api.Error
				if !errors.As(err, &detail) || detail.Kind != sub2api.KindConflict {
					t.Fatalf("old %s committed after logout and relogin: %v", action, err)
				}
			case <-ctx.Done():
				t.Fatal("operation did not finish")
			}
			got, err := panelProvider(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if action == "bind" && (got.Sub2API != nil || got.BalanceToken != "old-static") {
				t.Fatal("stale binding changed the original balance")
			}
			if got.PriceRate != current.PriceRate {
				t.Fatal("stale rate confirmation changed the price")
			}
		})
	}
}

func TestSub2APIBalanceRejectsLoggedOutRead(t *testing.T) {
	for _, mode := range []string{"local-in-flight", "other-manager-in-flight", "other-manager-cached",
		"forgotten-in-flight", "newer-cached-in-flight", "logout-during-acceptance"} {
		t.Run(mode, func(t *testing.T) {
			isolate(t)
			priceHome(t)
			started, release := make(chan struct{}), make(chan struct{})
			letResponse := sync.OnceFunc(func() { close(release) })
			var hold atomic.Bool
			var wallets atomic.Int32
			panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/auth/login":
					w.Write([]byte(`{"access_token":"test-access","refresh_token":"test-refresh"}`))
				case "/api/v1/auth/me":
					w.Write([]byte(`{"data":{"id":42}}`))
				case "/wallet":
					wallets.Add(1)
					if hold.Load() {
						close(started)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
						w.Write([]byte(`{"balance":99}`))
					} else {
						w.Write([]byte(`{"balance":12.5}`))
					}
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(panel.Close)
			t.Cleanup(letResponse)
			p := Provider{ID: "balance-panel", Name: "Balance panel", Chat: panel.URL + "/v1", Key: "sk-test",
				BalanceURL: panel.URL + "/wallet", BalancePath: "$balance"}
			if err := Save(p); err != nil {
				t.Fatal(err)
			}
			session, _, err := Sub2APIServices()
			if err != nil {
				t.Fatal(err)
			}
			p, _ = panelProvider(p.ID)
			input, err := Sub2APILoginInput(p.ID, panel.URL, "user@example.test", "test-password", false, Sub2APIRevision(p))
			if err != nil {
				t.Fatal(err)
			}
			login, err := session.Login(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			if err := BindSub2API(t.Context(), p.ID, Sub2APIBinding{Session: login.Account.ID}, login.Revision, login.Account.Generation); err != nil {
				t.Fatal(err)
			}
			ForgetBalances()
			seed := KeyBalances(t.Context())
			if len(seed) != 1 || seed[0].Balance != "$12.50" || seed[0].Error != "" {
				t.Fatalf("initial balance: %+v", seed)
			}
			history := readBalanceHist()
			for key, points := range history {
				for i := range points {
					points[i].At = points[i].At.Add(-time.Minute)
				}
				history[key] = points
			}
			writeBalanceHist(history)
			read := func() []SubscriptionQuota { return KeyBalances(t.Context()) }
			if mode != "other-manager-cached" {
				ForgetBalances()
				hold.Store(true)
				done, finished := make(chan []SubscriptionQuota, 1), make(chan struct{})
				readCtx, cancel := context.WithCancel(t.Context())
				go func() {
					defer close(finished)
					done <- KeyBalances(readCtx)
				}()
				t.Cleanup(func() {
					letResponse()
					cancel()
					<-finished
				})
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("balance request did not reach the panel")
				}
				read = func() []SubscriptionQuota {
					letResponse()
					select {
					case got := <-done:
						return got
					case <-time.After(5 * time.Second):
						t.Fatal("balance read did not finish")
						return nil
					}
				}
			}
			if mode == "newer-cached-in-flight" {
				hold.Store(false)
				earlier := quotaReadSequence.Load()
				if got := KeyBalances(t.Context()); len(got) != 1 || got[0].Balance != "$12.50" || got[0].Error != "" {
					t.Fatalf("newer balance read: %+v", got)
				}
				keyBalanceCache.Lock()
				newer := slices.Clone(keyBalanceCache.data)
				keyBalanceCache.Unlock()
				if len(newer) != 1 || newer[0].Error != "" || newer[0].readSeq <= earlier {
					t.Fatalf("newer successful reading was not cached: %+v", newer)
				}
			}
			if mode == "logout-during-acceptance" {
				balanceHistMu.Lock()
				letHistory := sync.OnceFunc(balanceHistMu.Unlock)
				t.Cleanup(letHistory)
				letResponse()
				deadline := time.Now().Add(5 * time.Second)
				for {
					lastQuotas.Lock()
					accepted := false
					for _, last := range lastQuotas.m {
						accepted = accepted || last.Q.Provider == p.ID && last.Q.Balance == "$99.00"
					}
					lastQuotas.Unlock()
					if accepted {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("balance did not reach local acceptance")
					}
					time.Sleep(time.Millisecond)
				}
				control, err := os.OpenFile(filepath.Join(appdir.Config(), "sub2api-local", "locks", "control.lock"), os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				held, err := filelock.TryLock(control)
				if held {
					filelock.Unlock(control)
				}
				control.Close()
				if err != nil || held {
					t.Errorf("history and cache acceptance did not retain the cross-process logout lock: held=%v err=%v", held, err)
				}
				loggedOut := make(chan error, 1)
				logoutDone := make(chan struct{})
				go func() {
					defer close(logoutDone)
					owner := sub2api.New(filepath.Join(appdir.Config(), "sub2api-local"))
					_, err := owner.LogoutExpected(t.Context(), login.Account.ID, login.Account.Generation)
					loggedOut <- err
				}()
				t.Cleanup(func() { letHistory(); <-logoutDone })
				letHistory()
				if got := read(); len(got) != 1 || got[0].Balance != "$99.00" || got[0].Error != "" {
					t.Errorf("read accepted before logout did not finish: %+v", got)
				}
				if err := <-loggedOut; err != nil {
					t.Fatal(err)
				}
				if got := KeyBalances(t.Context()); len(got) != 1 || got[0].Balance != "$99.00" || got[0].Error == "" || got[0].AsOf == nil {
					t.Errorf("subsequent logout reused the accepted balance as current: %+v", got)
				}
				return
			}
			if mode != "forgotten-in-flight" {
				owner := session
				if mode != "local-in-flight" {
					owner = sub2api.New(filepath.Join(appdir.Config(), "sub2api-local"))
				}
				if _, err := owner.LogoutExpected(t.Context(), login.Account.ID, login.Account.Generation); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "local-in-flight" || mode == "forgotten-in-flight" {
				ForgetBalances()
			}
			assertStale := func(got []SubscriptionQuota) {
				t.Helper()
				if len(got) != 1 || got[0].Balance != "$12.50" || got[0].Error == "" || got[0].AsOf == nil {
					t.Errorf("signed-out account accepted a current balance: %+v", got)
				}
			}
			assertStale(read())
			if mode == "forgotten-in-flight" {
				hold.Store(false)
				before := wallets.Load()
				if got := KeyBalances(t.Context()); len(got) != 1 || got[0].Error != "" || got[0].Balance != "$12.50" || wallets.Load() != before+1 {
					t.Errorf("forgotten read was cached instead of asking again: %+v", got)
				}
			} else {
				assertStale(KeyBalances(t.Context()))
			}
			for _, points := range readBalanceHist() {
				for _, point := range points {
					if point.Amount != 12.5 {
						t.Errorf("old response reached balance history: %+v", point)
					}
				}
			}
		})
	}
}

func TestSub2APIBindingRejectsAnotherSiteBeforeBalanceDefaults(t *testing.T) {
	isolate(t)
	priceHome(t)
	var balanceCalls atomic.Int32
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"access_token":"panel-access","refresh_token":"panel-refresh"}`))
		case "/api/v1/auth/me":
			w.Write([]byte(`{"data":{"id":42}}`))
		default:
			balanceCalls.Add(1)
			w.Write([]byte(`{"data":{"balance":12.5}}`))
		}
	}))
	defer panel.Close()
	session, _, err := Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	login, err := session.Login(t.Context(), sub2api.LoginRequest{
		Base: panel.URL, Email: "user@example.test", Password: "test-password", Target: "other", Revision: "fixture",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, balance := range []string{"", "https://other.test/v1/usage"} {
		p := Provider{ID: "other", Chat: "https://other.test/v1", Key: "sk-other",
			BalanceURL: balance, BalanceToken: "keep-static"}
		if err := Save(p); err != nil {
			t.Fatal(err)
		}
		p, err = panelProvider(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		err = BindSub2API(t.Context(), p.ID, Sub2APIBinding{Session: login.Account.ID},
			Sub2APIRevision(p), login.Account.Generation)
		if err == nil || !strings.Contains(err.Error(), "same origin") || balanceCalls.Load() != 0 {
			t.Fatalf("another site's source reached the panel: balance=%q calls=%d err=%v", balance, balanceCalls.Load(), err)
		}
		stored, err := panelProvider(p.ID)
		if err != nil || stored.Sub2API != nil || stored.BalanceURL != balance || stored.BalanceToken != "keep-static" {
			t.Fatalf("rejected binding changed the source: %+v, %v", stored, err)
		}
	}
}

func TestSub2APIExistingBalanceLinksWithoutExposingTokens(t *testing.T) {
	isolate(t)
	priceHome(t)
	var balanceCalls atomic.Int32
	var method atomic.Value
	method.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/auth/login":
			w.Write([]byte(`{"data":{"access_token":"panel-access","refresh_token":"panel-refresh","expires_in":3600}}`))
		case "/api/v1/auth/me":
			if r.Header.Get("Authorization") != "Bearer panel-access" {
				t.Error("identity request did not use the panel token")
			}
			w.Write([]byte(`{"data":{"id":42}}`))
		case "/custom-balance":
			balanceCalls.Add(1)
			method.Store(r.Method)
			if r.Header.Get("Authorization") != "Bearer panel-access" || r.Header.Get("X-Org") != "acme" {
				t.Errorf("account headers: %+v", r.Header)
			}
			w.Write([]byte(`{"wallet":12.5}`))
		case "/v1/sub2api/billing":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer sk-") {
				t.Error("panel JWT was sent to the API key billing request")
			}
			w.Write([]byte(`{"effective_rate_multiplier":1.5}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := Provider{ID: "relay", Name: "Relay", Chat: srv.URL + "/v1", Key: "sk-one",
		Keys: []KeyAccount{{Key: "sk-two"}}, BalanceURL: srv.URL + "/custom-balance",
		BalancePath: "$wallet", BalanceMethod: "POST", BalanceToken: "old-static",
		Headers: map[string]string{"X-Org": "acme"}}
	if err := Save(p); err != nil {
		t.Fatal(err)
	}
	s, _, err := Sub2APIServices()
	if err != nil {
		t.Fatal(err)
	}
	current, _ := panelProvider(p.ID)
	input, err := Sub2APILoginInput(p.ID, srv.URL, "user@example.test", "test-password", false, Sub2APIRevision(current))
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.Login(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	binding := Sub2APIBinding{Session: result.Account.ID, KeyIDs: []string{keyID("sk-one"), keyID("sk-two")}}
	if err := BindSub2API(context.Background(), p.ID, binding, result.Revision, result.Account.Generation); err != nil {
		t.Fatal(err)
	}
	stored, _ := panelProvider(p.ID)
	if stored.BalanceToken != "" || stored.BalanceMethod != "POST" || stored.BalancePath != "$wallet" {
		t.Fatalf("existing balance configuration changed: %+v", stored)
	}
	ForgetBalances()
	before := balanceCalls.Load()
	cards := KeyBalances(context.Background())
	if len(cards) != 1 || cards[0].Balance != "$12.50" || cards[0].Error != "" || balanceCalls.Load() != before+1 {
		t.Fatalf("shared account balance should be read once: %+v", cards)
	}
	if method.Load() != "POST" {
		t.Fatal("the custom balance method was lost")
	}
	copy := stored
	copy.ID, copy.Name = "shared", "Shared account"
	copyID, err := AddCopy(copy, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	copy.ID = copyID
	ForgetBalances()
	before = balanceCalls.Load()
	if cards := KeyBalances(context.Background()); len(cards) != 1 || balanceCalls.Load() != before+1 {
		t.Fatalf("identical shared balances were read more than once: %+v", cards)
	}
	copy.BalancePath = "$wallet / 2"
	if err := Save(copy); err != nil {
		t.Fatal(err)
	}
	ForgetBalances()
	before = balanceCalls.Load()
	cards = KeyBalances(context.Background())
	if len(cards) != 2 || balanceCalls.Load() != before+2 {
		t.Fatalf("a distinct custom balance was hidden by session deduplication: %+v", cards)
	}
	amounts := map[string]bool{}
	for _, card := range cards {
		amounts[card.Balance] = true
	}
	if !amounts["$12.50"] || !amounts["$6.25"] {
		t.Fatalf("distinct custom balance parsing was lost: %+v", cards)
	}
	bytes, err := os.ReadFile(Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"panel-access", "panel-refresh", "test-password", "old-static"} {
		if strings.Contains(string(bytes), secret) {
			t.Fatalf("portable provider contains %s", secret)
		}
	}
	groupBinding := *stored.Sub2API
	groupBinding.GroupID, groupBinding.GroupName = "group", "Group"
	if err := BindSub2API(context.Background(), p.ID, groupBinding, Sub2APIRevision(stored), result.Account.Generation); err != nil {
		t.Fatal(err)
	}
	stored, _ = panelProvider(p.ID)
	if err := ApplySub2APIRate(context.Background(), p.ID, 1.5, 0, stored.Sub2API.Version, Sub2APIRevision(stored), result.Account.Generation); err != nil {
		t.Fatal(err)
	}
	stored, _ = panelProvider(p.ID)
	if stored.PriceRate != 1.5 || stored.Headers["X-Org"] != "acme" {
		t.Fatal("rate application overwrote unrelated configuration")
	}
	if err := ApplySub2APIRate(context.Background(), p.ID, 1.5, 0, stored.Sub2API.Version, Sub2APIRevision(stored), result.Account.Generation); err == nil {
		t.Fatal("an old expected price overwrote a concurrent price change")
	}
	view, _ := json.Marshal(result)
	if strings.Contains(string(view), "panel-access") {
		t.Fatal("login response exposed a token")
	}
	// Logout preserves the source and its inference keys.
	if err := s.Logout(context.Background(), result.Account.ID); err != nil {
		t.Fatal(err)
	}
	if stored, err := panelProvider(p.ID); err != nil || len(stored.KeysOn()) != 2 {
		t.Fatal("logging out removed inference keys")
	}
	ForgetBalances()
	cards = KeyBalances(context.Background())
	if len(cards) != 2 {
		t.Fatalf("logging out hid the last balances: %+v", cards)
	}
	for _, card := range cards {
		if card.Balance == "" || card.Error == "" || card.AsOf == nil {
			t.Fatalf("logging out hid a last balance or current error: %+v", card)
		}
	}
}
