package adsetup

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Keys in service_config. A tenant row overrides the platform row; cmd/seed writes the platform
// rows when missing.
const (
	KeyActionName     = "ads_setup.google_action_name"
	KeyActionCategory = "ads_setup.google_action_category"
	KeyActionValue    = "ads_setup.google_action_value"
	KeyActionWindow   = "ads_setup.google_action_window_days"
	KeyMinSitelinks   = "ads_setup.google_min_sitelinks"
	KeyMinCallouts    = "ads_setup.google_min_callouts"
	// KeyListMin: members a customer list needs before lifecycle goals can use it (Google Ads help
	// 14007601: 1,000 active members in at least one network, read 2026-11-24).
	KeyListMin = "ads_setup.google_customer_list_min"
	// KeyMatchMin: Meta's Event Match Quality worth aiming for (Meta advises 6 or more).
	KeyMatchMin = "ads_setup.meta_match_quality_min"
	// KeyPixelQuiet: a pixel with no event for this long counts as quiet.
	KeyPixelQuiet = "ads_setup.pixel_quiet"
	// KeyFormQuestions: Meta instant form prefill questions, comma separated.
	KeyFormQuestions = "ads_setup.meta_form_questions"
	// KeyCache: how long a checklist is kept before the accounts are read again.
	KeyCache = "ads_setup.cache"
	// KeyLinks: JSON overrides of DefaultLinks.
	KeyLinks = "ads_setup.links"
)

// Config is the effective configuration.
type Config struct {
	ActionName, ActionCategory string
	ActionValue                float64
	WindowDays                 int
	MinSitelinks, MinCallouts  int
	ListMin                    int64
	MatchMin                   float64
	PixelQuiet                 time.Duration
	FormQuestions              []string
	Cache                      time.Duration
	Links                      map[string]string
}

// DefaultLinks are the provider pages. {id} is the account id; providers do not document most
// deep links, so an admin can correct them without a release. Google documents no parameter that
// picks an account by customer id, so its pages open the account the person last used.
func DefaultLinks() map[string]string {
	return map[string]string{
		"google_account":   "https://ads.google.com/aw/overview",
		"google_managers":  "https://ads.google.com/aw/accountaccess/managers",
		"google_goals":     "https://ads.google.com/aw/conversions",
		"google_assets":    "https://ads.google.com/aw/assetreport/associations",
		"google_audiences": "https://ads.google.com/aw/audiences/management",
		"google_campaigns": "https://ads.google.com/aw/campaigns",
		"google_policy":    "https://ads.google.com/aw/policymanager",
		"google_settings":  "https://ads.google.com/aw/settings/account",
		"meta_billing":     "https://business.facebook.com/billing_hub/payment_settings/?asset_id={id}",
		"meta_quality":     "https://www.facebook.com/accountquality/{id}",
		"meta_events":      "https://business.facebook.com/events_manager2/overview?act={id}",
		"meta_pages":       "https://business.facebook.com/settings/pages",
		"meta_forms":       "https://business.facebook.com/latest/instant_forms",
		"meta_terms":       "https://www.facebook.com/ads/leadgen/tos",
		"meta_instagram":   "https://www.facebook.com/settings/?tab=linked_instagram",
		"meta_whatsapp":    "https://www.facebook.com/settings/?tab=linked_whatsapp",
		"meta_domains":     "https://business.facebook.com/settings/owned-domains",
		"meta_ads":         "https://adsmanager.facebook.com/adsmanager/manage/ads?act={id}",
		"tiktok_events":    "https://ads.tiktok.com/i18n/events_manager?aadvid={id}",
		"tiktok_forms":     "https://ads.tiktok.com/i18n/material/instantpage?aadvid={id}",
		"tiktok_bc":        "https://business.tiktok.com/",
	}
}

func defaults() Config {
	return Config{ActionName: "Qualified lead (MarketFlow)", ActionCategory: "QUALIFIED_LEAD", ActionValue: 1, WindowDays: 90,
		MinSitelinks: 4, MinCallouts: 2, ListMin: 1000, MatchMin: 6, PixelQuiet: 7 * 24 * time.Hour,
		FormQuestions: []string{"FULL_NAME", "PHONE", "EMAIL"}, Cache: 5 * time.Minute, Links: DefaultLinks()}
}

// LoadConfig reads the tenant's configuration over the platform's over the code fallbacks.
func LoadConfig(ctx context.Context, db *ent.Client, tenantID uuid.UUID) Config {
	return parseConfig(func(k string) string { return svcconfig.Value(ctx, db, tenantID, k) })
}

func parseConfig(val func(string) string) Config {
	c := defaults()
	if v := val(KeyActionName); v != "" {
		c.ActionName = clip(v, 100)
	}
	if v := strings.ToUpper(val(KeyActionCategory)); v != "" {
		c.ActionCategory = v
	}
	if f, err := strconv.ParseFloat(val(KeyActionValue), 64); err == nil && f >= 0 {
		c.ActionValue = f
	}
	number := func(key string, min, max int, set func(int)) {
		if n, err := strconv.Atoi(val(key)); err == nil && n >= min && n <= max {
			set(n)
		}
	}
	number(KeyActionWindow, 1, 90, func(n int) { c.WindowDays = n })
	number(KeyMinSitelinks, 0, 20, func(n int) { c.MinSitelinks = n })
	number(KeyMinCallouts, 0, 20, func(n int) { c.MinCallouts = n })
	number(KeyListMin, 1, 10_000_000, func(n int) { c.ListMin = int64(n) })
	if f, err := strconv.ParseFloat(val(KeyMatchMin), 64); err == nil && f >= 0 && f <= 10 {
		c.MatchMin = f
	}
	if d, ok := svcconfig.ParseDuration(val(KeyPixelQuiet), time.Hour, 90*24*time.Hour); ok {
		c.PixelQuiet = d
	}
	if d, ok := svcconfig.ParseDuration(val(KeyCache), 10*time.Second, time.Hour); ok {
		c.Cache = d
	}
	var qs []string
	for _, q := range strings.Split(val(KeyFormQuestions), ",") {
		if q = strings.ToUpper(strings.TrimSpace(q)); q != "" {
			qs = append(qs, q)
		}
	}
	if len(qs) > 0 {
		c.FormQuestions = qs
	}
	var links map[string]string
	if v := val(KeyLinks); v != "" && json.Unmarshal([]byte(v), &links) == nil {
		for k, u := range links {
			if strings.HasPrefix(u, "https://") {
				c.Links[k] = u
			}
		}
	}
	return c
}

// link fills a provider page with the account id.
func (c Config) link(key, label, id string) *Link {
	u := c.Links[key]
	if u == "" {
		return nil
	}
	return &Link{Label: label, URL: strings.ReplaceAll(u, "{id}", url.QueryEscape(strings.TrimPrefix(id, "act_")))}
}
