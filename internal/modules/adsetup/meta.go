package adsetup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// metaPage is a connected Facebook Page and its token.
type metaPage struct{ ID, Name, Token string }

// metaInput is everything a Meta checklist needs.
type metaInput struct {
	Client *meta.Client
	Token  string
	ActID  string
	Scopes []string // granted permissions; empty when not recorded
	Pages  []metaPage
	// SendingTo is the pixel MarketFlow already sends conversions to ("" when none).
	SendingTo string
	// StartSending stores the pixel MarketFlow sends conversions to.
	StartSending func(ctx context.Context, pixelID string) error
	Website      string
	Business     string
	Now          time.Time
	Cfg          Config
}

// may reports whether the person granted a permission; unknown scopes are left to Meta to judge.
func (in metaInput) may(scope string) bool {
	if len(in.Scopes) == 0 {
		return true
	}
	for _, s := range in.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

// guard drops a fix MarketFlow has no permission for and says so.
func (in metaInput) guard(it Item, scope string) Item {
	if it.Fix != nil && !in.may(scope) {
		it.Fix = nil
		it.Detail += " MarketFlow cannot do it for you: Meta has not granted it the permission " + scope + " on this connection."
	}
	return it
}

func metaChecklist(ctx context.Context, in metaInput) []Item {
	var (
		acc                                        *meta.AdAccount
		pixels                                     []meta.Pixel
		promote                                    []string
		bizID                                      string
		domains                                    []meta.OwnedDomain
		ads                                        []meta.ProblemAd
		quality                                    []meta.MatchQuality
		eAcc, ePix, ePromote, eDom, eAds, eQuality error
	)
	c, cfg := in.Client, in.Cfg
	together(
		func() { acc, eAcc = c.AdAccountHealth(ctx, in.Token, in.ActID) },
		func() { pixels, ePix = c.Pixels(ctx, in.Token, in.ActID) },
		func() { promote, ePromote = c.PromotePages(ctx, in.Token, in.ActID) },
		func() { bizID, domains, eDom = c.BusinessDomains(ctx, in.Token, in.ActID) },
		func() { ads, eAds = c.ProblemAds(ctx, in.Token, in.ActID) },
		func() {
			if in.SendingTo != "" {
				quality, eQuality = c.DatasetQuality(ctx, in.Token, in.SendingTo)
			}
		},
	)
	var items []Item
	if eAcc != nil {
		items = append(items, unread("meta.account", "Ad account active", "Meta", eAcc, cfg.link("meta_quality", "Open account quality", in.ActID)))
	} else {
		items = append(items, in.accountItem(acc), in.fundingItem(acc), in.capItem(acc))
	}
	items = append(items, in.pixelItem(pixels, ePix), in.capiItem(pixels, ePix, quality, eQuality))
	items = append(items, in.pageLinkItem(promote, ePromote))
	pageItems := make([][]Item, len(in.Pages))
	reads := make([]func(), len(in.Pages))
	for i, p := range in.Pages {
		reads[i] = func() { pageItems[i] = in.pageItems(ctx, p) }
	}
	together(reads...)
	for _, p := range pageItems {
		items = append(items, p...)
	}
	items = append(items, in.domainItem(bizID, domains, eDom), in.policyItem(ads, eAds))
	return seal(items)
}

// metaAccountStatus explains account_status (Marketing API, Ad Account reference).
var metaAccountStatus = map[int]string{
	2:   "Meta disabled this ad account",
	3:   "this ad account has an unpaid balance",
	7:   "Meta is reviewing this ad account",
	8:   "a payment on this ad account is still pending",
	9:   "this ad account has a short time left to pay",
	100: "this ad account is closing",
	101: "this ad account is closed",
}

func (in metaInput) accountItem(a *meta.AdAccount) Item {
	it := Item{Key: "meta.account", Title: "Ad account active"}
	if a.AccountStatus == meta.AccountActive {
		it.Status, it.Detail = StatusOK, "Meta has this ad account active."
		return it
	}
	why := metaAccountStatus[a.AccountStatus]
	if why == "" {
		why = "Meta gives this ad account status " + strconv.Itoa(a.AccountStatus)
	}
	it.Status = StatusAction
	it.Link = in.Cfg.link("meta_quality", "Open account quality", in.ActID)
	if a.AccountStatus == 3 || a.AccountStatus == 8 || a.AccountStatus == 9 {
		it.Link = in.Cfg.link("meta_billing", "Open Meta billing", in.ActID)
	}
	it.Detail = "Ads may not run: " + why + ". Open the page to see what Meta asks for."
	return it
}

// amount formats a Meta minor unit amount; every currency MarketFlow sells in has two decimals on
// Meta except the zero decimal ones.
func amount(minor, currency string) string {
	n, err := strconv.ParseFloat(minor, 64)
	if err != nil {
		return ""
	}
	switch currency {
	case "UGX", "RWF", "BIF", "JPY", "KRW", "CLP", "XAF", "XOF", "VND", "PYG":
		return fmt.Sprintf("%s %.0f", currency, n)
	}
	return fmt.Sprintf("%s %.2f", currency, n/100)
}

func (in metaInput) fundingItem(a *meta.AdAccount) Item {
	it := Item{Key: "meta.funding", Title: "Payment method"}
	l := in.Cfg.link("meta_billing", "Open Meta billing", in.ActID)
	switch {
	case a.FundingSource == "" && a.FundingDetails == nil:
		it.Status, it.Link = StatusAction, l
		it.Detail = "Meta shows no payment method on this ad account. Add one before you run ads. If you already have, ask a person with full control of the ad account to connect it."
	case a.IsPrepayAccount:
		it.Status, it.Link = StatusInfo, l
		it.Detail = "You pay before ads run. Check that the balance covers your next ad on Meta's billing page."
	default:
		it.Status, it.Detail = StatusOK, "A payment method is set up. Meta charges it as your ads run."
	}
	return it
}

func (in metaInput) capItem(a *meta.AdAccount) Item {
	it := Item{Key: "meta.spend_cap", Title: "Account spending limit"}
	l := in.Cfg.link("meta_billing", "Open Meta billing", in.ActID)
	limit, _ := strconv.ParseFloat(a.SpendCap, 64)
	spent, _ := strconv.ParseFloat(a.AmountSpent, 64)
	switch {
	case limit <= 0:
		it.Status, it.Link = StatusInfo, l
		it.Detail = "No limit is set. A limit is a safety net: Meta stops every ad once the account has spent that much. You set it on Meta's billing page."
	case spent >= limit:
		it.Status, it.Link = StatusAction, l
		it.Detail = "The account has reached its limit of " + amount(a.SpendCap, a.Currency) + ", so Meta has stopped your ads. Raise or reset the limit on Meta's billing page."
	default:
		left := strconv.FormatFloat(limit-spent, 'f', 0, 64)
		it.Status, it.Detail = StatusOK, "The limit is "+amount(a.SpendCap, a.Currency)+", with "+amount(left, a.Currency)+" left."
	}
	return it
}

func (in metaInput) pixelItem(pixels []meta.Pixel, err error) Item {
	it := Item{Key: "meta.pixel", Title: "Pixel (dataset)"}
	l := in.Cfg.link("meta_events", "Open Events Manager", in.ActID)
	if err != nil {
		return unread(it.Key, it.Title, "Meta", err, l)
	}
	if len(pixels) == 0 {
		it.Status, it.Detail = StatusAction, "This ad account has no pixel, so Meta cannot learn what people do after they click."
		it.Fix = &Fix{Label: "Create the pixel", Changes: []string{
			"Create one pixel on this ad account. A pixel cannot be deleted later.",
			"No ad is created. The pixel collects nothing until it is added to your website or MarketFlow sends leads to it.",
		}, Fields: []Field{{Key: "name", Label: "Pixel name", Value: clip(strings.TrimSpace(in.Business+" pixel"), 60), Max: 60, Required: true}},
			run: func(ctx context.Context, v map[string]string) error {
				_, err := in.Client.CreatePixel(ctx, in.Token, in.ActID, v["name"])
				return err
			}}
		return in.guard(it, "ads_management")
	}
	for _, p := range pixels {
		if p.Unavailable {
			continue
		}
		if t, ok := metaTime(p.LastFired); ok && in.Now.Sub(t) <= in.Cfg.PixelQuiet {
			it.Status, it.Detail = StatusOK, "The pixel \""+p.Name+"\" is receiving events."
			return it
		}
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = "You have a pixel, but it has received nothing lately. Add it to your website (Events Manager shows how), or let MarketFlow send your leads to it."
	return it
}

// metaTime reads Meta's "2026-11-20T10:00:00+0000".
func metaTime(s string) (time.Time, bool) {
	for _, f := range []string{"2006-01-02T15:04:05-0700", time.RFC3339} {
		if t, err := time.Parse(f, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func (in metaInput) capiItem(pixels []meta.Pixel, ePix error, quality []meta.MatchQuality, eQuality error) Item {
	it := Item{Key: "meta.conversions_api", Title: "MarketFlow sends your leads to Meta"}
	l := in.Cfg.link("meta_events", "Open Events Manager", in.ActID)
	if in.SendingTo == "" {
		it.Status = StatusWarning
		if ePix != nil {
			return unread(it.Key, it.Title, "Meta", ePix, l)
		}
		if len(pixels) == 0 {
			it.Detail = "First create the pixel (the check above). Then MarketFlow can send your leads to it."
			return it
		}
		p := pixels[0]
		it.Detail = "Meta does not hear about the leads MarketFlow records, so it cannot look for more people like them."
		it.Fix = &Fix{Label: "Send leads to Meta", Changes: []string{
			"Send MarketFlow's leads, bookings and sales to the pixel \"" + p.Name + "\" (" + p.ID + ").",
			"Meta receives each person's email and phone as a one-way code (SHA-256) and, when there is one, the ad click id. Your privacy policy must say that you share this with Meta.",
			"Only MarketFlow's own setting changes. No ad is created.",
		}, run: func(ctx context.Context, _ map[string]string) error { return in.StartSending(ctx, p.ID) }}
		return in.guard(it, "ads_management")
	}
	if eQuality != nil {
		u := unread(it.Key, it.Title, "Meta", eQuality, l)
		u.Detail = "MarketFlow sends leads to pixel " + in.SendingTo + ". How well Meta matches them to people could not be read: " + u.Detail
		return u
	}
	it.Status, it.Detail = StatusOK, "MarketFlow sends leads to pixel "+in.SendingTo+"."
	if len(quality) == 0 {
		it.Detail += " Meta has no match score yet; it needs some events first."
	}
	for _, q := range quality {
		it.Points = append(it.Points, fmt.Sprintf("%s: match score %.1f out of 10", q.Event, q.Score))
		if q.Score < in.Cfg.MatchMin {
			it.Status, it.Link = StatusWarning, l
			it.Detail = fmt.Sprintf("Meta can match fewer people than it should (aim for %.0f or more). Ask for both an email and a phone number on your forms.", in.Cfg.MatchMin)
		}
	}
	return it
}

func (in metaInput) pageLinkItem(promote []string, err error) Item {
	it := Item{Key: "meta.page", Title: "Facebook Page linked"}
	l := in.Cfg.link("meta_pages", "Open Page settings", in.ActID)
	if len(in.Pages) == 0 {
		it.Status, it.Detail = StatusAction, "No Facebook Page is connected to MarketFlow. Ads need one: connect Meta again in Settings, Integrations and tick your Page."
		return it
	}
	if err != nil {
		return unread(it.Key, it.Title, "Meta", err, l)
	}
	can := map[string]bool{}
	for _, id := range promote {
		can[id] = true
	}
	for _, p := range in.Pages {
		if can[p.ID] {
			it.Status, it.Detail = StatusOK, "This ad account can advertise your Page \""+p.Name+"\"."
			return it
		}
	}
	it.Status, it.Link = StatusAction, l
	it.Detail = "This ad account cannot advertise your connected Page. In Meta Business settings give the same person access to both the Page and the ad account."
	return it
}

func (in metaInput) pageItems(ctx context.Context, p metaPage) []Item {
	var (
		forms                    []meta.LeadForm
		terms                    bool
		ig, wa                   string
		hasWA                    *bool
		eForms, eTerms, eIG, eWA error
	)
	c, cfg := in.Client, in.Cfg
	if p.Token == "" {
		return []Item{unread("meta.lead_form:"+p.ID, "Lead form on "+p.Name, "Meta", errors.New("the Page needs connecting again"), nil)}
	}
	together(
		func() { forms, eForms = c.LeadForms(ctx, p.Token, p.ID) },
		func() { terms, eTerms = c.LeadTerms(ctx, p.Token, p.ID) },
		func() { ig, eIG = c.PageInstagram(ctx, p.Token, p.ID) },
		func() { wa, hasWA, eWA = c.PageWhatsApp(ctx, p.Token, p.ID) },
	)
	out := []Item{in.formItem(p, forms, terms, eForms, eTerms)}

	it := Item{Key: "meta.instagram:" + p.ID, Title: "Instagram linked to " + p.Name, Status: StatusOK, Detail: "An Instagram professional account is linked, so ads can run as it."}
	l := cfg.link("meta_instagram", "Link Instagram", p.ID)
	if eIG != nil {
		it = unread(it.Key, it.Title, "Meta", eIG, l)
	} else if ig == "" {
		it.Status, it.Link, it.Detail = StatusWarning, l, "No Instagram account is linked to this Page. Link a professional account so your ads can run on Instagram as you."
	}
	out = append(out, it)

	it = Item{Key: "meta.whatsapp:" + p.ID, Title: "WhatsApp number on " + p.Name, Status: StatusOK}
	l = cfg.link("meta_whatsapp", "Link WhatsApp", p.ID)
	switch {
	case eWA != nil:
		it = unread(it.Key, it.Title, "Meta", eWA, l)
	case hasWA == nil:
		it = unread(it.Key, it.Title, "Meta", errors.New("the Page did not share its WhatsApp details"), l)
	case !*hasWA:
		it.Status, it.Link, it.Detail = StatusWarning, l, "No WhatsApp number is linked to this Page, so ads that open a WhatsApp chat cannot run."
	default:
		it.Detail = strings.TrimSpace("Ads can open a WhatsApp chat with you. " + wa)
	}
	return append(out, it)
}

func (in metaInput) formItem(p metaPage, forms []meta.LeadForm, terms bool, eForms, eTerms error) Item {
	it := Item{Key: "meta.lead_form:" + p.ID, Title: "Lead form on " + p.Name}
	l := in.Cfg.link("meta_forms", "Open lead forms", p.ID)
	if eForms != nil {
		return unread(it.Key, it.Title, "Meta", eForms, l)
	}
	if eTerms == nil && !terms {
		it.Status, it.Link = StatusAction, in.Cfg.link("meta_terms", "Accept the terms", p.ID)
		it.Detail = "Meta's lead ads terms are not accepted for this Page. Only you can accept them; it takes one tap on Meta's page."
		return it
	}
	for _, f := range forms {
		if f.Status == "ACTIVE" {
			it.Status, it.Detail = StatusOK, "The form \""+f.Name+"\" is ready for lead ads."
			return it
		}
	}
	it.Status, it.Detail = StatusWarning, "This Page has no lead form. With one, people leave their details without leaving Facebook."
	qs := make([]string, 0, len(in.Cfg.FormQuestions))
	for _, q := range in.Cfg.FormQuestions {
		qs = append(qs, strings.ToLower(words(q)))
	}
	it.Fix = &Fix{Label: "Create a form", Changes: []string{
		"Create a lead form on your Page \"" + p.Name + "\" that asks for: " + strings.Join(qs, ", ") + ".",
		"It also asks, without a default answer, whether you may contact them on WhatsApp.",
		"No ad is created. Meta does not let a published form be edited.",
	}, Fields: []Field{
		{Key: "name", Label: "Form name", Value: clip(in.Business+" leads", 100), Max: 100, Required: true},
		{Key: "privacy_url", Label: "Your privacy policy link", Max: 500, Required: true},
	}, run: func(ctx context.Context, v map[string]string) error {
		if !httpsURL(v["privacy_url"]) {
			return badInput("the privacy policy link starts with https://")
		}
		_, err := in.Client.CreateLeadForm(ctx, p.Token, p.ID, meta.LeadFormInput{Name: v["name"], PrivacyURL: v["privacy_url"],
			Questions: in.Cfg.FormQuestions, AskWhatsAppConsent: true})
		return err
	}}
	return in.guard(it, "pages_manage_ads")
}

func (in metaInput) domainItem(bizID string, domains []meta.OwnedDomain, err error) Item {
	it := Item{Key: "meta.domain", Title: "Website domain verified"}
	l := in.Cfg.link("meta_domains", "Open domains", in.ActID)
	host := hostOf(in.Website)
	switch {
	case host == "":
		it.Status, it.Detail = StatusInfo, "Your profile has no website, so there is no domain to verify."
		return it
	case err != nil:
		return unread(it.Key, it.Title, "Meta", err, l)
	case bizID == "":
		it.Status, it.Link, it.Detail = StatusInfo, l, "This ad account is not in a Meta business portfolio, and domains are verified there."
		return it
	}
	for _, d := range domains {
		name := strings.ToLower(d.Name)
		if name == "" || (host != name && !strings.HasSuffix(host, "."+name)) {
			continue
		}
		if d.Status == "" || strings.EqualFold(d.Status, "verified") {
			it.Status, it.Detail = StatusOK, d.Name+" is in your Meta business portfolio."
			if d.Status == "" {
				it.Detail += " Meta does not tell MarketFlow whether it is verified; a green dot on the domains page means it is."
				it.Link = l
			}
			return it
		}
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = host + " is not a verified domain in your Meta business portfolio. Add it there and place the code Meta gives you on your website; MarketFlow cannot do this for you."
	return it
}

func hostOf(site string) string {
	if site == "" {
		return ""
	}
	if !strings.Contains(site, "://") {
		site = "https://" + site
	}
	u, err := url.Parse(site)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
}

func (in metaInput) policyItem(ads []meta.ProblemAd, err error) Item {
	it := Item{Key: "meta.policy", Title: "Ads within Meta's policies"}
	l := in.Cfg.link("meta_ads", "Open Ads Manager", in.ActID)
	if err != nil {
		return unread(it.Key, it.Title, "Meta", err, l)
	}
	if len(ads) == 0 {
		it.Status, it.Detail = StatusOK, "Meta reports no rejected ad."
		return it
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = "Meta rejected or flagged these ads. Open one to edit it or ask for a review."
	for _, a := range ads {
		line := a.Name + ": " + strings.ToLower(words(a.Status))
		if a.Status == "DISAPPROVED" {
			it.Status = StatusAction
		}
		if len(a.Issues) > 0 && a.Issues[0].Summary != "" {
			line += " (" + clip(a.Issues[0].Summary, 160) + ")"
		}
		it.Points = append(it.Points, line)
	}
	return it
}
