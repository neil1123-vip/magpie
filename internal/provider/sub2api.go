package provider

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/sub2api"
)

// Sub2APIBinding contains portable references only. Group membership is a
// local choice: the billing endpoint does not prove which group owns a key.
type Sub2APIBinding struct {
	Session string              `json:"session"`
	Keys    []Sub2APIKeyBinding `json:"keys,omitempty"`
	// Legacy source-wide selections are expanded when read.
	GroupID   string   `json:"groupId,omitempty"`
	GroupName string   `json:"groupName,omitempty"`
	KeyIDs    []string `json:"keyIds,omitempty"`
	Version   string   `json:"version"`
}

type Sub2APIKeyBinding struct {
	ID        string  `json:"id"`
	GroupID   string  `json:"groupId,omitempty"`
	GroupName string  `json:"groupName,omitempty"`
	PriceRate float64 `json:"priceRate,omitempty"`
}

// Sub2APIKeyBindings reads both saved formats without assigning replaced keys.
func Sub2APIKeyBindings(p Provider) []Sub2APIKeyBinding {
	var keys []Sub2APIKeyBinding
	if p.Sub2API != nil {
		keys = slices.Clone(p.Sub2API.Keys)
	}
	if p.Sub2API != nil && keys == nil {
		for _, id := range p.Sub2API.KeyIDs {
			keys = append(keys, Sub2APIKeyBinding{ID: id, GroupID: p.Sub2API.GroupID, GroupName: p.Sub2API.GroupName})
		}
	}
	ids := make(map[string]bool)
	for _, key := range p.KeyList() {
		ids[key.ID] = true
	}
	byID := make(map[string]int, len(keys))
	for i, key := range keys {
		byID[key.ID] = i
	}
	for id, rate := range p.KeyPriceRates {
		if i, ok := byID[id]; ok {
			keys[i].PriceRate = rate
		} else {
			keys = append(keys, Sub2APIKeyBinding{ID: id, PriceRate: rate})
		}
	}
	return slices.DeleteFunc(keys, func(k Sub2APIKeyBinding) bool {
		return !ids[k.ID]
	})
}

func sub2apiKeyPriceRates(p Provider) map[string]float64 {
	rates := make(map[string]float64)
	for _, key := range Sub2APIKeyBindings(p) {
		if key.PriceRate > 0 && PriceRateOK(key.PriceRate) == "" {
			rates[key.ID] = key.PriceRate
		}
	}
	if len(rates) == 0 {
		return nil
	}
	return rates
}

// Numeric versions belonged to the unshipped draft and cannot authorize writes.
func (b *Sub2APIBinding) UnmarshalJSON(data []byte) error {
	type binding Sub2APIBinding
	var value struct {
		*binding
		Version json.RawMessage `json:"version"`
	}
	value.binding = (*binding)(b)
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	b.Version = ""
	if len(value.Version) == 0 || string(value.Version) == "null" {
		return nil
	}
	if err := json.Unmarshal(value.Version, &b.Version); err != nil {
		var legacy uint64
		return json.Unmarshal(value.Version, &legacy)
	}
	return nil
}

func cloneSub2APIBinding(b *Sub2APIBinding) *Sub2APIBinding {
	if b == nil {
		return nil
	}
	v := *b
	v.KeyIDs = slices.Clone(b.KeyIDs)
	v.Keys = slices.Clone(b.Keys)
	return &v
}

var sub2apiEdits sync.Mutex

// ErrSub2APIConflict asks an old editor to reload instead of replacing new state.
var ErrSub2APIConflict = errors.New("the source or account binding changed; reload it before trying again")

var panelService struct {
	sync.Mutex
	root    string
	session *sub2api.Manager
	monitor *sub2api.Monitor
	notify  func(sub2api.Change)
}

