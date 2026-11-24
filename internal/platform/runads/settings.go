// Package runads holds the Run ads rules (owner, 2026-11-24): after a video is approved the owner
// gets one ad plan for Meta, Google Ads and TikTok with a daily budget, run days and a total spend
// cap, and nothing is spent until the owner confirms it.
//
// Every value has a platform default in service_config (tenant_id NULL, written by cmd/seed when
// missing) and a tenant override in Tenant.metadata.run_ads, set by an owner or admin on the
// Settings page. Amounts in service_config are in KES and are converted to the tenant's currency
// with treasury's rate; amounts a tenant sets are in the tenant's own currency.
package runads

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Platforms a plan can hold, in the order the plan shows them.
const (
	Meta   = "meta"
	Google = "google"
	TikTok = "tiktok"
)

// AllPlatforms is every platform in plan order.
var AllPlatforms = []string{Meta, Google, TikTok}

// Budget tiers.
const (
	TierLow         = "low"
	TierRecommended = "recommended"
	TierHigh        = "high"
	TierCustom      = "custom"
)

// TenantKey is the tenant metadata object holding the tenant's overrides:
//
//	{"platforms": ["meta", "google"], "tier": "recommended", "daily_budget": {"meta": 1000},
//	 "days": 7, "total_cap": 20000}
//
// Every key is optional. daily_budget and total_cap are whole units of the tenant's currency;
// a daily_budget for a platform replaces the tier for that platform; total_cap 0 means no cap
// beyond the plan's own total.
const TenantKey = "run_ads"

// Platform defaults in service_config (tenant_id NULL).
const (
	// KeyPlatforms: the platforms switched on in a new plan ("meta,google,tiktok").
	KeyPlatforms = "run_ads.platforms"
	// KeyTier: the tier a new plan picks ("recommended").
	KeyTier = "run_ads.tier"
	// KeyDays: how many days a new plan runs ("7").
	KeyDays = "run_ads.days"
	// KeyTotalCapKES: the platform's default total spend cap in KES ("0" for none).
	KeyTotalCapKES = "run_ads.total_cap_kes"
	// KeyRuleOfThumbKES: the daily budgets per platform and tier in KES, used when a platform gives
	// no estimate of its own. JSON {"meta": {"low": 650, "recommended": 1300, "high": 2600}, ...}.
	// These are rules of thumb and the plan labels them so.
	KeyRuleOfThumbKES = "run_ads.rule_of_thumb_kes"
	// KeyTikTokMinDailyUSD: TikTok's minimum daily budget per ad group in USD ("20"; TikTok Ads
	// help "About budget", read 2026-11-24).
	KeyTikTokMinDailyUSD = "run_ads.tiktok_min_daily_usd"
	// KeyTikTokMode: "guided" (prefilled links) or "api" (Marketing API, once TikTok approves the
	// app and a launcher is registered; until then "api" still runs guided).
	KeyTikTokMode = "run_ads.tiktok_mode"
	// KeyTikTokLinks: the TikTok Ads Manager pages as JSON {"campaign": url, "payment": url,
	// "home": url}; {advertiser_id} is replaced with the connected advertiser's id.
	KeyTikTokLinks = "run_ads.tiktok_links"
	// KeyGoogleBillingURL: the Google Ads billing page; {customer_id} is replaced with the
	// customer id (digits) when the template holds it.
	KeyGoogleBillingURL = "run_ads.google_billing_url"
	// KeyGoogleStatus: the status a new Google campaign is created in, "ENABLED" or "PAUSED".
	KeyGoogleStatus = "run_ads.google_campaign_status"
	// KeyGoogleBudgetPeriod: "daily" (CampaignBudget period DAILY, amount = daily budget) or
	// "total" (CUSTOM_PERIOD with total_amount_micros = the platform total).
	KeyGoogleBudgetPeriod = "run_ads.google_budget_period"
)

// Limits on a plan.
const (
	MinDays = 1
	MaxDays = 90
	// maxAmount keeps a typo from becoming a budget nobody meant (whole units).
	maxAmount = int64(1_000_000_000)
)

