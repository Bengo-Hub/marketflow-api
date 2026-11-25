// Package adaccounts decides which connected ad account each platform's ads run on (owner,
// 2026-11-25: Run ads picked a personal Meta ad account over the business's own). Boost, Run ads,
// WhatsApp, the autopilot and the Ads setup page all read it, so there is one rule.
//
// The choice is kept in Tenant.metadata.ad_accounts by the platform's own account id, not on the
// connected account row, so a reconnect (which rewrites the rows) or a disconnect keeps it.
package adaccounts

import (
	"errors"
	"sort"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// TenantKey is the tenant metadata object: {"meta": {"default": "act_1", "hidden": ["act_2"]}}.
const TenantKey = "ad_accounts"

// Platforms with ad accounts.
const (
	Meta   = "meta"
	Google = "google"
	TikTok = "tiktok"
)

// TypeOf is the connected account type of a platform's ad accounts.
var TypeOf = map[string]string{Meta: "meta_ad_account", Google: "google_ads_customer", TikTok: "tiktok_advertiser"}

// Types are the connected account types of every platform's ad accounts.
func Types() []string {
	return []string{TypeOf[Meta], TypeOf[Google], TypeOf[TikTok]}
}

// PlatformOf is the platform of a connected account type, "" when it is not an ad account.
func PlatformOf(accountType string) string {
	for p, t := range TypeOf {
		if t == accountType {
			return p
		}
	}
	return ""
}

// Connected account metadata about who owns a Meta ad account, written at connect and by
// adsmanager when a row does not carry it yet. An empty business_id means Meta said the account
// belongs to no business portfolio; a missing key means it is not known.
const (
	BusinessID   = "business_id"
	BusinessName = "business_name"
	OwnerChecked = "owner_checked_at"
)

// Ownership of an account.
const (
	OwnerPortfolio = "portfolio"
	OwnerPersonal  = "personal"
	OwnerUnknown   = "unknown"
)

// Owner says whether a Meta ad account belongs to a business portfolio or to a person. Other
// platforms have no such difference.
func Owner(a *ent.ConnectedAccount) string {
	if a.AccountType != TypeOf[Meta] {
		return OwnerUnknown
	}
	v, ok := a.Metadata[BusinessID].(string)
	switch {
	case !ok:
		return OwnerUnknown
	case strings.TrimSpace(v) == "":
		return OwnerPersonal
	}
	return OwnerPortfolio
}

// Choice is one platform's choice, by the platform's own account ids.
type Choice struct {
	Default string   `json:"default,omitempty"`
	Hidden  []string `json:"hidden,omitempty"`
}

// Prefs are a tenant's choices per platform.
type Prefs map[string]Choice

// Read reads the tenant's choices; anything malformed is ignored.
func Read(meta map[string]any) Prefs {
	out := Prefs{}
	m, _ := meta[TenantKey].(map[string]any)
	for p, v := range m {
		c, ok := v.(map[string]any)
		if !ok || TypeOf[p] == "" {
			continue
		}
		ch := Choice{}
		ch.Default, _ = c["default"].(string)
		list, _ := c["hidden"].([]any)
		for _, x := range list {
			if s, ok := x.(string); ok && s != "" {
				ch.Hidden = append(ch.Hidden, s)
			}
		}
		out[p] = ch
	}
	return out
}

// Hidden reports whether the tenant hid the account.
func (p Prefs) Hidden(platform, externalID string) bool {
	for _, x := range p[platform].Hidden {
		if x == externalID {
			return true
		}
	}
	return false
}

// Errors of Set.
var (
	ErrNotAdAccount  = errors.New("not an ad account")
	ErrHiddenDefault = errors.New("a hidden account cannot be the default")
)

// Set applies a change to one account and returns the new choices. Hiding the default clears it,
// and making a hidden account the default shows it again.
func (p Prefs) Set(platform, externalID string, makeDefault, hidden *bool) (Prefs, error) {
	if TypeOf[platform] == "" || strings.TrimSpace(externalID) == "" {
		return nil, ErrNotAdAccount
	}
	if makeDefault != nil && *makeDefault && hidden != nil && *hidden {
		return nil, ErrHiddenDefault
	}
	c := p[platform]
	hide := p.Hidden(platform, externalID)
	if hidden != nil {
		hide = *hidden
		if hide && c.Default == externalID {
			c.Default = ""
		}
	}
	if makeDefault != nil {
		if *makeDefault {
			c.Default, hide = externalID, false
		} else if c.Default == externalID {
			c.Default = ""
		}
	}
	list := make([]string, 0, len(c.Hidden)+1)
	for _, x := range c.Hidden {
		if x != externalID {
			list = append(list, x)
		}
	}
	if hide {
		list = append(list, externalID)
	}
	c.Hidden = list
	out := Prefs{}
	for k, v := range p {
		out[k] = v
	}
	out[platform] = c
	return out, nil
}

// Usable are the accounts not hidden, the one ads run on first: the tenant's chosen default, else
// accounts in a business portfolio before unknown and personal ones, then (Meta) KES accounts,
// where M-Pesa funding works, then by name. legacy is a connected account id chosen before this
// package (metadata.whatsapp_boost.ad_account_connected_id); it counts when no default is chosen.
func (p Prefs) Usable(platform string, accounts []*ent.ConnectedAccount, legacy string) []*ent.ConnectedAccount {
	out := make([]*ent.ConnectedAccount, 0, len(accounts))
	for _, a := range accounts {
		if !p.Hidden(platform, a.ExternalID) {
			out = append(out, a)
		}
	}
	def := p[platform].Default
	rank := func(a *ent.ConnectedAccount) int {
		switch {
		case def != "" && a.ExternalID == def:
			return 0
		case def == "" && legacy != "" && a.ID.String() == legacy:
			return 1
		}
		switch Owner(a) {
		case OwnerPortfolio:
			return 2
		case OwnerPersonal:
			return 4
		}
		return 3
	}
	kes := func(a *ent.ConnectedAccount) bool {
		s, _ := a.Metadata["currency"].(string)
		return platform == Meta && strings.EqualFold(strings.TrimSpace(s), "KES")
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra < rb
		}
		if ka, kb := kes(a), kes(b); ka != kb {
			return ka
		}
		return a.DisplayName < b.DisplayName
	})
	return out
}

// Pick is the account ads run on, nil when none can be used.
func (p Prefs) Pick(platform string, accounts []*ent.ConnectedAccount, legacy string) *ent.ConnectedAccount {
	if u := p.Usable(platform, accounts, legacy); len(u) > 0 {
		return u[0]
	}
	return nil
}
