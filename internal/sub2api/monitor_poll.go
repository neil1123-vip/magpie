package sub2api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (m *Monitor) pollAccount(ctx context.Context, previous MonitorAccount, targets []Target, watched []Group) (MonitorAccount, monitorBaseline, Change) {
	now := m.now()
	account := previous
	account.LastAttempt, account.NextPoll = now, now.Add(pollInterval)
	account.Error, account.GroupsError, account.BillingError, account.ObservationError = "", "", "", ""
	account.Complete, account.Observation = false, nil
	contextKnown := targets[0].Context != ""
	for _, target := range targets[1:] {
		contextKnown = contextKnown && target.Context == targets[0].Context
	}
	identity := monitorIdentity(account.SessionID, account.Site, targets[0].Context)
	m.mu.Lock()
	baseline := m.baselines[account.SessionID]
	originalBaseline := baseline
	if contextKnown && baseline.Identity != "" && baseline.Identity != identity {
		delete(m.modes, account.SessionID)
		for key, limit := range m.cool {
			if strings.HasPrefix(key, account.SessionID+"\x00") && !strings.HasPrefix(key, account.SessionID+"\x00billing:") &&
				limit.Status != http.StatusTooManyRequests && limit.RetryAfter == 0 {
				delete(m.cool, key)
			}
		}
	}
	m.mu.Unlock()
	if contextKnown && baseline.Identity != identity {
		baseline = monitorBaseline{Identity: identity, Keys: baseline.Keys}
	}
	// A copied map still shares storage with the committed snapshot/baseline.
	groupBaselines := make(map[string]groupBaseline, len(baseline.Groups))
	for id, value := range baseline.Groups {
		groupBaselines[id] = value
	}
	baseline.Groups = groupBaselines
	keyBaselines := make(map[string]keyBaseline)
	sampledKeys := make(map[string]bool)
	for _, target := range targets {
		for _, key := range target.Keys {
			sampledKeys[target.ID+"\x00"+key.ID] = true
		}
		retained := target.RetainedKeyIDs
		if retained == nil {
			for _, key := range target.Keys {
				retained = append(retained, key.ID)
			}
		}
		for _, id := range retained {
			slot := target.ID + "\x00" + id
			if value, exists := baseline.Keys[slot]; exists {
				keyBaselines[slot] = value
			}
		}
	}
	actions := make(map[string]bool)
	for resource, pending := range previous.ActionResources {
		actions[resource] = pending
	}
	noteAction := func(resource string, err error) {
		if err == nil {
			delete(actions, resource)
		} else if monitorNeedsAction(err) {
			actions[resource] = true
		}
	}
	account.GroupsKnown = baseline.GroupsKnown
	groups := make(map[string]Group)
	for _, group := range previous.Groups {
		group.Visible, group.Rate, group.Enabled, group.Observation = nil, nil, nil, nil
		group.ObservedAt, group.Error = time.Time{}, ""
		groups[group.ID] = group
	}
	addGroup := func(group Group) {
		if group.ID != "" && groups[group.ID].ID == "" {
			groups[group.ID] = Group{ID: group.ID, Name: group.Name}
		}
	}
	for _, group := range watched {
		addGroup(group)
	}
	for _, target := range targets {
		addGroup(Group{ID: target.GroupID, Name: target.GroupName})
		for _, key := range target.Keys {
			addGroup(Group{ID: key.GroupID, Name: key.GroupName})
		}
		for _, group := range target.WatchGroups {
			addGroup(group)
		}
	}
	for resource := range actions {
		if strings.HasPrefix(resource, "billing:") {
			if !sampledKeys[strings.TrimPrefix(resource, "billing:")] {
				delete(actions, resource)
			}
		} else if strings.HasPrefix(resource, "observation:") {
			delete(actions, resource)
		}
	}
	items := make([]ChangeItem, 0)
	body, err := m.readPanel(ctx, account.SessionID, "/api/v1/groups/available", false)
	if err == nil {
		var available []Group
		var complete bool
		available, complete, err = parseGroups(body)
		if err != nil {
			m.failed(ctx, account.SessionID, "/api/v1/groups/available", err)
		}
		if err == nil {
			seen := make(map[string]bool)
			for _, current := range available {
				seen[current.ID] = true
				group := groups[current.ID]
				group.ID, group.Name = current.ID, current.Name
				group.Visible, group.Rate, group.Enabled = current.Visible, current.Rate, current.Enabled
				group.LastSuccess, group.LastVisible, group.LastVisibleAt = now, current.Visible, now
				value := baseline.Groups[group.ID]
				groupIdentity := monitorIdentity(identity, group.ID)
				if contextKnown && value.Identity != groupIdentity {
					value.Identity, value.Rate, value.Enabled = groupIdentity, nil, nil
					value.RateAt, value.EnabledAt = time.Time{}, time.Time{}
				}
				if complete {
					if contextKnown && baseline.GroupsKnown {
						if value.Visible == nil || !*value.Visible {
							items = append(items, ChangeItem{Kind: "group_added", GroupID: group.ID, GroupName: group.Name, After: group.Name, BeforeAt: value.VisibleAt, AfterAt: now})
						} else if value.Name != group.Name {
							items = append(items, ChangeItem{Kind: "group_renamed", GroupID: group.ID, GroupName: group.Name, Before: value.Name, After: group.Name, BeforeAt: value.VisibleAt, AfterAt: now})
						}
					}
					value.Name, value.Visible, value.VisibleAt = group.Name, group.Visible, now
				}
				if group.Rate != nil {
					group.LastRate, group.LastRateAt = group.Rate, now
					if value.Identity != "" {
						if contextKnown && value.Rate != nil && *value.Rate != *group.Rate {
							items = append(items, ChangeItem{Kind: "group_rate", GroupID: group.ID, GroupName: group.Name, Before: monitorRate(value.Rate), After: monitorRate(group.Rate), BeforeAt: value.RateAt, AfterAt: now})
						}
						value.Rate, value.RateAt = group.Rate, now
					}
				}
				if group.Enabled != nil {
					group.LastEnabled, group.LastEnabledAt = group.Enabled, now
					if value.Identity != "" {
						if contextKnown && value.Enabled != nil && *value.Enabled != *group.Enabled {
							items = append(items, ChangeItem{Kind: "group_enabled", GroupID: group.ID, GroupName: group.Name, Before: strconv.FormatBool(*value.Enabled), After: strconv.FormatBool(*group.Enabled), BeforeAt: value.EnabledAt, AfterAt: now})
						}
						value.Enabled, value.EnabledAt = group.Enabled, now
					}
				}
				baseline.Groups[group.ID] = value
				groups[group.ID] = group
			}
			if complete {
				for id, group := range groups {
					if seen[id] {
						continue
					}
					visible := false
					group.Visible = &visible
					group.LastVisible, group.LastVisibleAt = group.Visible, now
					groups[id] = group
					value := baseline.Groups[id]
					if contextKnown && baseline.GroupsKnown && value.Visible != nil && *value.Visible {
						items = append(items, ChangeItem{Kind: "group_unavailable", GroupID: id, GroupName: value.Name, Before: value.Name, BeforeAt: value.VisibleAt, AfterAt: now})
					}
					value.Name, value.Visible, value.VisibleAt = group.Name, group.Visible, now
					baseline.Groups[id] = value
				}
				baseline.GroupsKnown = true
			}
			account.GroupsKnown = baseline.GroupsKnown
			account.Complete = complete
			account.GroupsLastSuccess = now
		}
	}
	account.GroupsError = monitorError(err)
	noteAction("groups", err)

	// Requests sharing key and effective context are read once, even across sources.
	type billingResult struct {
		rate *float64
		err  error
	}
	billing := make(map[string]billingResult)
	oldSources := make(map[string]SourceSnapshot)
	for _, source := range previous.Sources {
		oldSources[source.ID] = source
	}
	account.Sources = make([]SourceSnapshot, 0, len(targets))
	billingCount := 0
	for _, target := range targets {
		source := SourceSnapshot{
			ID: target.ID, Name: target.Name, GroupID: target.GroupID, GroupName: target.GroupName,
			BindingVersion: target.BindingVersion, Revision: target.Revision,
			ActiveKeyIDs: append([]string(nil), target.ActiveKeyIDs...),
		}
		oldKeys := make(map[string]KeySnapshot)
		for _, key := range oldSources[target.ID].Keys {
			oldKeys[key.ID] = key
		}
		allCurrent := len(target.Keys) > 0
		perKeyGroups, differentRates := false, false
		for _, key := range target.Keys {
			groupID, groupName := key.GroupID, key.GroupName
			if groupID == "" && groupName == "" {
				groupID, groupName = target.GroupID, target.GroupName
			} else {
				perKeyGroups = true
			}
			billingCount++
			requestID := monitorIdentity(account.SessionID, target.Site, key.Value, key.Context)
			result, exists := billing[requestID]
			if !exists {
				raw, readErr := m.request(ctx, account.SessionID, "billing:"+requestID, func(ctx context.Context) ([]byte, error) {
					return m.billing(ctx, target, key)
				})
				result.err = readErr
				if readErr == nil {
					result.rate, result.err = BillingRate(raw)
					if result.err != nil {
						m.failed(ctx, account.SessionID, "billing:"+requestID, result.err)
					}
				}
				billing[requestID] = result
			}
			slot := target.ID + "\x00" + key.ID
			value := baseline.Keys[slot]
			binding := groupID
			if binding == "" {
				binding = "name:" + groupName
			}
			keyIdentity := monitorIdentity(account.SessionID, target.Site, binding, key.ID, key.Value, key.Context)
			keyKnown := key.Context != ""
			current := oldKeys[key.ID]
			if keyKnown && value.Identity != "" && value.Identity != keyIdentity {
				current = KeySnapshot{}
			}
			if keyKnown && value.Identity != keyIdentity {
				value = keyBaseline{Identity: keyIdentity}
			}
			if current.LastRate == nil && value.Identity == keyIdentity && value.Rate != nil {
				current.LastRate, current.LastRateAt, current.LastSuccess = value.Rate, value.At, value.At
			}
			current.ID, current.Rate, current.Error = key.ID, nil, monitorError(result.err)
			current.GroupID, current.GroupName = groupID, groupName
			if result.err == nil && result.rate != nil {
				current.Rate, current.LastSuccess, current.LastRate, current.LastRateAt = result.rate, now, result.rate, now
				if keyKnown && value.Rate != nil && *value.Rate != *result.rate {
					items = append(items, ChangeItem{
						Kind: "key_rate", GroupID: groupID, GroupName: groupName,
						SourceID: source.ID, KeyID: key.ID, Before: monitorRate(value.Rate), After: monitorRate(result.rate),
						BeforeAt: value.At, AfterAt: now,
					})
				}
				if keyKnown {
					value.Rate, value.At = result.rate, now
				}
				if source.Rate == nil {
					source.Rate = result.rate
				} else if *source.Rate != *result.rate {
					differentRates = true
				}
			} else {
				allCurrent = false
				if account.BillingError == "" {
					account.BillingError = monitorError(result.err)
				}
			}
			keyBaselines[slot] = value
			source.Keys = append(source.Keys, current)
			noteAction("billing:"+slot, result.err)
		}
		source.Conflict = differentRates && !perKeyGroups
		if !allCurrent || differentRates {
			source.Rate = nil
		}
		account.Sources = append(account.Sources, source)
	}
	baseline.Keys = keyBaselines
	if billingCount > 0 && account.BillingError == "" {
		account.BillingLastSuccess = now
	}
	// V1/V2 observations stay deferred; background and preview read only groups/billing.
	account.Groups = make([]Group, 0, len(groups))
	for _, group := range groups {
		account.Groups = append(account.Groups, group)
	}
	sort.Slice(account.Groups, func(i, j int) bool { return account.Groups[i].ID < account.Groups[j].ID })
	for _, resourceError := range []string{account.GroupsError, account.BillingError, account.ObservationError} {
		if resourceError != "" {
			account.Error = resourceError
			break
		}
	}
	if account.Error == "" {
		account.LastSuccess = now
	}
	m.mu.Lock()
	if limited := m.cool[account.SessionID+"\x00limited"]; limited.Until.After(account.NextPoll) {
		account.NextPoll = limited.Until
	}
	m.mu.Unlock()
	if ctx.Err() != nil {
		return previous, originalBaseline, Change{}
	}
	// Unknown context cannot replace the last baseline from an identified session.
	if !contextKnown {
		baseline.Identity, baseline.GroupsKnown, baseline.Groups = originalBaseline.Identity, originalBaseline.GroupsKnown, originalBaseline.Groups
	}
	account.ActionResources, account.NeedsAction = actions, len(actions) > 0
	if account.NeedsAction && !previous.NeedsAction {
		items = append(items, ChangeItem{Kind: "needs_action", After: "Sign in or complete verification", AfterAt: now})
	}
	change := Change{SessionID: account.SessionID, Site: account.Site, At: now, Items: items}
	if len(items) > 0 {
		change.ID = monitorChangeID(account.SessionID, now, items)
	}
	return account, baseline, change
}

