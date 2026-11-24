package adsetup

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/bengobox/marketflow-api/internal/integrations/google"
)

// Google Ads text limits (Google Ads help, sitelink and callout assets).
const (
	maxSitelinkText = 25
	maxCalloutText  = 25
	sitelinkRows    = 4
	calloutRows     = 4
)

// Profile is the business's own facts, the source of every proposed asset text.
type Profile struct {
	Name, Website, Phone, City, Country string
	Pages                               []Page
	Services                            []string
}

// Page is a page on the business's website.
type Page struct{ Title, URL string }

// googleInput is everything a Google Ads checklist needs.
type googleInput struct {
	Ads        google.Ads
	CustomerID string
	ManagerID  string // MarketFlow's manager account, digits, "" when not configured
	Mapping    map[string]string
	// SaveMapping stores the MarketFlow event to conversion action mapping.
	SaveMapping func(ctx context.Context, mapping map[string]string) error
	BillingURL  string
	Consented   func(ctx context.Context) (int, error)
	Profile     func(ctx context.Context) Profile
	Cfg         Config
}

func dashed(id string) string {
	if d := google.CustomerDigits(id); len(d) == 10 {
		return d[:3] + "-" + d[3:6] + "-" + d[6:]
	}
	return id
}

func googleChecklist(ctx context.Context, in googleInput) []Item {
	var (
		acc                                             google.SetupAccount
		spend                                           google.SpendStatus
		links                                           []google.ManagerLink
		actions                                         []google.SetupAction
		camps                                           []google.SetupCampaign
		accGoals, campGoals                             []google.SetupGoal
		own                                             map[string]bool
		assets                                          google.AssetCount
		lists                                           []google.UserList
		ads                                             []google.LimitedAd
		consented                                       int
		prof                                            Profile
		eAcc, eSpend, eLink, eAct, eCamp, eGoal, eAsset error
		eList, eAds, eCons                              error
	)
	a, id, cfg := in.Ads, in.CustomerID, in.Cfg
	acc, eAcc = a.SetupAccount(ctx, id)
	if eAcc != nil {
		return seal([]Item{unread("google.account", "Account enabled and billing set", "Google", eAcc, cfg.link("google_account", "Open Google Ads", id))})
	}
	together(
		func() { spend, eSpend = a.SpendStatus(ctx, id) },
		func() { links, eLink = a.ManagerLinks(ctx, id) },
		func() { actions, eAct = a.SetupActions(ctx, id) },
		func() { camps, eCamp = a.SetupCampaigns(ctx, id) },
		func() { accGoals, campGoals, own, eGoal = a.SetupGoals(ctx, id) },
		func() { assets, eAsset = a.AssetCounts(ctx, id) },
		func() { lists, eList = a.CustomerLists(ctx, id) },
		func() { ads, eAds = a.LimitedAds(ctx, id) },
		func() {
			if in.Consented != nil {
				consented, eCons = in.Consented(ctx)
			}
		},
		func() {
			if in.Profile != nil {
				prof = in.Profile(ctx)
			}
		},
	)
	if eCons != nil {
		consented = -1
	}
	items := []Item{
		in.accountItem(acc, spend, eSpend),
		in.trackingItem(actions, eAct, acc.Currency),
		in.mappingItem(actions, eAct),
		in.goalItem(actions, camps, accGoals, campGoals, own, firstErr(eAct, eCamp, eGoal)),
		in.taggingItem(acc),
		in.statusItem(camps, ads, firstErr(eCamp, eAds)),
		in.managerItem(links, eLink),
	}
	items = append(items, in.assetItems(assets, camps, prof, firstErr(eAsset, eCamp))...)
	items = append(items, in.listItem(lists, consented, eList))
	return seal(items)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

func (in googleInput) accountItem(acc google.SetupAccount, spend google.SpendStatus, err error) Item {
	it := Item{Key: "google.account", Title: "Account enabled and billing set"}
	billing := &Link{Label: "Open Google Ads billing", URL: in.BillingURL}
	switch {
	case acc.Status != "ENABLED":
		it.Status, it.Link = StatusAction, in.Cfg.link("google_account", "Open Google Ads", in.CustomerID)
		it.Detail = "Google has this account (" + dashed(in.CustomerID) + ") marked " + strings.ToLower(words(acc.Status)) + ", so no ad can run. Open Google Ads and follow the notice at the top of the page."
	case err != nil:
		return unread(it.Key, it.Title, "Google", err, billing)
	case spend.Billing == "APPROVED":
		it.Status, it.Detail = StatusOK, "The account is enabled and Google has approved its billing."
	case spend.Billing == "PENDING" || spend.Billing == "APPROVED_HELD":
		it.Status, it.Link, it.Detail = StatusWarning, billing, "The payment details are in, and Google is still checking them. Ads start once Google approves them."
	case spend.Billing == "":
		// Only monthly invoicing accounts are guaranteed a billing setup row, so an empty read is
		// not proof that no card is saved.
		it.Status, it.Link, it.Detail = StatusWarning, billing, "Google does not show a billing setup for this account. If you have not added a card or other payment method, add one: ads cannot run without it. In Google Ads pick account "+dashed(in.CustomerID)+" first."
	default:
		it.Status, it.Link, it.Detail = StatusAction, billing, "The billing setup is "+strings.ToLower(words(spend.Billing))+", so ads cannot run. Set up payment for account "+dashed(in.CustomerID)+"."
	}
	return it
}

func (in googleInput) managerItem(links []google.ManagerLink, err error) Item {
	it := Item{Key: "google.manager", Title: "Linked to MarketFlow's manager account"}
	l := in.Cfg.link("google_managers", "Open manager links", in.CustomerID)
	if in.ManagerID == "" {
		it.Status, it.Detail = StatusInfo, "MarketFlow has no manager account set, so there is nothing to link."
		return it
	}
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, l)
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = "This account is not linked to MarketFlow's manager account (" + dashed(in.ManagerID) + "). Your connection still works. Ask MarketFlow support to send the link request, then accept it in Google Ads under Admin, Access and security, Managers."
	for _, x := range links {
		if x.ManagerID != in.ManagerID {
			continue
		}
		switch x.Status {
		case "ACTIVE":
			it.Status, it.Link, it.Detail = StatusOK, nil, "Linked to MarketFlow's manager account "+dashed(in.ManagerID)+"."
			return it
		case "PENDING":
			it.Detail = "MarketFlow's link request (" + dashed(in.ManagerID) + ") is waiting for you. Accept it in Google Ads under Admin, Access and security, Managers."
		}
	}
	return it
}

