package provider

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"

	"github.com/yetone/magpie/internal/sub2api"
)

// The monitor holds the account generation/settings guard; this commit adds
// source revisions and providers.json's file CAS without making another request.
func syncSub2APIAccount(ctx context.Context, targets []sub2api.Target, account *sub2api.MonitorAccount) ([]sub2api.Target, error) {
	sub2apiEdits.Lock()
	defer sub2apiEdits.Unlock()
	f, err := read()
	if err != nil {
		return nil, err
	}
	next := slices.Clone(targets)
	changed := false
	for i, target := range targets {
		if target.ID == "" || len(target.Keys) == 0 {
			continue
		}
		index := slices.IndexFunc(f.Providers, func(p Provider) bool { return p.ID == target.ID })
		if index < 0 {
			return nil, ErrSub2APIConflict
		}
		p := &f.Providers[index]
		if p.Sub2API == nil || p.Sub2API.Session != target.SessionID ||
			Sub2APIRevision(*p) != target.Revision {
			return nil, ErrSub2APIConflict
		}
		sourceIndex := slices.IndexFunc(account.Sources, func(s sub2api.SourceSnapshot) bool { return s.ID == p.ID })
		if sourceIndex < 0 {
			return nil, ErrSub2APIConflict
		}
		source := &account.Sources[sourceIndex]
		if !sameSub2APIKeySite(target.Site, *p) {
			for j := range source.Keys {
				key := &source.Keys[j]
				key.GroupVerified = false
				key.GroupError = "The source and panel account must use the same origin"
				key.SyncError = key.GroupError
			}
			continue
		}
		bindings := Sub2APIKeyBindings(*p)
		prices := sub2apiKeyPriceRates(*p)
		sourceChanged, groupsChanged := false, false
		for _, key := range source.Keys {
			if !slices.ContainsFunc(target.Keys, func(k sub2api.Key) bool { return k.ID == key.ID }) {
				return nil, ErrSub2APIConflict
			}
			if key.GroupVerified && key.GroupID != "" {
				j := slices.IndexFunc(bindings, func(b Sub2APIKeyBinding) bool { return b.ID == key.ID })
				if j < 0 {
					bindings = append(bindings, Sub2APIKeyBinding{ID: key.ID, GroupID: key.GroupID, GroupName: key.GroupName})
					groupsChanged = true
				} else if bindings[j].GroupID != key.GroupID || bindings[j].GroupName != key.GroupName {
					bindings[j].GroupID, bindings[j].GroupName = key.GroupID, key.GroupName
					groupsChanged = true
				}
			}
			// A failed or unrepresentable reading never overwrites the last price.
			if key.GroupVerified && key.Error == "" && key.Rate != nil && *key.Rate > 0 && PriceRateOK(*key.Rate) == "" {
				if prices == nil {
					prices = make(map[string]float64)
				}
				if prices[key.ID] != *key.Rate {
					prices[key.ID], sourceChanged = *key.Rate, true
				}
			}
		}
		if groupsChanged {
			p.Sub2API = cloneSub2APIBinding(p.Sub2API)
			for j := range bindings {
				bindings[j].PriceRate = 0
			}
			p.Sub2API.Keys = bindings
			p.Sub2API.GroupID, p.Sub2API.GroupName, p.Sub2API.KeyIDs = "", "", nil
			p.Sub2API.Version = rand.Text()
			p.KeyPriceRates = prices
		}
		if sourceChanged {
			p.KeyPriceRates = prices
		}
		changed = changed || sourceChanged || groupsChanged
		a := sub2api.AccountView{ID: target.SessionID, Base: target.Site, Context: target.Context, Generation: target.Generation}
		next[i] = sub2apiTarget(*p, a)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if changed {
		if err := store(f); err != nil {
			return nil, err
		}
	}
	// Expose the committed local state, never the candidate of a failed write.
	for i := range account.Sources {
		source := &account.Sources[i]
		j := slices.IndexFunc(f.Providers, func(p Provider) bool { return p.ID == source.ID })
		if j < 0 {
			continue
		}
		p := f.Providers[j]
		if p.Sub2API == nil {
			return nil, errors.New("Sub2API account was unlinked")
		}
		source.Revision, source.BindingVersion = Sub2APIRevision(p), p.Sub2API.Version
		source.GroupID, source.GroupName = p.Sub2API.GroupID, p.Sub2API.GroupName
		for k := range source.Keys {
			key := &source.Keys[k]
			rate := sub2apiLocalKeyRate(p, key.ID)
			key.LocalRate = &rate
			if key.Rate != nil && (PriceRateOK(*key.Rate) != "" || *key.Rate <= 0) {
				key.SyncError = "Sub2API billing rate cannot be used as a local price"
			}
		}
	}
	return next, nil
}