// Tiers is one platform's daily budgets per tier, in KES (platform config) or tenant currency.
type Tiers map[string]int64

// Settings are one tenant's effective Run ads defaults.
type Settings struct {
	Platforms []string
	Tier      string
	// DailyBudget is the tenant's own daily amount per platform, tenant currency (none in
	// platform config).
	DailyBudget map[string]int64
	Days        int
	// TotalCap is the tenant's cap in tenant currency; TotalCapKES is the platform's in KES.
	// Merge sets TotalCap only from the tenant; the plan converts TotalCapKES when TotalCap is 0.
	TotalCap    int64
	TotalCapKES int64
	// TotalCapSet is true when the tenant set total_cap itself; its 0 then means no cap even when
	// the platform has one.
	TotalCapSet bool
	// RuleOfThumbKES is per platform; a platform missing here has no rule of thumb.
	RuleOfThumbKES map[string]Tiers
	TikTokMinUSD   int64
	TikTokMode     string
	TikTokLinks    map[string]string
	GoogleBilling  string
	GoogleStatus   string
	GoogleTotal    bool
}

// Fallback is what applies when a platform row is missing or unreadable: every platform on, the
// recommended tier, 7 days (the default duration of Meta's boost flow), no cap, no rule of thumb
// (the plan then asks for an amount instead of inventing one), TikTok guided, and a Google
// campaign created ENABLED on a daily budget (the owner has confirmed the spend by then). cmd/seed
// writes the same values, plus the rule of thumb, so an admin can change them.
func Fallback() Settings {
	return Settings{
		Platforms:      append([]string(nil), AllPlatforms...),
		Tier:           TierRecommended,
		DailyBudget:    map[string]int64{},
		Days:           7,
		RuleOfThumbKES: map[string]Tiers{},
		TikTokMode:     "guided",
		TikTokLinks:    DefaultTikTokLinks(),
		GoogleBilling:  DefaultGoogleBillingURL,
		GoogleStatus:   "ENABLED",
	}
}

// DefaultGoogleBillingURL is Google Ads' billing summary. Google documents no parameter that
// selects an account by customer id, so the owner picks the account after signing in.
const DefaultGoogleBillingURL = "https://ads.google.com/aw/billing/summary"

// DefaultTikTokLinks are TikTok Ads Manager's pages. Only the ad group list with ?aadvid= is in
// TikTok's help (article 10007023, read 2026-11-24); the campaign and payment paths follow the
// same pattern and are config so an admin can correct them without a release.
func DefaultTikTokLinks() map[string]string {
	return map[string]string{
		"home":     "https://ads.tiktok.com/i18n/home?aadvid={advertiser_id}",
		"campaign": "https://ads.tiktok.com/i18n/perf/campaign?aadvid={advertiser_id}",
		"payment":  "https://ads.tiktok.com/i18n/account/payment?aadvid={advertiser_id}",
		"signup":   "https://ads.tiktok.com/",
	}
}

// Platform reads the platform defaults from service_config over Fallback.
func Platform(ctx context.Context, db *ent.Client) Settings {
	s := Fallback()
	val := func(k string) string { return svcconfig.Value(ctx, db, uuid.Nil, k) }
	if p, ok := parsePlatforms(strings.Split(val(KeyPlatforms), ",")); ok {
		s.Platforms = p
	}
	if t := strings.ToLower(val(KeyTier)); validTier(t) {
		s.Tier = t
	}
	if d, err := strconv.Atoi(val(KeyDays)); err == nil && d >= MinDays && d <= MaxDays {
		s.Days = d
	}
	if c, err := strconv.ParseInt(val(KeyTotalCapKES), 10, 64); err == nil && c >= 0 && c <= maxAmount {
		s.TotalCapKES = c
	}
	if r, ok := ParseRuleOfThumb(val(KeyRuleOfThumbKES)); ok {
		s.RuleOfThumbKES = r
	}
	if m, err := strconv.ParseInt(val(KeyTikTokMinDailyUSD), 10, 64); err == nil && m > 0 {
		s.TikTokMinUSD = m
	}
	if m := strings.ToLower(val(KeyTikTokMode)); m == "guided" || m == "api" {
		s.TikTokMode = m
	}
	if v := val(KeyTikTokLinks); v != "" {
		var links map[string]string
		if json.Unmarshal([]byte(v), &links) == nil {
			for k, u := range links {
				if strings.HasPrefix(u, "https://") {
					s.TikTokLinks[k] = u
				}
			}
		}
	}
	if v := val(KeyGoogleBillingURL); strings.HasPrefix(v, "https://") {
		s.GoogleBilling = v
	}
	if v := strings.ToUpper(val(KeyGoogleStatus)); v == "ENABLED" || v == "PAUSED" {
		s.GoogleStatus = v
	}
	s.GoogleTotal = strings.EqualFold(val(KeyGoogleBudgetPeriod), "total")
	return s
}