// Sub2APIServices keeps credentials and observations outside portable config.
func Sub2APIServices() (*sub2api.Manager, *sub2api.Monitor, error) {
	panelService.Lock()
	defer panelService.Unlock()
	root := filepath.Join(appdir.Config(), "sub2api-local")
	if panelService.root == root && panelService.session != nil {
		return panelService.session, panelService.monitor, nil
	}
	session := sub2api.New(root)
	panelRead := func(ctx context.Context, id, path string) ([]byte, error) {
		a, err := session.Info(id)
		if err != nil {
			return nil, err
		}
		return session.Read(ctx, id, http.MethodGet, a.Base+path, nil)
	}
	monitor, err := sub2api.NewMonitor(root, panelRead, readSub2APIBilling)
	if err != nil {
		return nil, nil, err
	}
	monitor.SetRawReader(func(ctx context.Context, id, path string) ([]byte, error) {
		a, err := session.Info(id)
		if err != nil {
			return nil, err
		}
		return session.ReadRaw(ctx, id, http.MethodGet, a.Base+path, nil)
	})
	monitor.SetGenerationGuard(session.WithGeneration)
	monitor.SetTargetReader(func() ([]sub2api.Target, error) { return sub2apiTargets(session) })
	panelService.root, panelService.session, panelService.monitor = root, session, monitor
	return session, monitor, nil
}

func SetSub2APINotifier(f func(sub2api.Change)) {
	panelService.Lock()
	panelService.notify = f
	panelService.Unlock()
}

// KeepSub2API belongs to the gateway, independently of the Usage page.
func KeepSub2API(ctx context.Context) {
	_, monitor, err := Sub2APIServices()
	if err != nil {
		return
	}
	monitor.Run(ctx, nil, func(c sub2api.Change) {
		panelService.Lock()
		f := panelService.notify
		panelService.Unlock()
		if f != nil {
			f(c)
		}
	})
}