func monitorNeedsAction(err error) bool {
	var detail *Error
	if !errors.As(err, &detail) {
		return false
	}
	return detail.Kind == KindAuth || detail.Kind == KindChallenge || detail.Code == "REFRESH_UNCERTAIN" ||
		detail.Challenge || detail.Status == http.StatusUnauthorized
}

func monitorIdentity(values ...string) string {
	body, _ := json.Marshal(values)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

func monitorHasMetrics(value *Observation) bool {
	return value.Availability != nil || value.ProbeLatencyMS != nil || value.TTFTP50MS != nil || value.CacheRate != nil
}

func monitorObservationHistory(previous map[string]ObservedMetric, value *Observation, at time.Time) map[string]ObservedMetric {
	history := make(map[string]ObservedMetric, len(previous)+4)
	for key, metric := range previous {
		history[key] = metric
	}
	for _, metric := range []struct {
		name  string
		value *float64
	}{
		{"availability", value.Availability}, {"probeLatencyMs", value.ProbeLatencyMS},
		{"ttftP50Ms", value.TTFTP50MS}, {"cacheRate", value.CacheRate},
	} {
		if metric.value == nil {
			continue
		}
		item := *value
		item.Availability, item.ProbeLatencyMS, item.TTFTP50MS, item.CacheRate = nil, nil, nil, nil
		switch metric.name {
		case "availability":
			item.Availability = metric.value
		case "probeLatencyMs":
			item.ProbeLatencyMS = metric.value
		case "ttftP50Ms":
			item.TTFTP50MS = metric.value
		case "cacheRate":
			item.CacheRate = metric.value
		}
		history[metric.name] = ObservedMetric{Observation: item, ObservedAt: at}
	}
	return history
}

func monitorRate(rate *float64) string {
	if rate == nil {
		return ""
	}
	return strconv.FormatFloat(*rate, 'g', -1, 64)
}

type observationResult struct {
	value *Observation
	err   error
}

func (m *Monitor) observations(ctx context.Context, session string, groups []Group) map[string]observationResult {
	results := make(map[string]observationResult)
	if len(groups) == 0 {
		return results
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	type readResult struct {
		body []byte
		err  error
	}
	reads := make(map[string]readResult)
	read := func(path string, raw bool) ([]byte, error) {
		key := path
		if raw {
			key += "|raw"
		}
		if result, ok := reads[key]; ok {
			return result.body, result.err
		}
		body, err := m.readPanel(ctx, session, path, raw)
		reads[key] = readResult{body, err}
		return body, err
	}
	mode, modeErr := m.monitorMode(ctx, session, read)
	if modeErr != nil && !monitorFallback(modeErr) {
		for _, group := range groups {
			key := group.ID
			if key == "" {
				key = "name:" + group.Name
			}
			results[key] = observationResult{err: modeErr}
		}
		return results
	}
	var matrix []map[string]any
	var matrixErr error
	if mode != "v1" {
		matrixCtx, matrixCancel := context.WithTimeout(ctx, 5*time.Second)
		matrixBody, err := m.readPanel(matrixCtx, session, "/api/v1/channel-monitor-v2/matrix?group_by=platform_group&range=90m", false)
		matrixCancel()
		matrixErr = err
		if err == nil {
			matrix, matrixErr = parseMatrix(matrixBody)
			if matrixErr != nil {
				m.failed(ctx, session, "/api/v1/channel-monitor-v2/matrix?group_by=platform_group&range=90m", matrixErr)
			}
		}
	}
	readV1 := func(group Group, raw bool) (*Observation, error) {
		body, err := read("/api/v1/channel-monitors", raw)
		if monitorStatus(err) == http.StatusUnauthorized && raw {
			body, err = read("/api/v1/channel-monitors", false)
		}
		if err != nil {
			return nil, err
		}
		return parseV1(body, group)
	}
	for _, group := range groups {
		key := group.ID
		if key == "" {
			key = "name:" + group.Name
		}
		if mode == "v1" {
			value, err := readV1(group, false)
			results[key] = observationResult{value, err}
			continue
		}
		if matrixErr == nil {
			if row, err := selectMatrix(matrix, group); err == nil {
				match := "id"
				if group.ID == "" {
					match = "name"
				}
				results[key] = observationResult{value: parseV2(row, "v2-matrix", "group", match)}
				continue
			}
		} else if !monitorFallback(matrixErr) {
			results[key] = observationResult{err: matrixErr}
			continue
		}
		query := url.Values{"range": {"90m"}}
		if group.ID != "" {
			query.Set("group_id", group.ID)
		}
		body, err := read("/api/v1/channel-monitor-v2/snapshot?"+query.Encode(), true)
		if err != nil && (monitorStatus(err) == http.StatusUnauthorized || monitorUnsupported(err)) && !monitorChallenge(err) {
			value, fallbackErr := readV1(group, true)
			if fallbackErr == nil || monitorStatus(fallbackErr) == http.StatusTooManyRequests {
				results[key] = observationResult{value, fallbackErr}
				continue
			}
		}
		if err != nil {
			results[key] = observationResult{err: err}
			continue
		}
		row, err := monitorObject(body)
		if err != nil {
			m.failed(ctx, session, "/api/v1/channel-monitor-v2/snapshot?"+query.Encode()+"|raw", err)
			results[key] = observationResult{err: err}
			continue
		}
		if id := monitorGroupID(row); group.ID != "" && id != "" && id != group.ID {
			results[key] = observationResult{err: fmt.Errorf("Sub2API snapshot returned a different group")}
			continue
		}
		scope, match := "global", "none"
		if group.ID != "" {
			scope, match = "group", "id"
		}
		results[key] = observationResult{value: parseV2(row, "v2-snapshot", scope, match)}
	}
	return results
}

func (m *Monitor) monitorMode(ctx context.Context, session string, read func(string, bool) ([]byte, error)) (string, error) {
	m.mu.Lock()
	cached := m.modes[session]
	m.mu.Unlock()
	if m.now().Before(cached.until) {
		return cached.mode, cached.err
	}
	body, err := read("/api/v1/settings/public", false)
	mode := ""
	if err == nil {
		if row, parseErr := monitorObject(body); parseErr == nil {
			value, _ := row["channel_monitor_mode"].(string)
			if value == "v1" || value == "v2" {
				mode = value
			}
		}
	}
	ttl := 15 * time.Minute
	if mode == "" {
		ttl = 30 * time.Second
	}
	m.mu.Lock()
	m.modes[session] = modeCache{mode: mode, until: m.now().Add(ttl), err: err}
	m.mu.Unlock()
	return mode, err
}

func monitorStatus(err error) int {
	var detail *Error
	if errors.As(err, &detail) {
		return detail.Status
	}
	return 0
}

func monitorChallenge(err error) bool {
	var detail *Error
	return errors.As(err, &detail) && detail.Challenge
}

func monitorUnsupported(err error) bool {
	switch monitorStatus(err) {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return true
	}
	return false
}

func monitorFallback(err error) bool {
	if monitorChallenge(err) {
		return false
	}
	status := monitorStatus(err)
	if status == 401 || status == 403 || status == 429 {
		return false
	}
	return status == 0 || monitorUnsupported(err)
}
