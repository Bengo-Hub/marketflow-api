// Package enrichment builds a contact's profile from what the business already holds about the
// person: the lead they came in as, their timeline, their purchases and payments, their deals,
// the company account they are linked to and, when the owner allows it, that company's own public
// home page. It never searches the web for a person, never calls a paid API and never uses AI, so
// it costs the tenant no credits. The profile lives in Contact.metadata.enrichment.
package enrichment

import (
	"net/url"
	"strings"
	"time"
)

// MetadataKey is the Contact.metadata key that holds the profile.
const MetadataKey = "enrichment"

// Version is bumped when the profile's shape changes, so readers can tell old profiles apart.
const Version = 1

// Sources a profile can be built from.
const (
	SourceCRM            = "crm"             // lead, timeline, deals and purchases in MarketFlow
	SourceEmailDomain    = "email_domain"    // the domain of the email the business holds
	SourceAccount        = "account"         // the company account the business linked or entered
	SourceCompanyWebsite = "company_website" // that account's public home page (owner's switch)
)

// Segments a profile can land in, in the order segmentFor checks them.
const (
	SegmentActiveCustomer = "active_customer" // bought or paid in the last ActiveWindow
	SegmentLapsedCustomer = "lapsed_customer" // bought before, not in the last ActiveWindow
	SegmentInPipeline     = "in_pipeline"     // has an open deal, never bought
	SegmentEngaged        = "engaged"         // timeline entries in the last 90 days
	SegmentQuiet          = "quiet"           // none of the above
)

// Segments lists every segment, for validation (segment rules).
var Segments = []string{SegmentActiveCustomer, SegmentLapsedCustomer, SegmentInPipeline, SegmentEngaged, SegmentQuiet}

// ActiveWindow is how recent a purchase must be for an active customer.
const ActiveWindow = 90 * 24 * time.Hour

// Profile is the enrichment stored on a contact.
type Profile struct {
	Version     int       `json:"version"`
	RefreshedAt string    `json:"refreshed_at"` // RFC 3339, UTC, whole seconds (sorts as text)
	Sources     []string  `json:"sources"`
	Segment     string    `json:"segment"`
	Origin      *Origin   `json:"origin,omitempty"`
	Engagement  Engaged   `json:"engagement"`
	Purchases   Purchases `json:"purchases"`
	Deals       Deals     `json:"deals"`
	Email       *Email    `json:"email,omitempty"`
	Company     *Company  `json:"company,omitempty"`
}

// Origin is how the person first reached the business, from their lead.
type Origin struct {
	LeadSource  string `json:"lead_source,omitempty"`
	UTMSource   string `json:"utm_source,omitempty"`
	UTMMedium   string `json:"utm_medium,omitempty"`
	UTMCampaign string `json:"utm_campaign,omitempty"`
	Interest    string `json:"interest,omitempty"`
	FirstSeenAt string `json:"first_seen_at,omitempty"`
}

// Engaged counts the contact's timeline. Entries the agents write (suggestions and anything with
// source "agent") do not count.
type Engaged struct {
	Activities90d    int    `json:"activities_90d"`
	LastActivityAt   string `json:"last_activity_at,omitempty"`
	LastActivityType string `json:"last_activity_type,omitempty"`
}

// Purchases counts orders and payments from POS, online ordering and Books.
type Purchases struct {
	Orders         int    `json:"orders"`
	Payments       int    `json:"payments"`
	LastPurchaseAt string `json:"last_purchase_at,omitempty"`
}

// Deals sums the contact's deals.
type Deals struct {
	Open      int     `json:"open"`
	Won       int     `json:"won"`
	OpenValue float64 `json:"open_value"`
	WonValue  float64 `json:"won_value"`
	Currency  string  `json:"currency,omitempty"`
}

// Email says whether the address is on a free mail service or on an organisation's own domain.
// The domain is kept only for an organisation's domain.
type Email struct {
	Kind   string `json:"kind"` // "organisation" or "free"
	Domain string `json:"domain,omitempty"`
}

// Company is the account the contact belongs to, or the one account whose website matches the
// contact's email domain (a suggestion; the contact is never linked automatically).
type Company struct {
	AccountID          string `json:"account_id,omitempty"`
	SuggestedAccountID string `json:"suggested_account_id,omitempty"`
	Name               string `json:"name,omitempty"`
	Industry           string `json:"industry,omitempty"`
	Website            string `json:"website,omitempty"`
	Summary            string `json:"summary,omitempty"`
	SummaryReadAt      string `json:"summary_read_at,omitempty"`
}

// freeMail are mail services anyone can sign up to; an address there says nothing about an
// organisation.
var freeMail = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "yahoo.co.uk": true, "ymail.com": true,
	"rocketmail.com": true, "hotmail.com": true, "hotmail.co.uk": true, "outlook.com": true, "live.com": true,
	"msn.com": true, "icloud.com": true, "me.com": true, "mac.com": true, "aol.com": true, "proton.me": true,
	"protonmail.com": true, "gmx.com": true, "gmx.net": true, "mail.com": true, "zoho.com": true,
	"zohomail.com": true, "yandex.com": true, "yandex.ru": true, "tutanota.com": true, "fastmail.com": true,
	"hey.com": true, "qq.com": true, "163.com": true, "rediffmail.com": true,
}

// classifyEmail returns the email's kind and, for an organisation's domain, the domain. An
// address without a usable domain gives nil.
func classifyEmail(email string) *Email {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return nil
	}
	domain := strings.Trim(strings.ToLower(strings.TrimSpace(email[at+1:])), ".")
	if domain == "" || !strings.Contains(domain, ".") {
		return nil
	}
	if freeMail[domain] {
		return &Email{Kind: "free"}
	}
	return &Email{Kind: "organisation", Domain: domain}
}

// hostOf is a website's host in lower case without "www.", or "" when it is not a link.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

// segmentFor places a profile in one segment, first match wins.
func segmentFor(p *Profile, now time.Time) string {
	if p.Purchases.Orders > 0 || p.Purchases.Payments > 0 {
		if t, err := time.Parse(time.RFC3339, p.Purchases.LastPurchaseAt); err == nil && now.Sub(t) <= ActiveWindow {
			return SegmentActiveCustomer
		}
		return SegmentLapsedCustomer
	}
	if p.Deals.Open > 0 {
		return SegmentInPipeline
	}
	if p.Engagement.Activities90d > 0 {
		return SegmentEngaged
	}
	return SegmentQuiet
}

// keepSummary carries the earlier site summary into a fresh company block when this read failed
// and the website is the same, so a site that is briefly down does not wipe what was read.
func keepSummary(fresh *Company, prev *Profile) {
	if fresh == nil || fresh.Summary != "" || prev == nil || prev.Company == nil {
		return
	}
	if prev.Company.Summary != "" && hostOf(prev.Company.Website) == hostOf(fresh.Website) {
		fresh.Summary, fresh.SummaryReadAt = prev.Company.Summary, prev.Company.SummaryReadAt
	}
}

// stamp formats a time the way the profile stores it.
func stamp(t time.Time) string { return t.UTC().Truncate(time.Second).Format(time.RFC3339) }

func addSource(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}