// leadAction is the action the Lead event is mapped to, else the one MarketFlow would create.
func (in googleInput) leadAction(actions []google.SetupAction) (mapped, named *google.SetupAction) {
	for i := range actions {
		x := &actions[i]
		if x.Type != "UPLOAD_CLICKS" {
			continue
		}
		if rn := in.Mapping["Lead"]; rn != "" && x.ResourceName == rn {
			mapped = x
		}
		if strings.EqualFold(x.Name, in.Cfg.ActionName) {
			named = x
		}
	}
	return mapped, named
}

func (in googleInput) trackingItem(actions []google.SetupAction, err error, currency string) Item {
	it := Item{Key: "google.conversion_action", Title: "Google can count your leads"}
	l := in.Cfg.link("google_goals", "Open conversions", in.CustomerID)
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, l)
	}
	mapped, named := in.leadAction(actions)
	if x := firstAction(mapped, named); x != nil {
		it.Status, it.Detail = StatusOK, "Google Ads has the conversion \""+x.Name+"\" for leads from MarketFlow."
		if !x.Primary {
			it.Status, it.Link = StatusWarning, l
			it.Detail = "The conversion \"" + x.Name + "\" is there, but set to secondary, so Google counts it without using it to find more leads. Open it in Google Ads and set it to primary."
		}
		return it
	}
	it.Status = StatusAction
	it.Detail = "Google cannot tell which ad clicks became leads, so it cannot look for more people like them. MarketFlow can create the conversion for you."
	cfg := in.Cfg
	it.Fix = &Fix{Label: "Create it", Changes: []string{
		"Create the conversion \"" + cfg.ActionName + "\" in Google Ads, type offline click import, category " + words(cfg.ActionCategory) + ".",
		fmt.Sprintf("Count one per click, up to %d days after the click, at a value of %s %s when a lead has no value of its own.", cfg.WindowDays, currency, strconv.FormatFloat(cfg.ActionValue, 'f', -1, 64)),
		"No ad, budget or campaign is created.",
	}, run: func(ctx context.Context, _ map[string]string) error {
		_, err := in.Ads.CreateOfflineAction(ctx, in.CustomerID, google.NewAction{Name: cfg.ActionName, Category: cfg.ActionCategory,
			Currency: currency, Value: cfg.ActionValue, WindowDays: cfg.WindowDays})
		return err
	}}
	return it
}