func Sub2APIRevision(p Provider) string {
	v := struct {
		ID, URL, Path, Method, Token, Proxy string
		Chat, Responses, Anthropic, Gemini  string
		Headers                             map[string]string
		AccountProxies                      map[string]string
		Keys                                []KeyAccount
		Binding                             *Sub2APIBinding
		KeyPrices                           map[string]float64
	}{p.ID, p.BalanceURL, p.BalancePath, p.BalanceMethod, p.BalanceToken, p.Proxy,
		p.Chat, p.Responses, p.Anthropic, p.Gemini, p.Headers, p.AccountProxies,
		append([]KeyAccount{p.first()}, p.Keys...), p.Sub2API, p.KeyPriceRates}
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func panelProvider(id string) (Provider, error) {
	f, err := read()
	if err != nil {
		return Provider{}, err
	}
	for _, p := range f.Providers {
		if p.ID == id && hasEndpoint(p) && !p.IsVertex() {
			return p, nil
		}
	}
	return Provider{}, errors.New("save a provider with API keys before linking a panel account")
}

// Sub2APILoginInput binds both login steps to the saved source and network.
func Sub2APILoginInput(id, base, email, password string, remember bool, expected string) (sub2api.LoginRequest, error) {
	p, err := panelProvider(id)
	if err != nil {
		return sub2api.LoginRequest{}, err
	}
	if expected == "" || expected != Sub2APIRevision(p) {
		return sub2api.LoginRequest{}, ErrSub2APIConflict
	}
	if err := sub2api.CheckPanelHeaders(sub2apiHeaders(p)); err != nil {
		return sub2api.LoginRequest{}, err
	}
	ua := ""
	for k, v := range p.Headers {
		if strings.EqualFold(k, "User-Agent") {
			ua = v
		}
	}
	return sub2api.LoginRequest{
		Base: base, Email: email, Password: password, Remember: remember,
		Proxy: p.ProxyChoice(), UserAgent: ua, Target: p.ID, Revision: Sub2APIRevision(p),
	}, nil
}

func Sub2APIProvider(id string) (Provider, error) { return panelProvider(id) }

func samePanelOrigin(base, raw string) bool {
	a, err := url.Parse(base)
	if err != nil || a.Hostname() == "" || a.User != nil || a.Scheme != "https" && a.Scheme != "http" {
		return false
	}
	b, err := url.Parse(raw)
	if err != nil || b.Hostname() == "" || b.User != nil {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

func sameSub2APISite(base string, p Provider) bool {
	return samePanelOrigin(base, cmp.Or(p.BalanceURL, p.Chat, p.Responses, p.Anthropic, p.Gemini))
}

func sameSub2APIKeySite(base string, p Provider) bool {
	return samePanelOrigin(base, cmp.Or(p.Chat, p.Responses, p.Anthropic, p.Gemini))
}

func keepSub2APIBinding(p *Provider) error {
	p.KeyPriceRates = sub2apiKeyPriceRates(*p)
	if p.Sub2API == nil {
		return nil
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		return err
	}
	a, err := session.Info(p.Sub2API.Session)
	if err == nil && (p.BalanceURL == "" || !samePanelOrigin(a.Base, p.BalanceURL)) {
		return errors.New("unlink the Sub2API account before changing its balance site")
	}
	// New or replaced keys require an explicit group confirmation.
	ids := make(map[string]bool)
	for _, k := range p.KeyList() {
		ids[k.ID] = true
	}
	b := cloneSub2APIBinding(p.Sub2API)
	if err == nil && !sameSub2APIKeySite(a.Base, *p) && (len(b.Keys) > 0 || len(b.KeyIDs) > 0) {
		return errors.New("the source and panel account must use the same origin")
	}
	b.KeyIDs = slices.DeleteFunc(b.KeyIDs, func(id string) bool { return !ids[id] })
	b.Keys = slices.DeleteFunc(b.Keys, func(k Sub2APIKeyBinding) bool { return !ids[k.ID] })
	if !slices.Equal(b.KeyIDs, p.Sub2API.KeyIDs) || !slices.Equal(b.Keys, p.Sub2API.Keys) {
		b.Version = rand.Text()
	}
	p.Sub2API = b
	return nil
}

func sub2apiBalanceProvider(p Provider, a sub2api.AccountView) Provider {
	if p.BalanceURL == "" || balanceURLPath(p.BalanceURL) == sub2APIKeyUsage {
		p.BalanceURL = a.Base + sub2APIProfile
		p.BalancePath = "$data.balance"
		p.BalanceMethod = ""
	}
	return p
}

// Sub2APIBrowserValidator checks the saved query before a candidate replaces a session.
func Sub2APIBrowserValidator(p Provider) sub2api.BrowserValidator {
	return func(ctx context.Context, a sub2api.AccountView, read sub2api.BrowserRead) error {
		if !sameSub2APISite(a.Base, p) {
			return errors.New("the source and panel account must use the same origin")
		}
		p := sub2apiBalanceProvider(p, a)
		p.Sub2API = &Sub2APIBinding{Session: a.ID}
		method, err := balanceMethodOf(p.BalanceMethod)
		if err != nil {
			return err
		}
		if !samePanelOrigin(a.Base, p.BalanceURL) {
			return errors.New("the balance URL and panel account must use the same origin")
		}
		body, err := read(ctx, method, withBalanceKey(p.BalanceURL, p.Key, true), sub2apiHeaders(p))
		if err != nil {
			return err
		}
		_, err = readBalanceParts(body, balancePathOf(p))
		return err
	}
}

func sub2apiPanelUserAgent(p Provider, a sub2api.AccountView) string {
	if ua := sub2apiHeaders(p).Get("User-Agent"); ua != "" {
		return ua
	}
	return a.UserAgent
}

// BindSub2API checks the real balance before removing a pasted token.
// expected, the binding version and generation describe what the user confirmed.
func BindSub2API(ctx context.Context, id string, binding Sub2APIBinding, expected string, generation uint64) error {
	session, monitor, err := Sub2APIServices()
	if err != nil {
		return err
	}
	if err := session.WithGeneration(binding.Session, generation, nil); err != nil {
		return err
	}
	a, err := session.Info(binding.Session)
	if err != nil || a.State == sub2api.StateLoggedOut {
		return errors.New("sign in to the panel before linking it")
	}
	p, err := panelProvider(id)
	if err != nil {
		return err
	}
	revision := Sub2APIRevision(p)
	if expected == "" || expected != revision {
		return ErrSub2APIConflict
	}
	oldVersion := ""
	if p.Sub2API != nil {
		oldVersion = p.Sub2API.Version
	}
	if binding.Version != oldVersion {
		return ErrSub2APIConflict
	}
	if p.Sub2API != nil && p.Sub2API.Session == binding.Session && binding.Keys == nil && p.Sub2API.Keys != nil {
		binding.Keys = slices.Clone(p.Sub2API.Keys)
		binding.GroupID, binding.GroupName, binding.KeyIDs = "", "", nil
	}
	if !sameSub2APISite(a.Base, p) {
		return errors.New("the source and panel account must use the same origin")
	}
	p = sub2apiBalanceProvider(p, a)
	if !samePanelOrigin(a.Base, p.BalanceURL) {
		return errors.New("the balance URL and panel account must use the same origin")
	}
	userAgent := sub2apiPanelUserAgent(p, a)
	if err := session.CheckContext(binding.Session, p.ProxyChoice(), userAgent); err != nil {
		return err
	}
	for _, id := range binding.KeyIDs {
		if !slices.ContainsFunc(p.KeyList(), func(k KeyInfo) bool { return k.ID == id }) {
			return errors.New("a selected key was replaced; reload the source")
		}
	}
	seen := make(map[string]bool)
	for _, key := range binding.Keys {
		if key.ID == "" || seen[key.ID] || PriceRateOK(key.PriceRate) != "" ||
			!slices.ContainsFunc(p.KeyList(), func(k KeyInfo) bool { return k.ID == key.ID }) {
			return errors.New("a selected key was replaced; reload the source")
		}
		seen[key.ID] = true
	}
	binding.KeyIDs = slices.Compact(slices.Sorted(slices.Values(binding.KeyIDs)))
	binding.Version = rand.Text()
	p.KeyPriceRates = sub2apiKeyPriceRates(p)
	for i := range binding.Keys {
		binding.Keys[i].PriceRate = 0
	}
	p.Sub2API = cloneSub2APIBinding(&binding)
	p.BalanceToken = ""
	if _, _, _, _, ok, err := balanceRead(ctx, p); err != nil {
		return fmt.Errorf("the panel signed in but its balance could not be verified: %w", err)
	} else if !ok {
		return errors.New("the panel signed in but no account balance was found")
	}
	err = session.WithGeneration(binding.Session, generation, func() error {
		sub2apiEdits.Lock()
		defer sub2apiEdits.Unlock()
		f, err := read()
		if err != nil {
			return err
		}
		for i := range f.Providers {
			q := &f.Providers[i]
			if q.ID != id {
				continue
			}
			if Sub2APIRevision(*q) != revision {
				return ErrSub2APIConflict
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			q.Sub2API, q.BalanceURL, q.BalancePath = p.Sub2API, p.BalanceURL, p.BalancePath
			q.BalanceMethod, q.BalanceToken = p.BalanceMethod, ""
			q.KeyPriceRates = maps.Clone(p.KeyPriceRates)
			return store(f)
		}
		return errors.New("the source was removed during login")
	})
	if err == nil {
		ForgetBalances()
		monitor.Wake(binding.Session)
	}
	return err
}

// BindSub2APIKey changes one key's local group, preserving its siblings.
func BindSub2APIKey(ctx context.Context, id, sessionID, keyID, groupID, groupName, version, expected string, generation uint64) error {
	p, err := panelProvider(id)
	if err != nil {
		return err
	}
	if expected == "" || Sub2APIRevision(p) != expected {
		return ErrSub2APIConflict
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		return err
	}
	a, err := session.Info(sessionID)
	if err != nil {
		return err
	}
	if !sameSub2APIKeySite(a.Base, p) {
		return errors.New("the source and panel account must use the same origin")
	}
	if keyID == "" || !slices.ContainsFunc(p.KeyList(), func(k KeyInfo) bool { return k.ID == keyID }) {
		return errors.New("a selected key was replaced; reload the source")
	}
	b := cloneSub2APIBinding(p.Sub2API)
	if b == nil {
		b = &Sub2APIBinding{Session: sessionID}
	} else if b.Session != sessionID {
		return errors.New("unlink the Sub2API account before changing its balance site")
	}
	b.Keys = Sub2APIKeyBindings(p)
	i := slices.IndexFunc(b.Keys, func(k Sub2APIKeyBinding) bool { return k.ID == keyID })
	if i < 0 {
		b.Keys = append(b.Keys, Sub2APIKeyBinding{ID: keyID})
		i = len(b.Keys) - 1
	}
	b.Keys[i].GroupID, b.Keys[i].GroupName = groupID, groupName
	if groupID == "" {
		b.Keys[i].GroupName = ""
	}
	b.GroupID, b.GroupName, b.KeyIDs, b.Version = "", "", nil, version
	return BindSub2API(ctx, id, *b, expected, generation)
}

func UnbindSub2API(id string, version string, expected string) error {
	sub2apiEdits.Lock()
	defer sub2apiEdits.Unlock()
	f, err := read()
	if err != nil {
		return err
	}
	for i := range f.Providers {
		if f.Providers[i].ID == id {
			p := f.Providers[i]
			if expected == "" || Sub2APIRevision(p) != expected ||
				p.Sub2API == nil && version != "" || p.Sub2API != nil && (version == "" || p.Sub2API.Version != version) {
				return ErrSub2APIConflict
			}
			if f.Providers[i].Sub2API == nil {
				return nil
			}
			f.Providers[i].Sub2API = nil
			f.Providers[i].KeyPriceRates = sub2apiKeyPriceRates(p)
			if err := store(f); err != nil {
				return err
			}
			ForgetBalances()
			return nil
		}
	}
	return errors.New("the source no longer exists")
}

func sub2apiHeaders(p Provider) http.Header {
	h := make(http.Header)
	for k, v := range p.Headers {
		h.Set(k, withBalanceKey(v, p.Key, false))
	}
	return h
}

func readSub2APIAccountBalance(ctx context.Context, p Provider) ([]byte, error) {
	session, _, err := Sub2APIServices()
	if err != nil {
		return nil, err
	}
	method, err := balanceMethodOf(p.BalanceMethod)
	if err != nil {
		return nil, err
	}
	a, err := session.Info(p.Sub2API.Session)
	if err != nil {
		return nil, err
	}
	if err := session.CheckContext(p.Sub2API.Session, p.ProxyChoice(), sub2apiPanelUserAgent(p, a)); err != nil {
		return nil, err
	}
	raw, err := session.Read(ctx, p.Sub2API.Session, method, withBalanceKey(p.BalanceURL, p.Key, true), sub2apiHeaders(p))
	if err != nil {
		return nil, err
	}
	if err := session.WithGeneration(a.ID, a.Generation, nil); err != nil {
		return nil, err
	}
	return raw, nil
}

type sub2apiBalanceStamp struct {
	generation              uint64
	query, context, network string
}

func sub2apiBalanceStampOf(p Provider) (*sub2apiBalanceStamp, error) {
	stamp := &sub2apiBalanceStamp{query: sub2apiBalanceID(p)}
	session, _, err := Sub2APIServices()
	if err != nil {
		return stamp, err
	}
	a, err := session.Info(p.Sub2API.Session)
	if err != nil {
		return stamp, err
	}
	stamp.generation, stamp.context = a.Generation, a.Context
	h := sub2apiHeaders(p)
	h.Set("User-Agent", sub2apiPanelUserAgent(p, a))
	stamp.network, err = sub2api.KeyNetworkContext(p.Via(context.Background()), p.BalanceURL, h)
	return stamp, err
}

// The caller holds control through fallback, history and cache acceptance.
// Info reads local records without taking a session lock or entering control.
func (stamp *sub2apiBalanceStamp) check(session *sub2api.Manager, q SubscriptionQuota) error {
	if session == nil || stamp == nil || stamp.generation == 0 {
		return ErrSub2APIConflict
	}
	a, err := session.Info(q.Sub2API)
	if err != nil {
		return err
	}
	if a.Generation != stamp.generation || a.Context != stamp.context || a.State == sub2api.StateLoggedOut && q.Error == "" {
		return ErrSub2APIConflict
	}
	p, err := panelProvider(q.Provider)
	if err != nil {
		return err
	}
	if p.Sub2API == nil || p.Hidden || p.Off || stamp.query != sub2apiBalanceID(p) {
		return ErrSub2APIConflict
	}
	h := sub2apiHeaders(p)
	h.Set("User-Agent", sub2apiPanelUserAgent(p, a))
	network, err := sub2api.KeyNetworkContext(p.Via(context.Background()), p.BalanceURL, h)
	if err != nil {
		return err
	}
	if network != stamp.network {
		return ErrSub2APIConflict
	}
	return nil
}

func withSub2APIBalances(qs []SubscriptionQuota, commit func(*sub2api.Manager) error) error {
	if !slices.ContainsFunc(qs, func(q SubscriptionQuota) bool { return q.Sub2API != "" }) {
		return commit(nil)
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		return err
	}
	return sub2api.WithControl(filepath.Join(appdir.Config(), "sub2api-local"), func() error {
		return commit(session)
	})
}

func failedSub2APIBalance(q SubscriptionQuota, err error) SubscriptionQuota {
	q = SubscriptionQuota{Provider: q.Provider, Name: q.Name, Icon: q.Icon, User: q.User,
		Sub2API: q.Sub2API, sub2apiBalance: q.sub2apiBalance, readSeq: q.readSeq, Error: err.Error()}
	return q
}

func keepSub2APIBalance(q SubscriptionQuota) SubscriptionQuota {
	if q.sub2apiBalance == nil {
		return q
	}
	out := keepLast(q, keyTag("sub2api", q.sub2apiBalance.query))
	out.sub2apiBalance = q.sub2apiBalance
	return out
}

// Read-only exports check live authority without updating the cache or last reading.
func checkedSub2APIBalances(qs []SubscriptionQuota) []SubscriptionQuota {
	err := withSub2APIBalances(qs, func(session *sub2api.Manager) error {
		for i, q := range qs {
			if q.Sub2API == "" || q.sub2apiBalance == nil && q.AsOf != nil {
				continue // a disk reading is already historical, never current authority
			}
			if err := q.sub2apiBalance.check(session, q); err != nil {
				qs[i].Error = err.Error()
			}
		}
		return nil
	})
	for i, q := range qs {
		if q.Sub2API == "" {
			continue
		}
		if err != nil {
			qs[i].Error = err.Error()
		}
		if qs[i].Error != "" && q.AsOf == nil {
			qs[i].AsOf = q.ReadAt
		}
	}
	return qs
}

// Different custom queries of one account keep their own balance and history.
func sub2apiBalanceID(p Provider) string {
	method, _ := balanceMethodOf(p.BalanceMethod)
	b, _ := json.Marshal(struct {
		Session, URL, Method, Path, Proxy string
		Headers                           http.Header
	}{p.Sub2API.Session, withBalanceKey(p.BalanceURL, p.Key, true), method,
		balancePathOf(p), p.ProxyChoice(), sub2apiHeaders(p)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sub2apiTarget(p Provider, a sub2api.AccountView) sub2api.Target {
	b := p.Sub2API
	t := sub2api.Target{ID: p.ID, Name: p.Name, Site: a.Base, SessionID: a.ID,
		GroupID: b.GroupID, GroupName: b.GroupName, Generation: a.Generation,
		Context: a.Context, BindingVersion: b.Version, Revision: Sub2APIRevision(p)}
	bindings := Sub2APIKeyBindings(p)
	for _, key := range bindings {
		if (key.GroupID != "" || key.GroupName != "") && !slices.Contains(t.RetainedKeyIDs, key.ID) {
			t.RetainedKeyIDs = append(t.RetainedKeyIDs, key.ID)
		}
	}
	for _, key := range p.KeysOn() {
		id := keyID(key.Key)
		t.ActiveKeyIDs = append(t.ActiveKeyIDs, id)
		i := slices.IndexFunc(bindings, func(k Sub2APIKeyBinding) bool { return k.ID == id })
		if i < 0 || bindings[i].GroupID == "" && bindings[i].GroupName == "" {
			continue
		}
		q := p
		q.Key = key.Key
		// Invalid settings fail this key's read, without stopping other accounts.
		contextID, _ := sub2api.KeyNetworkContext(q.Via(context.Background()), a.Base+"/v1/sub2api/billing", sub2apiHeaders(q))
		t.Keys = append(t.Keys, sub2api.Key{ID: id, Value: key.Key, Context: contextID,
			GroupID: bindings[i].GroupID, GroupName: bindings[i].GroupName})
	}
	return t
}

func sub2apiTargets(session *sub2api.Manager) ([]sub2api.Target, error) {
	f, err := read()
	if err != nil {
		return nil, err
	}
	var out []sub2api.Target
	seen := map[string]bool{}
	accounts, err := session.Accounts()
	if err != nil {
		return nil, err
	}
	byID := map[string]sub2api.AccountView{}
	for _, a := range accounts {
		byID[a.ID] = a
	}
	for _, p := range f.Providers {
		b := p.Sub2API
		if b == nil {
			continue
		}
		a, ok := byID[b.Session]
		if !ok || a.State == sub2api.StateLoggedOut {
			continue
		}
		out = append(out, sub2apiTarget(p, a))
		seen[a.ID] = true
	}
	for _, a := range accounts {
		if !seen[a.ID] && a.State != sub2api.StateLoggedOut {
			out = append(out, sub2api.Target{ID: "watch:" + a.ID, Site: a.Base, SessionID: a.ID,
				Context: a.Context, Generation: a.Generation})
		}
	}
	return out, nil
}

// Sub2APIPreviewTargets validates the displayed source, without enabling monitoring.
func Sub2APIPreviewTargets(sessionID, providerID, expected, version string, generation uint64) ([]sub2api.Target, error) {
	session, _, err := Sub2APIServices()
	if err != nil {
		return nil, err
	}
	if err := session.WithGeneration(sessionID, generation, nil); err != nil {
		return nil, err
	}
	a, err := session.Info(sessionID)
	if err != nil {
		return nil, err
	}
	if providerID == "" {
		if expected != "" || version != "" {
			return nil, ErrSub2APIConflict
		}
		return []sub2api.Target{{Site: a.Base, SessionID: a.ID,
			Context: a.Context, Generation: generation}}, nil
	}
	p, err := panelProvider(providerID)
	if err != nil {
		return nil, err
	}
	if expected == "" || version == "" || Sub2APIRevision(p) != expected || p.Sub2API == nil ||
		p.Sub2API.Session != sessionID || p.Sub2API.Version != version {
		return nil, ErrSub2APIConflict
	}
	return []sub2api.Target{sub2apiTarget(p, a)}, nil
}

func readSub2APIBilling(ctx context.Context, target sub2api.Target, key sub2api.Key) ([]byte, error) {
	p, err := panelProvider(target.ID)
	if err != nil {
		return nil, err
	}
	bindings := Sub2APIKeyBindings(p)
	i := slices.IndexFunc(bindings, func(k Sub2APIKeyBinding) bool { return k.ID == key.ID })
	if target.Generation == 0 || target.BindingVersion == "" || target.Revision == "" ||
		p.Sub2API == nil || p.Sub2API.Session != target.SessionID || p.Sub2API.Version != target.BindingVersion ||
		Sub2APIRevision(p) != target.Revision || p.Sub2API.GroupID != target.GroupID || p.Sub2API.GroupName != target.GroupName ||
		key.ID == "" || key.ID != keyID(key.Value) || i < 0 ||
		bindings[i].GroupID != cmp.Or(key.GroupID, target.GroupID) ||
		bindings[i].GroupName != cmp.Or(key.GroupName, target.GroupName) ||
		!slices.ContainsFunc(p.KeysOn(), func(k KeyAccount) bool { return k.Key == key.Value }) {
		return nil, ErrSub2APIConflict
	}
	session, _, err := Sub2APIServices()
	if err != nil {
		return nil, err
	}
	if err := session.WithGeneration(target.SessionID, target.Generation, nil); err != nil {
		return nil, err
	}
	a, err := session.Info(target.SessionID)
	if err != nil || target.Site != a.Base || target.Context != a.Context || !sameSub2APIKeySite(a.Base, p) {
		return nil, ErrSub2APIConflict
	}
	p.Key = key.Value
	endpoint := a.Base + "/v1/sub2api/billing"
	actual, err := sub2api.KeyNetworkContext(p.Via(ctx), endpoint, sub2apiHeaders(p))
	if err != nil {
		return nil, err
	}
	if key.Context == "" || actual != key.Context {
		return nil, ErrSub2APIConflict
	}
	return sub2api.FetchKey(p.Via(ctx), http.MethodGet, endpoint, key.Value, sub2apiHeaders(p))
}

// ApplySub2APIRate supports the legacy source-wide rate action.
func ApplySub2APIRate(ctx context.Context, id string, rate, expectedRate float64, version string, expected string, generation uint64) error {
	return applySub2APIRate(ctx, id, "", rate, expectedRate, version, expected, generation)
}

func ApplySub2APIKeyRate(ctx context.Context, id, keyID string, rate, expectedRate float64, version, expected string, generation uint64) error {
	if keyID == "" {
		return ErrSub2APIConflict
	}
	return applySub2APIRate(ctx, id, keyID, rate, expectedRate, version, expected, generation)
}

func sub2apiLocalKeyRate(p Provider, id string) float64 {
	for _, key := range Sub2APIKeyBindings(p) {
		if key.ID == id && key.PriceRate > 0 {
			return key.PriceRate
		}
	}
	if p.PriceRate > 0 {
		return p.PriceRate
	}
	return 1
}

func applySub2APIRate(ctx context.Context, id, keyID string, rate, expectedRate float64, version, expected string, generation uint64) error {
	if rate <= 0 || PriceRateOK(rate) != "" {
		return errors.New("the observed rate must be positive and have at most three decimals")
	}
	p, err := panelProvider(id)
	if err != nil {
		return err
	}
	localRate := p.PriceRate
	if keyID != "" {
		localRate = sub2apiLocalKeyRate(p, keyID)
	}
	if expected == "" || version == "" || Sub2APIRevision(p) != expected || p.Sub2API == nil ||
		p.Sub2API.Version != version || localRate != expectedRate {
		return ErrSub2APIConflict
	}
	if keyID == "" && p.Sub2API.Keys != nil {
		return errors.New("confirm the source group before applying its rate")
	}
	if keyID == "" && p.Sub2API.GroupID == "" {
		return errors.New("confirm the source group before applying its rate")
	}
	revision := Sub2APIRevision(p)
	session, _, err := Sub2APIServices()
	if err != nil {
		return err
	}
	if err := session.WithGeneration(p.Sub2API.Session, generation, nil); err != nil {
		return err
	}
	a, err := session.Info(p.Sub2API.Session)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	target := sub2apiTarget(p, a)
	if len(target.ActiveKeyIDs) == 0 {
		return errors.New("the source has no active key")
	}
	if keyID != "" {
		target.Keys = slices.DeleteFunc(target.Keys, func(k sub2api.Key) bool { return k.ID != keyID })
		if len(target.Keys) != 1 {
			return errors.New("confirm the group binding for every active key before applying a source rate")
		}
	} else if len(target.Keys) != len(target.ActiveKeyIDs) {
		return errors.New("confirm the group binding for every active key before applying a source rate")
	}
	for _, key := range target.Keys {
		body, err := readSub2APIBilling(ctx, target, key)
		if err != nil {
			return err
		}
		n, err := sub2api.BillingRate(body)
		if err != nil || *n != rate {
			return errors.New("the active keys no longer report this same rate; refresh them before applying")
		}
	}
	return session.WithGeneration(a.ID, generation, func() error {
		sub2apiEdits.Lock()
		defer sub2apiEdits.Unlock()
		f, err := read()
		if err != nil {
			return err
		}
		for i := range f.Providers {
			q := &f.Providers[i]
			if q.ID == id {
				currentRate := q.PriceRate
				if keyID != "" {
					currentRate = sub2apiLocalKeyRate(*q, keyID)
				}
				if Sub2APIRevision(*q) != revision || currentRate != expectedRate {
					return ErrSub2APIConflict
				}
				current := sub2apiTarget(*q, a)
				if keyID != "" {
					current.Keys = slices.DeleteFunc(current.Keys, func(k sub2api.Key) bool { return k.ID != keyID })
				}
				if !slices.Equal(current.ActiveKeyIDs, target.ActiveKeyIDs) || !slices.Equal(current.Keys, target.Keys) {
					return ErrSub2APIConflict
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				if keyID == "" {
					q.PriceRate = rate
				} else {
					q.KeyPriceRates = sub2apiKeyPriceRates(*q)
					if q.KeyPriceRates == nil {
						q.KeyPriceRates = make(map[string]float64)
					}
					q.KeyPriceRates[keyID] = rate
				}
				return store(f)
			}
		}
		return errors.New("the source was removed while its rate was checked")
	})
}

// Sub2APISafeBinding is detached from provider caches before GUI serialization.
func Sub2APISafeBinding(p Provider) *Sub2APIBinding {
	b := cloneSub2APIBinding(p.Sub2API)
	if b != nil {
		b.Keys = Sub2APIKeyBindings(p)
	}
	return b
}
