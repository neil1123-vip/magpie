package provider

import (
	"crypto/rand"
	"maps"
	"regexp"
	"slices"
	"strings"
)

var secretHeader = regexp.MustCompile(`(?i)auth|key|token|secret|cookie|session|password`)

// SecretHeader reports whether a header or an environment variable by that
// name looks like it holds a key: what a backup without keys leaves empty.
func SecretHeader(name string) bool { return secretHeader.MatchString(name) }

// KeepSecrets is in, as sync or a restore brings it over here, with the
// secrets that didn't come along kept from here (#1821). keyless is that
// in came from a copy without keys: its keys and its secret headers are
// here's then. An AccessKey ID and Secret, or a balance token, that in
// hasn't at all are kept when it came without keys or with the key here
// has, the same account: a magpie from before access keys were synced
// (v0.1.1142), or one the key was never given on, sends a provider with
// its key and nothing else, and taking that whole wiped the Volcengine
// and Tencent plan cards' access keys on this computer. A key of its own
// is another account's, whose access key this one's isn't.
func KeepSecrets(in, here Provider, keyless bool) Provider {
	if keyless {
		in.Key, in.KeyName, in.Keys, in.KeyProtocol, in.KeyWeight = here.Key, here.KeyName, here.Keys, here.KeyProtocol, here.KeyWeight
		// the headers a copy without keys left out, in a map of its own:
		// in's may be shared with the list it came in
		cloned := false
		for k, v := range here.Headers {
			if _, ok := in.Headers[k]; ok || !SecretHeader(k) {
				continue
			}
			if !cloned {
				in.Headers, cloned = maps.Clone(in.Headers), true
				if in.Headers == nil {
					in.Headers = map[string]string{}
				}
			}
			in.Headers[k] = v
		}
	}
	if in.KeyPriceRates == nil {
		in.KeyPriceRates = sub2apiKeyPriceRates(here)
	}
	in.KeyPriceRates = sub2apiKeyPriceRates(in)
	if !keyless && (in.Key == "" || in.Key != here.Key) {
		in.Sub2API = restoredSub2APIBinding(in, here)
		return in
	}
	if in.Sub2API == nil && here.Sub2API != nil && samePanelOrigin(here.BalanceURL, in.BalanceURL) {
		in.Sub2API = cloneSub2APIBinding(here.Sub2API)
	}
	if in.BalanceToken == "" {
		in.BalanceToken = here.BalanceToken
	}
	if in.AccessKeyID == "" && in.SecretAccessKey == "" {
		in.AccessKeyID, in.SecretAccessKey = here.AccessKeyID, here.SecretAccessKey
	}
	in.Sub2API = restoredSub2APIBinding(in, here)
	return in
}

func normalizedSub2APIBinding(p Provider) *Sub2APIBinding {
	b := cloneSub2APIBinding(p.Sub2API)
	if b == nil {
		return nil
	}
	ids := make(map[string]bool)
	for _, key := range p.KeyList() {
		ids[key.ID] = true
	}
	b.KeyIDs = slices.DeleteFunc(b.KeyIDs, func(id string) bool { return !ids[id] })
	slices.Sort(b.KeyIDs)
	b.KeyIDs = slices.Compact(b.KeyIDs)
	b.Keys = slices.DeleteFunc(b.Keys, func(k Sub2APIKeyBinding) bool { return !ids[k.ID] })
	slices.SortFunc(b.Keys, func(a, b Sub2APIKeyBinding) int { return strings.Compare(a.ID, b.ID) })
	return b
}

func importedSub2APIBinding(p Provider) *Sub2APIBinding {
	b := normalizedSub2APIBinding(p)
	if b != nil {
		b.Version = rand.Text()
	}
	return b
}

func restoredSub2APIBinding(p, here Provider) *Sub2APIBinding {
	b, old := normalizedSub2APIBinding(p), normalizedSub2APIBinding(here)
	if b == nil {
		return nil
	}
	if old != nil && old.Version != "" && b.Session == old.Session && b.GroupID == old.GroupID &&
		b.GroupName == old.GroupName && slices.Equal(b.KeyIDs, old.KeyIDs) &&
		slices.Equal(b.Keys, old.Keys) &&
		maps.Equal(sub2apiKeyPriceRates(p), sub2apiKeyPriceRates(here)) &&
		sub2apiBalanceID(p) == sub2apiBalanceID(here) && maps.Equal(p.AccountProxies, here.AccountProxies) {
		b.Version = old.Version
	} else {
		b.Version = rand.Text()
	}
	return b
}