func firstAction(xs ...*google.SetupAction) *google.SetupAction {
	for _, x := range xs {
		if x != nil {
			return x
		}
	}
	return nil
}

func (in googleInput) mappingItem(actions []google.SetupAction, err error) Item {
	it := Item{Key: "google.mapping", Title: "MarketFlow sends your leads to Google"}
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, in.Cfg.link("google_goals", "Open conversions", in.CustomerID))
	}
	mapped, named := in.leadAction(actions)
	if mapped != nil {
		it.Status, it.Detail = StatusOK, "Leads that came from a Google ad are sent to \""+mapped.Name+"\"."
		return it
	}
	if named == nil {
		it.Status, it.Detail = StatusAction, "First let Google count your leads (the check above). Then MarketFlow can send them."
		return it
	}
	it.Status, it.Detail = StatusAction, "The conversion is ready in Google Ads, but MarketFlow is not sending leads to it yet."
	next := map[string]string{}
	for k, v := range in.Mapping {
		next[k] = v
	}
	next["Lead"] = named.ResourceName
	it.Fix = &Fix{Label: "Send leads to Google", Changes: []string{
		"Send each MarketFlow lead that came from a Google ad click to \"" + named.Name + "\". MarketFlow sends the click id, time and value.",
		"Only MarketFlow's own setting changes. Nothing changes in Google Ads.",
	}, run: func(ctx context.Context, _ map[string]string) error { return in.SaveMapping(ctx, next) }}
	return it
}

func (in googleInput) goalItem(actions []google.SetupAction, camps []google.SetupCampaign, accGoals, campGoals []google.SetupGoal, own map[string]bool, err error) Item {
	it := Item{Key: "google.goals", Title: "Every campaign aims for leads"}
	l := in.Cfg.link("google_goals", "Open goals", in.CustomerID)
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, l)
	}
	mapped, named := in.leadAction(actions)
	x := firstAction(mapped, named)
	if x == nil {
		it.Status, it.Detail = StatusAction, "First let Google count your leads (the check above)."
		return it
	}
	if len(camps) == 0 {
		it.Status, it.Detail = StatusInfo, "There is no running campaign yet. MarketFlow checks this once one is running."
		return it
	}
	same := func(g google.SetupGoal) bool { return g.Category == x.Category && g.Origin == x.Origin && g.Biddable }
	has := map[string]bool{}
	for _, g := range campGoals {
		if same(g) {
			has[g.Campaign] = true
		}
	}
	accountHas := false
	for _, g := range accGoals {
		accountHas = accountHas || same(g)
	}
	var ownIDs, missing, changes []string
	viaAccount := false
	for _, c := range camps {
		if has[c.ResourceName] {
			continue
		}
		missing = append(missing, c.Name)
		// A campaign with its own goal list gets the goal added to it, which keeps its other goals.
		// One that follows the account gets it through the account's defaults.
		if own[c.ResourceName] || accountHas {
			ownIDs = append(ownIDs, c.ID)
			changes = append(changes, "Add the goal "+words(x.Category)+" to the campaign \""+c.Name+"\". Its other goals stay.")
		} else {
			viaAccount = true
		}
	}
	if len(missing) == 0 {
		it.Status, it.Detail = StatusOK, "All running campaigns use the goal "+words(x.Category)+"."
		return it
	}
	it.Status, it.Points, it.Link = StatusAction, missing, l
	it.Detail = fmt.Sprintf("%d of %d running campaigns do not use the goal %s, so Google does not look for leads in them.", len(missing), len(camps), words(x.Category))
	if x.Category == "" || x.Origin == "" {
		return it
	}
	if viaAccount {
		changes = append(changes, "Make "+words(x.Category)+" one of the account's default goals. Every campaign that follows the account's defaults will use it.")
	}
	changes = append(changes, "Google will change how it bids in these campaigns. Budgets do not change.")
	it.Fix = &Fix{Label: "Add the goal", Changes: changes, run: func(ctx context.Context, _ map[string]string) error {
		if viaAccount {
			if err := in.Ads.AddAccountGoal(ctx, in.CustomerID, x.Category, x.Origin); err != nil {
				return err
			}
		}
		if len(ownIDs) > 0 {
			return in.Ads.AddCampaignGoals(ctx, in.CustomerID, ownIDs, x.Category, x.Origin)
		}
		return nil
	}}
	return it
}