// ParseRuleOfThumb reads the rule of thumb JSON; every platform needs all three tiers, ascending.
func ParseRuleOfThumb(v string) (map[string]Tiers, bool) {
	if strings.TrimSpace(v) == "" {
		return nil, false
	}
	var raw map[string]map[string]int64
	if json.Unmarshal([]byte(v), &raw) != nil {
		return nil, false
	}
	out := map[string]Tiers{}
	for p, t := range raw {
		if !validPlatform(p) {
			continue
		}
		lo, rec, hi := t[TierLow], t[TierRecommended], t[TierHigh]
		if lo <= 0 || rec < lo || hi < rec || hi > maxAmount {
			continue
		}
		out[p] = Tiers{TierLow: lo, TierRecommended: rec, TierHigh: hi}
	}
	return out, len(out) > 0
}

// Merge applies a tenant's metadata overrides to the platform settings. Invalid tenant values are
// ignored (the profile PATCH refuses them, so only a hand-edited row can hold one).
func Merge(platform Settings, tenantMeta map[string]any) Settings {
	s := platform
	s.Platforms = append([]string(nil), platform.Platforms...)
	s.DailyBudget = map[string]int64{}
	o, _ := tenantMeta[TenantKey].(map[string]any)
	if o == nil {
		return s
	}
	if list, ok := o["platforms"].([]any); ok {
		names := make([]string, 0, len(list))
		for _, v := range list {
			if str, ok := v.(string); ok {
				names = append(names, str)
			}
		}
		if p, ok := parsePlatforms(names); ok || len(list) == 0 {
			s.Platforms = p
		}
	}
	if t, ok := o["tier"].(string); ok && validTier(t) {
		s.Tier = t
	}
	if m, ok := o["daily_budget"].(map[string]any); ok {
		for p, v := range m {
			if n, ok := wholeNumber(v); ok && validPlatform(p) && n > 0 && n <= maxAmount {
				s.DailyBudget[p] = n
			}
		}
	}
	if d, ok := wholeNumber(o["days"]); ok && d >= MinDays && d <= MaxDays {
		s.Days = int(d)
	}
	if c, ok := wholeNumber(o["total_cap"]); ok && c >= 0 && c <= maxAmount {
		s.TotalCap, s.TotalCapSet = c, true
	}
	return s
}

// Load is one tenant's effective settings and the platform defaults they were built on.
func Load(ctx context.Context, db *ent.Client, t *ent.Tenant) (effective, platform Settings) {
	platform = Platform(ctx, db)
	var meta map[string]any
	if t != nil {
		meta = t.Metadata
	}
	return Merge(platform, meta), platform
}

// On reports whether platform p is switched on.
func (s Settings) On(p string) bool {
	for _, x := range s.Platforms {
		if x == p {
			return true
		}
	}
	return false
}