func (in googleInput) taggingItem(acc google.SetupAccount) Item {
	it := Item{Key: "google.auto_tagging", Title: "Auto-tagging on"}
	if acc.AutoTagging {
		it.Status, it.Detail = StatusOK, "Google marks each ad click, so MarketFlow knows which leads came from your ads."
		return it
	}
	it.Status = StatusAction
	it.Detail = "Google does not mark your ad clicks, so MarketFlow cannot tell which leads came from your ads."
	it.Fix = &Fix{Label: "Switch it on", Changes: []string{
		"Switch auto-tagging on for the whole account. Google adds a click id (gclid) to the end of your ad links.",
		"Budgets and bids do not change.",
	}, run: func(ctx context.Context, _ map[string]string) error {
		return in.Ads.EnableAutoTagging(ctx, in.CustomerID)
	}}
	return it
}

// googleReasons explains campaign.primary_status_reasons; any other reason is shown in words.
var googleReasons = map[string]string{
	"BUDGET_CONSTRAINED":                  "the daily budget runs out, so some people never see the ad",
	"HAS_ADS_LIMITED_BY_POLICY":           "some ads are limited by a Google policy",
	"HAS_ADS_DISAPPROVED":                 "some ads were disapproved",
	"MOST_ADS_UNDER_REVIEW":               "Google is still reviewing most ads",
	"BIDDING_STRATEGY_LEARNING":           "Google is learning who responds; leave it alone for about a week",
	"BIDDING_STRATEGY_LIMITED":            "the bidding target is too tight",
	"BIDDING_STRATEGY_MISCONFIGURED":      "the bidding setup has a problem, often missing conversions",
	"BIDDING_STRATEGY_CONSTRAINED":        "the bidding target is too tight",
	"HAS_ASSET_GROUPS_LIMITED_BY_POLICY":  "some ad material is limited by a Google policy",
	"HAS_ASSET_GROUPS_DISAPPROVED":        "some ad material was disapproved",
	"MOST_ASSET_GROUPS_UNDER_REVIEW":      "Google is still reviewing most of the ad material",
	"ASSET_GROUPS_LIMITED_BY_POLICY":      "some ad material is limited by a Google policy",
	"MISSING_LEAD_FORM_EXTENSION":         "the lead form is missing",
	"NO_AD_GROUPS":                        "the campaign has no ad group",
	"NO_ASSET_GROUPS":                     "the campaign has no ad material",
	"ASSET_GROUPS_PAUSED":                 "all ad material is paused",
	"AD_GROUPS_PAUSED":                    "all ad groups are paused",
	"CALL_EXTENSION_DISAPPROVED":          "the phone number was disapproved",
}

func (in googleInput) statusItem(camps []google.SetupCampaign, ads []google.LimitedAd, err error) Item {
	it := Item{Key: "google.status", Title: "Campaigns running and within policy"}
	l := in.Cfg.link("google_campaigns", "Open campaigns", in.CustomerID)
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, l)
	}
	if len(camps) == 0 {
		it.Status, it.Detail = StatusInfo, "There is no running campaign yet."
		return it
	}
	worst := StatusOK
	raise := func(s string) {
		if s == StatusAction || worst == StatusOK {
			worst = s
		}
	}
	for _, c := range camps {
		if c.PrimaryStatus == "ELIGIBLE" || c.PrimaryStatus == "" {
			continue
		}
		var why []string
		for _, r := range c.Reasons {
			if t, ok := googleReasons[r]; ok {
				why = append(why, t)
			} else {
				why = append(why, strings.ToLower(words(r)))
			}
		}
		line := c.Name + ": " + strings.ToLower(words(c.PrimaryStatus))
		if len(why) > 0 {
			line += " (" + strings.Join(why, "; ") + ")"
		}
		it.Points = append(it.Points, line)
		switch c.PrimaryStatus {
		case "NOT_ELIGIBLE", "MISCONFIGURED":
			raise(StatusAction)
		case "LIMITED":
			raise(StatusWarning)
		}
	}
	for _, a := range ads {
		line := a.Campaign + ": an ad was disapproved"
		if a.Approval == "DISAPPROVED" {
			raise(StatusAction)
		} else {
			line = a.Campaign + ": an ad is approved with limits"
			raise(StatusWarning)
		}
		if len(a.Topics) > 0 {
			line += " (" + strings.Join(a.Topics, ", ") + ")"
		}
		it.Points = append(it.Points, line)
	}
	it.Status = worst
	switch {
	case worst == StatusOK && len(it.Points) == 0:
		it.Detail = "All running campaigns are eligible and Google reports no policy problem."
	case worst == StatusOK:
		it.Detail = "Nothing to fix. Google gave these notes."
	default:
		it.Link = l
		it.Detail = "Google reports these problems. Only you or Google can change them, so open the page to see what to do."
		if len(ads) > 0 {
			it.Link = in.Cfg.link("google_policy", "Open policy manager", in.CustomerID)
		}
	}
	return it
}

func (in googleInput) assetItems(n google.AssetCount, camps []google.SetupCampaign, p Profile, err error) []Item {
	l := in.Cfg.link("google_assets", "Open assets", in.CustomerID)
	if err != nil {
		return []Item{unread("google.assets", "Sitelinks, phone and callouts", "Google", err, l)}
	}
	// The number every running campaign has at least: the account's plus its own.
	lowest := func(t string) int {
		m := n.Account[t]
		for i, c := range camps {
			if v := n.Account[t] + n.Campaigns[c.ID][t]; i == 0 || v < m {
				m = v
			}
		}
		return m
	}
	return []Item{
		in.sitelinkItem(lowest("SITELINK"), p, l),
		in.callItem(lowest("CALL"), p, l),
		in.calloutItem(lowest("CALLOUT"), p, l),
	}
}

func (in googleInput) create(ctx context.Context, assets []google.NewAsset) error {
	if len(assets) == 0 {
		return badInput("fill in at least one row")
	}
	return in.Ads.CreateAccountAssets(ctx, in.CustomerID, assets)
}

func (in googleInput) sitelinkItem(have int, p Profile, l *Link) Item {
	it := Item{Key: "google.sitelinks", Title: "Sitelinks (extra links under your ad)"}
	if have >= in.Cfg.MinSitelinks {
		it.Status, it.Detail = StatusOK, fmt.Sprintf("Your ads have %d sitelinks.", have)
		return it
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = fmt.Sprintf("Your ads have %d sitelinks; aim for %d. They take people straight to the right page and make the ad larger.", have, in.Cfg.MinSitelinks)
	fx := &Fix{Label: "Add sitelinks", Changes: []string{
		"Create the links below in Google Ads for the whole account. Google may show them under any of your ads.",
		"Only pages on your own website work. Leave a row empty to skip it.",
	}}
	for i := 0; i < sitelinkRows; i++ {
		var pg Page
		if i < len(p.Pages) {
			pg = p.Pages[i]
		}
		k := strconv.Itoa(i + 1)
		fx.Fields = append(fx.Fields,
			Field{Key: "text_" + k, Label: "Link " + k + " text", Value: clip(pg.Title, maxSitelinkText), Max: maxSitelinkText},
			Field{Key: "url_" + k, Label: "Link " + k + " address", Value: pg.URL, Max: 500})
	}
	fx.run = func(ctx context.Context, v map[string]string) error {
		var out []google.NewAsset
		for i := 1; i <= sitelinkRows; i++ {
			k := strconv.Itoa(i)
			t, u := v["text_"+k], v["url_"+k]
			if t == "" && u == "" {
				continue
			}
			if t == "" || !httpsURL(u) {
				return badInput("link %s needs a text and an address that starts with https://", k)
			}
			out = append(out, google.NewAsset{FieldType: "SITELINK", Text: t, URL: u})
		}
		return in.create(ctx, out)
	}
	it.Fix = fx
	return it
}

func (in googleInput) callItem(have int, p Profile, l *Link) Item {
	it := Item{Key: "google.call", Title: "Phone number on your ads"}
	if have > 0 {
		it.Status, it.Detail = StatusOK, "Your ads carry a phone number people can tap to call."
		return it
	}
	it.Status, it.Link, it.Detail = StatusWarning, l, "Your ads have no phone number, so people cannot call you from the ad."
	it.Fix = &Fix{Label: "Add the number", Changes: []string{
		"Add this phone number to your ads for the whole account. Google charges a tap on the number like a click on the ad.",
		"Google may ask you to verify the number.",
	}, Fields: []Field{
		{Key: "country", Label: "Country code (two letters)", Value: p.Country, Max: 2, Required: true},
		{Key: "phone", Label: "Phone number", Value: p.Phone, Max: 25, Required: true},
	}, run: func(ctx context.Context, v map[string]string) error {
		if len(v["country"]) != 2 {
			return badInput("the country code is two letters, for example KE")
		}
		return in.create(ctx, []google.NewAsset{{FieldType: "CALL", Country: strings.ToUpper(v["country"]), Phone: v["phone"]}})
	}}
	return it
}

func (in googleInput) calloutItem(have int, p Profile, l *Link) Item {
	it := Item{Key: "google.callouts", Title: "Callouts (short facts under your ad)"}
	if have >= in.Cfg.MinCallouts {
		it.Status, it.Detail = StatusOK, fmt.Sprintf("Your ads have %d callouts.", have)
		return it
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = fmt.Sprintf("Your ads have %d callouts; aim for %d. Each is a short fact such as a service or your town.", have, in.Cfg.MinCallouts)
	fx := &Fix{Label: "Add callouts", Changes: []string{
		"Add the texts below to your ads for the whole account. They must be true: Google and the law treat them as claims.",
		"Leave a row empty to skip it.",
	}}
	texts := calloutTexts(p)
	for i := 0; i < calloutRows; i++ {
		v := ""
		if i < len(texts) {
			v = texts[i]
		}
		k := strconv.Itoa(i + 1)
		fx.Fields = append(fx.Fields, Field{Key: "callout_" + k, Label: "Callout " + k, Value: v, Max: maxCalloutText})
	}
	fx.run = func(ctx context.Context, v map[string]string) error {
		var out []google.NewAsset
		for i := 1; i <= calloutRows; i++ {
			if t := v["callout_"+strconv.Itoa(i)]; t != "" {
				out = append(out, google.NewAsset{FieldType: "CALLOUT", Text: t})
			}
		}
		return in.create(ctx, out)
	}
	it.Fix = fx
	return it
}

// calloutTexts proposes the business's own service names and town, never a claim it did not make.
func calloutTexts(p Profile) []string {
	var out []string
	for _, s := range append([]string{cityLine(p.City)}, p.Services...) {
		if s = strings.TrimSpace(s); s != "" && len([]rune(s)) <= maxCalloutText {
			out = append(out, s)
		}
	}
	return out
}

func cityLine(city string) string {
	if city == "" {
		return ""
	}
	return "Based in " + city
}

func (in googleInput) listItem(lists []google.UserList, consented int, err error) Item {
	it := Item{Key: "google.customer_list", Title: "Customer list"}
	l := in.Cfg.link("google_audiences", "Open audiences", in.CustomerID)
	if err != nil {
		return unread(it.Key, it.Title, "Google", err, l)
	}
	var best google.UserList
	for _, x := range lists {
		if max(x.SearchSize, x.DisplaySize) >= max(best.SearchSize, best.DisplaySize) {
			best = x
		}
	}
	size := max(best.SearchSize, best.DisplaySize)
	need := in.Cfg.ListMin
	if size >= need {
		it.Status, it.Detail = StatusOK, fmt.Sprintf("The list \"%s\" has about %d people, enough for Google's customer lifecycle goals.", best.Name, size)
		return it
	}
	it.Status = StatusInfo
	it.Detail = fmt.Sprintf("Google's customer lifecycle goals need a customer list with at least %d active members. You can ignore the warning Google shows about it, and keep customer acquisition switched off.", need)
	if len(lists) == 0 {
		it.Points = append(it.Points, "This account has no customer list.")
	} else {
		it.Points = append(it.Points, fmt.Sprintf("The largest list, \"%s\", has about %d people.", best.Name, size))
	}
	if consented >= 0 {
		it.Points = append(it.Points, fmt.Sprintf("MarketFlow has %d contacts with an email and a recorded marketing consent.", consented))
		if int64(consented) >= need {
			// Google closed Customer Match uploads through the Google Ads API to new adopters on
			// 2026-04-01 (Google Ads developer blog, 2026-03-04), so there is no fix to offer.
			it.Link = l
			it.Points = append(it.Points, "Google no longer lets MarketFlow upload a list for you. You may upload one in Google Ads yourself, with only people who agreed to their details being used for ads.")
		}
	}
	return it
}