// Validate checks and normalises metadata.run_ads in a profile update. A null value clears the
// override. Unknown keys are refused so a typo is not silently ignored.
func Validate(meta map[string]any) error {
	v, ok := meta[TenantKey]
	if !ok || v == nil {
		return nil
	}
	o, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("run_ads must be an object")
	}
	out := map[string]any{}
	for k, val := range o {
		switch k {
		case "platforms":
			list, ok := val.([]any)
			if !ok {
				return fmt.Errorf("run_ads.platforms must be a list of meta, google, tiktok")
			}
			names := make([]string, 0, len(list))
			for _, x := range list {
				str, ok := x.(string)
				if !ok || !validPlatform(strings.ToLower(strings.TrimSpace(str))) {
					return fmt.Errorf("run_ads.platforms must be a list of meta, google, tiktok")
				}
				names = append(names, str)
			}
			p, _ := parsePlatforms(names)
			out[k] = p
		case "tier":
			str, _ := val.(string)
			if !validTier(str) {
				return fmt.Errorf("run_ads.tier must be low, recommended or high")
			}
			out[k] = str
		case "daily_budget":
			m, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("run_ads.daily_budget must map a platform to a whole amount")
			}
			clean := map[string]any{}
			for p, x := range m {
				if x == nil {
					continue
				}
				n, ok := wholeNumber(x)
				if !validPlatform(p) || !ok || n < 0 || n > maxAmount {
					return fmt.Errorf("run_ads.daily_budget must map meta, google or tiktok to a whole amount")
				}
				if n > 0 {
					clean[p] = n
				}
			}
			out[k] = clean
		case "days":
			n, ok := wholeNumber(val)
			if !ok || n < MinDays || n > MaxDays {
				return fmt.Errorf("run_ads.days must be %d to %d", MinDays, MaxDays)
			}
			out[k] = n
		case "total_cap":
			n, ok := wholeNumber(val)
			if !ok || n < 0 || n > maxAmount {
				return fmt.Errorf("run_ads.total_cap must be a whole amount, 0 for none")
			}
			out[k] = n
		default:
			return fmt.Errorf("run_ads.%s is not a setting", k)
		}
	}
	meta[TenantKey] = out
	return nil
}

// Changed reports whether v (already validated) differs from the stored override.
func Changed(stored map[string]any, v any) bool {
	a, _ := json.Marshal(stored[TenantKey])
	b, _ := json.Marshal(v)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return !reflect.DeepEqual(x, y)
}

// View is the JSON form of the settings the Settings page shows and edits.
type View struct {
	Platforms   []string         `json:"platforms"`
	Tier        string           `json:"tier"`
	DailyBudget map[string]int64 `json:"daily_budget"`
	Days        int              `json:"days"`
	TotalCap    int64            `json:"total_cap"`
	TotalCapKES int64            `json:"total_cap_kes,omitempty"`
	// RuleOfThumbKES is the platform's rule of thumb, shown read only.
	RuleOfThumbKES map[string]Tiers `json:"rule_of_thumb_kes"`
	TikTokMode     string           `json:"tiktok_mode"`
}

// ToView renders the settings for the API.
func (s Settings) ToView() View {
	v := View{Platforms: append([]string{}, s.Platforms...), Tier: s.Tier, DailyBudget: map[string]int64{}, Days: s.Days,
		TotalCap: s.TotalCap, TotalCapKES: s.TotalCapKES, RuleOfThumbKES: map[string]Tiers{}, TikTokMode: s.TikTokMode}
	for k, n := range s.DailyBudget {
		v.DailyBudget[k] = n
	}
	for k, t := range s.RuleOfThumbKES {
		v.RuleOfThumbKES[k] = t
	}
	return v
}

func validPlatform(p string) bool { return p == Meta || p == Google || p == TikTok }

func validTier(t string) bool { return t == TierLow || t == TierRecommended || t == TierHigh }

// parsePlatforms keeps known names once, in plan order; ok is false when none is known.
func parsePlatforms(names []string) ([]string, bool) {
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if validPlatform(n) {
			seen[n] = true
		}
	}
	out := make([]string, 0, len(seen))
	for _, p := range AllPlatforms {
		if seen[p] {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return order(out[i]) < order(out[j]) })
	return out, len(out) > 0
}

func order(p string) int {
	for i, x := range AllPlatforms {
		if x == p {
			return i
		}
	}
	return len(AllPlatforms)
}

// wholeNumber reads a JSON number (float64), an int or int64 that is a whole number.
func wholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int64(n)) {
			return 0, false
		}
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
