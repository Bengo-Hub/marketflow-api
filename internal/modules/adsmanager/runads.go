package adsmanager

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/admetrics"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
)

// Run ads (owner 2026-11-24): one plan for Meta, Google Ads and TikTok after a video is approved,
// with a daily budget per platform, the run days and a total spend cap. The plan is built from
// the tenant's defaults (platform/runads), shows recommended budgets with where each number comes
// from, says whether each account can spend and where to add money, and spends nothing until the
// owner confirms that exact plan (its fingerprint). Meta runs through the boost (Boost, unchanged);
// Google Ads is created through the Google Ads API; TikTok is guided links until TikTok approves
// MarketFlow's Marketing API app, when a TikTokLauncher slots in behind run_ads.tiktok_mode.

// RunAdsDeps is what Run ads needs beyond the Meta boost. nil switches Run ads off.
type RunAdsDeps struct {
	// GoogleTokens refreshes a Google Ads account's access token (google.NewTokensFor).
	GoogleTokens *google.Tokens
	// GoogleExtra reads the platform Google Ads client's extra keys (api_version,
	// login_customer_id); an error means the platform client is not configured.
	GoogleExtra func(ctx context.Context) (map[string]string, error)
	GoogleHTTP  *http.Client
	GoogleBase  string // Google Ads REST host; "" for Google's own (tests set it)
	// BrandImages signs the tenant's brand image keys (brandimages.Linker.Links).
	BrandImages func(tenantID uuid.UUID, keys []string) []string
	// Fetch downloads the logo and pictures (a public-only client).
	Fetch *http.Client
	// Currency is the tenant's currency (tenant.CurrencyOf).
	Currency func(ctx context.Context, tenantID uuid.UUID) string
	// PublicPage is the tenant's MarketFlow public page, the fallback landing link ("" for none).
	PublicPage func(slug string) string
	// TikTok creates TikTok campaigns through the Marketing API; nil means guided links.
	TikTok TikTokLauncher
	Now    func() time.Time
}

// TikTokLauncher creates a TikTok campaign through the Marketing API. It is registered only once
// TikTok approves the app; run_ads.tiktok_mode "api" uses it, and without it TikTok stays guided.
type TikTokLauncher interface {
	Launch(ctx context.Context, tenantID uuid.UUID, p PlatformPlan, videoAssetID string) (PlatformResult, error)
}

func (d *RunAdsDeps) now() time.Time {
	if d != nil && d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// PlanStep is one thing the owner does: a missing piece to set up, a guided step, or a funding link.
// Path is a MarketFlow page ("/settings/integrations"); URL is a provider page.
type PlanStep struct {
	Label string `json:"label"`
	Path  string `json:"path,omitempty"`
	URL   string `json:"url,omitempty"`
	Done  bool   `json:"done,omitempty"`
}

// FundingView says whether an account can spend. CanSpend is nil when the provider does not show it.
type FundingView struct {
	CanSpend  *bool  `json:"can_spend"`
	Status    string `json:"status"`
	Detail    string `json:"detail"`
	Shortfall int64  `json:"shortfall,omitempty"` // account currency
	Link      string `json:"link,omitempty"`
}

// PlanAccount is the account a platform runs on.
type PlanAccount struct {
	ConnectedID string `json:"connected_account_id"`
	ExternalID  string `json:"external_id"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
}

// PlatformPlan is one platform in the plan. Amounts are whole units of the tenant's currency;
// AccountDaily and AccountTotal are what is sent to the platform in the account's currency.
type PlatformPlan struct {
	Platform string `json:"platform"`
	Enabled  bool   `json:"enabled"`
	// Mode is automatic (Meta boost), api (Google Ads API, or TikTok once approved) or guided.
	Mode         string            `json:"mode"`
	Account      *PlanAccount      `json:"account,omitempty"`
	Tier         string            `json:"tier"`
	Daily        int64             `json:"daily"`
	Days         int               `json:"days"`
	Total        int64             `json:"total"`
	AccountDaily int64             `json:"account_daily,omitempty"`
	AccountTotal int64             `json:"account_total,omitempty"`
	Tiers        []runads.Tier     `json:"tiers"`
	Estimates    []runads.Estimate `json:"estimates"`
	Funding      FundingView       `json:"funding"`
	Setup        []PlanStep        `json:"setup"`
	Steps        []PlanStep        `json:"steps"`
	Blockers     []string          `json:"blockers"`
	Notes        []string          `json:"notes"`
	// LaunchCredits is MarketFlow's charge for a Meta launch (0 on a plan that includes it).
	LaunchCredits int `json:"launch_credits,omitempty"`
}

// Ready says whether this platform can be confirmed: on, an amount, no blocker, nothing to set up.
func (p PlatformPlan) Ready() bool {
	return p.Enabled && p.Daily > 0 && len(p.Blockers) == 0 && len(p.Setup) == 0
}

// DaysHint is a run length suggestion with its source.
type DaysHint struct {
	Days   int    `json:"days"`
	Source string `json:"source"`
	Label  string `json:"label"`
}

// RunPlan is the whole plan the owner confirms.
type RunPlan struct {
	VersionID   string         `json:"version_id"`
	Currency    string         `json:"currency"`
	Days        int            `json:"days"`
	TotalCap    int64          `json:"total_cap"`
	Total       int64          `json:"total"`
	Platforms   []PlatformPlan `json:"platforms"`
	DaysHint    *DaysHint      `json:"days_hint,omitempty"`
	Blockers    []string       `json:"blockers"`
	Ready       bool           `json:"ready"`
	OTPRequired bool           `json:"otp_required"`
	Fingerprint string         `json:"fingerprint"`
}

// PlatformChoice is the owner's change to one platform; zero values keep the default.
type PlatformChoice struct {
	Platform string `json:"platform"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Daily    int64  `json:"daily,omitempty"`
	Tier     string `json:"tier,omitempty"`
}

// Choice is the owner's changes to the default plan.
type Choice struct {
	Days      int              `json:"days,omitempty"`
	TotalCap  *int64           `json:"total_cap,omitempty"`
	Platforms []PlatformChoice `json:"platforms,omitempty"`
}

func (c Choice) of(p string) PlatformChoice {
	for _, x := range c.Platforms {
		if x.Platform == p {
			return x
		}
	}
	return PlatformChoice{Platform: p}
}

// planCtx is what building the plan learned that running it needs again.
type planCtx struct {
	tenant   *ent.Tenant
	version  *ent.CreativeVersion
	asset    *ent.CreativeAsset
	settings runads.Settings
	cur      string // the tenant's currency, the plan's
	meta     *waBoostPlan
	google   *googleTarget
	tiktok   *ent.ConnectedAccount
}

var (
	errRunAdsOff   = errors.New("run ads is not set up on this server")
	errNotApproved = errors.New("not_approved")
)

// BuildPlan builds the plan for an approved version with the owner's choices over the defaults.
func (h *Handler) BuildPlan(ctx context.Context, tid, vid uuid.UUID, ch Choice) (*RunPlan, *planCtx, error) {
	if h.RunAds == nil {
		return nil, nil, errRunAdsOff
	}
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return nil, nil, err
	}
	if v.Status != "approved" {
		return nil, nil, errNotApproved
	}
	asset, err := h.DB.CreativeAsset.Query().
		Where(creativeasset.VersionIDEQ(vid), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).
		First(ctx)
	if err != nil {
		return nil, nil, err
	}
	t, err := h.DB.Tenant.Get(ctx, tid)
	if err != nil {
		return nil, nil, err
	}
	settings, _ := runads.Load(ctx, h.DB, t)
	cur := "KES"
	if h.RunAds.Currency != nil {
		cur = h.RunAds.Currency(ctx, tid)
	}
	pc := &planCtx{tenant: t, version: v, asset: asset, settings: settings, cur: cur}
	plan := &RunPlan{VersionID: vid.String(), Currency: cur, Days: settings.Days, Blockers: []string{}}
	if ch.Days >= runads.MinDays && ch.Days <= runads.MaxDays {
		plan.Days = ch.Days
	}
	switch {
	case ch.TotalCap != nil && *ch.TotalCap >= 0:
		plan.TotalCap = *ch.TotalCap
	case settings.TotalCapSet:
		plan.TotalCap = settings.TotalCap
	case settings.TotalCapKES > 0:
		if n, ok := h.fromKES(ctx, settings.TotalCapKES, cur); ok {
			plan.TotalCap = n
		}
	}
	if d := competitorDays(t.Metadata); d > 0 {
		plan.DaysHint = &DaysHint{Days: d, Source: runads.SourceCompetitors,
			Label: "Your competitors' ads that are still running have run for a median of " + strconv.Itoa(d) + " days (your weekly competitor scan)"}
	}
	for _, p := range runads.AllPlatforms {
		c := ch.of(p)
		pp := PlatformPlan{Platform: p, Enabled: settings.On(p), Days: plan.Days, Tier: settings.Tier,
			Tiers: []runads.Tier{}, Estimates: []runads.Estimate{}, Setup: []PlanStep{}, Steps: []PlanStep{}, Blockers: []string{}, Notes: []string{}}
		if c.Enabled != nil {
			pp.Enabled = *c.Enabled
		}
		if c.Tier != "" {
			pp.Tier = c.Tier
		}
		own := settings.DailyBudget[p]
		if c.Daily > 0 {
			own = c.Daily
		}
		in := runads.TierInput{Days: plan.Days, Past: h.pastResults(ctx, tid, p, cur)}
		if kes, ok := settings.RuleOfThumbKES[p]; ok {
			if conv, ok := h.tiersFromKES(ctx, kes, cur); ok {
				in.RuleOfThumb = conv
			} else {
				pp.Notes = append(pp.Notes, "The rule of thumb is in KES and the exchange rate for "+cur+" could not be read, so type an amount.")
			}
		}
		switch p {
		case runads.Meta:
			h.planMeta(ctx, tid, pc, &pp, &in, own)
		case runads.Google:
			h.planGoogle(ctx, tid, pc, &pp, &in, own, cur)
		case runads.TikTok:
			h.planTikTok(ctx, tid, pc, &pp, &in, own, cur)
		}
		if pp.Daily <= 0 && pp.Enabled {
			pp.Blockers = append(pp.Blockers, "choose a daily budget")
		}
		pp.Total = pp.Daily * int64(pp.Days)
		if pp.Enabled {
			plan.Total += pp.Total
		}
		plan.Platforms = append(plan.Platforms, pp)
	}
	if plan.TotalCap > 0 && plan.Total > plan.TotalCap {
		plan.Blockers = append(plan.Blockers, fmt.Sprintf("the plan totals %s %d, over your total cap of %s %d; lower a budget or the days",
			cur, plan.Total, cur, plan.TotalCap))
	}
	on := 0
	ready := len(plan.Blockers) == 0
	for _, pp := range plan.Platforms {
		if !pp.Enabled {
			continue
		}
		on++
		if !pp.Ready() {
			ready = false
		}
	}
	if on == 0 {
		plan.Blockers = append(plan.Blockers, "switch on at least one platform")
		ready = false
	}
	plan.Ready = ready
	if h.Codes != nil {
		plan.OTPRequired = h.otpNeeded(ctx, h.boostOTPThreshold(ctx, tid), plan.Total, cur)
	}
	plan.Fingerprint = planFingerprint(plan)
	return plan, pc, nil
}

// planFingerprint binds a confirmation to what the owner saw: the video, the days, the cap and,
// for every platform on, its account, amounts and mode.
func planFingerprint(p *RunPlan) string {
	parts := []string{p.VersionID, strconv.Itoa(p.Days), strconv.FormatInt(p.TotalCap, 10), p.Currency}
	for _, pp := range p.Platforms {
		if !pp.Enabled {
			continue
		}
		acc := ""
		if pp.Account != nil {
			acc = pp.Account.ConnectedID
		}
		parts = append(parts, pp.Platform, pp.Mode, acc, strconv.FormatInt(pp.Daily, 10), strconv.FormatInt(pp.AccountTotal, 10))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:32]
}

// --- currency ---

// kesPer is how many KES one unit of cur is worth (1 for KES).
func (h *Handler) kesPer(ctx context.Context, cur string) (float64, bool) {
	cur = strings.ToUpper(strings.TrimSpace(cur))
	if cur == "" || cur == "KES" {
		return 1, true
	}
	if h.KESPer == nil {
		return 0, false
	}
	r, err := h.KESPer(ctx, cur)
	if err != nil || r <= 0 || math.IsNaN(r) || math.IsInf(r, 0) {
		return 0, false
	}
	return r, true
}

// convert moves a whole amount between currencies with treasury's rates, rounded up so a small
// amount never becomes 0.
func (h *Handler) convert(ctx context.Context, amount int64, from, to string) (int64, bool) {
	if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(to)) || amount == 0 {
		return amount, true
	}
	a, ok1 := h.kesPer(ctx, from)
	b, ok2 := h.kesPer(ctx, to)
	if !ok1 || !ok2 {
		return 0, false
	}
	return int64(math.Ceil(float64(amount) * a / b)), true
}

func (h *Handler) fromKES(ctx context.Context, kes int64, to string) (int64, bool) {
	return h.convert(ctx, kes, "KES", to)
}

func (h *Handler) tiersFromKES(ctx context.Context, kes runads.Tiers, to string) (runads.Tiers, bool) {
	out := runads.Tiers{}
	for k, v := range kes {
		n, ok := h.fromKES(ctx, v, to)
		if !ok {
			return nil, false
		}
		out[k] = n
	}
	return out, true
}

// pastResults sums the tenant's stored ad metrics on a platform over the last 180 days, in the
// tenant's currency (rows in another currency are converted, or left out without a rate).
func (h *Handler) pastResults(ctx context.Context, tid uuid.UUID, platform, cur string) *runads.Past {
	since := h.RunAds.now().AddDate(0, 0, -180)
	rows, err := h.DB.AdMetrics.Query().
		Where(admetrics.TenantID(tid), admetrics.PlatformEQ(platform), admetrics.DateGTE(since)).
		Limit(5000).All(ctx)
	if err != nil || len(rows) == 0 {
		return nil
	}
	p := &runads.Past{}
	for _, r := range rows {
		spend := r.Spend
		if c := strings.ToUpper(r.Currency); c != "" && c != strings.ToUpper(cur) {
			a, ok1 := h.kesPer(ctx, c)
			b, ok2 := h.kesPer(ctx, cur)
			if !ok1 || !ok2 {
				continue
			}
			spend = spend * a / b
		}
		p.Spend += spend
		p.Impressions += r.Impressions
		p.Clicks += r.Clicks
		p.Leads += r.Leads
	}
	if p.Spend <= 0 {
		return nil
	}
	return p
}

// competitorDays is the median run length of the competitor ads still running in the tenant's
// competitor scan (metadata.competitor_scan), 0 without any.
func competitorDays(meta map[string]any) int {
	scan, _ := meta["competitor_scan"].(map[string]any)
	list, _ := scan["competitors"].([]any)
	var days []int
	for _, c := range list {
		m, _ := c.(map[string]any)
		winners, _ := m["winners"].([]any)
		for _, w := range winners {
			wm, _ := w.(map[string]any)
			if running, _ := wm["still_running"].(bool); !running {
				continue
			}
			if d, ok := wm["total_days_shown"].(float64); ok && d > 0 {
				days = append(days, int(d))
			}
		}
	}
	return runads.MedianDays(days)
}

// pickDaily chooses the platform's daily amount and fills its tiers and estimates.
func pickDaily(pp *PlatformPlan, in runads.TierInput, own int64) {
	pp.Tiers = runads.BuildTiers(in)
	pp.Daily, pp.Tier = runads.Pick(pp.Tiers, pp.Tier, own)
	if pp.Daily > 0 {
		pp.Estimates = runads.EstimatesFor(pp.Daily, in)
		for _, t := range pp.Tiers {
			if t.Name == pp.Tier && len(t.Estimates) > 0 {
				pp.Estimates = t.Estimates
			}
		}
	}
	if len(pp.Tiers) == 0 {
		pp.Notes = append(pp.Notes, "No platform estimate, past results or rule of thumb is available, so type a daily amount.")
	}
}

// --- Meta ---

var metaGoals = map[string]struct{ goal, metric string }{
	"whatsapp": {"CONVERSATIONS", "conversations"},
	"traffic":  {"LINK_CLICKS", "clicks"},
	"leads":    {"LEAD_GENERATION", "leads"},
}

func (h *Handler) planMeta(ctx context.Context, tid uuid.UUID, pc *planCtx, pp *PlatformPlan, in *runads.TierInput, own int64) {
	pp.Mode = "automatic"
	if h.Enabled != nil && !h.Enabled(ctx) {
		pp.Blockers = append(pp.Blockers, "paid Meta ads are not switched on for MarketFlow yet")
	}
	d := h.waBoostDefaults(ctx, tid)
	accs, _ := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("meta_ad_account"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	acc := pickAccount(accs, d.AdAccountConnID, true)
	pages, _ := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("facebook_page"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	page := pickAccount(pages, d.PageConnID, false)
	if acc == nil {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Connect your Meta ad account", Path: "/settings/integrations"})
	}
	if page == nil {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Connect the Facebook Page the ad runs as", Path: "/settings/integrations"})
	}
	var tok string
	if acc != nil {
		accCur := accountCurrency(acc)
		pp.Account = &PlanAccount{ConnectedID: acc.ID.String(), ExternalID: acc.ExternalID, Name: acc.DisplayName, Currency: accCur}
		tok, _ = h.token(ctx, acc)
		if g, ok := metaGoals[d.Objective]; ok && tok != "" && h.Meta != nil {
			if est, err := h.Meta.DeliveryEstimate(ctx, tok, acc.ExternalID, g.goal, nil); err == nil {
				for _, pt := range est.Curve {
					whole := FromMinor(strconv.FormatInt(int64(pt.Spend), 10), firstNonEmpty(accCur, "KES"))
					if n, ok := h.convert(ctx, whole, firstNonEmpty(accCur, "KES"), pc.cur); ok && n > 0 {
						in.Curve = append(in.Curve, runads.CurvePoint{Spend: float64(n), Reach: pt.Reach, Actions: pt.Actions})
					}
				}
				in.CurveLabel, in.ActionMetric = "Meta delivery estimate for this ad account", g.metric
			} else {
				h.Logger.Debug("meta delivery estimate", zap.Error(err))
			}
		}
	}
	pickDaily(pp, *in, own)
	if acc == nil || page == nil || pp.Daily <= 0 {
		return
	}
	cur := pc.cur
	accCur := firstNonEmpty(pp.Account.Currency, cur)
	daily, ok := h.convert(ctx, pp.Daily, cur, accCur)
	if !ok {
		pp.Blockers = append(pp.Blockers, "the exchange rate for the ad account's currency could not be read; try again shortly")
		return
	}
	pp.AccountDaily, pp.AccountTotal = daily, daily*int64(pp.Days)
	q, _, _, err := h.quote(ctx, tid, QuoteInput{AdAccountConnID: acc.ID.String(), Budget: pp.AccountTotal, Days: pp.Days})
	if err != nil {
		pp.Blockers = append(pp.Blockers, err.Error())
		return
	}
	if q.Currency != "" {
		pp.Account.Currency = q.Currency
	}
	pp.Blockers = append(pp.Blockers, q.Blockers...)
	pp.LaunchCredits = q.LaunchCredits
	pp.Funding = metaFunding(q)
	wp, err := h.waPlan(ctx, tid, pc.version.ID, pp.AccountTotal, pp.Days)
	if err == nil {
		pc.meta = wp
		pp.Notes = append(pp.Notes, "Runs as "+wp.PageName+", aimed at "+metaGoalText(wp.Objective)+"; starts paused and goes live once Meta approves it.")
	}
}

func metaGoalText(objective string) string {
	switch objective {
	case "traffic":
		return "visits to your link"
	case "leads":
		return "leads"
	}
	return "WhatsApp messages to your business"
}

func metaFunding(q Quote) FundingView {
	yes, no := true, false
	link := BillingURL(q.AccountID)
	switch q.Funding {
	case FundingCard:
		return FundingView{CanSpend: &yes, Status: string(q.Funding), Detail: "A card pays Meta after the ads run.", Link: link}
	case FundingPrepaid:
		return FundingView{CanSpend: &yes, Status: string(q.Funding), Detail: fmt.Sprintf("The prepaid balance (%s %d) covers this budget.", q.Currency, q.Balance), Link: link}
	case FundingNeeded:
		return FundingView{CanSpend: &no, Status: string(q.Funding), Shortfall: q.Shortfall, Link: link,
			Detail: fmt.Sprintf("Add %s %d on Meta (M-Pesa or card) before the ad can run.", q.Currency, q.Shortfall)}
	}
	return FundingView{Status: string(FundingUnknown), Shortfall: q.Shortfall, Link: link,
		Detail: "Meta does not show this account's balance. If it is prepaid, add the budget on Meta; the ad waits for your \"I've paid\"."}
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// --- TikTok ---

func (h *Handler) planTikTok(ctx context.Context, tid uuid.UUID, pc *planCtx, pp *PlatformPlan, in *runads.TierInput, own int64, cur string) {
	s := pc.settings
	pp.Mode = "guided"
	if s.TikTokMode == "api" {
		if h.RunAds.TikTok != nil {
			pp.Mode = "api"
		} else {
			pp.Notes = append(pp.Notes, "TikTok automation is switched on but not available yet, so TikTok stays guided.")
		}
	}
	acc, _ := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("tiktok_advertiser"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).First(ctx)
	adv := ""
	if acc != nil {
		pc.tiktok = acc
		adv = acc.ExternalID
		pp.Account = &PlanAccount{ConnectedID: acc.ID.String(), ExternalID: acc.ExternalID, Name: acc.DisplayName, Currency: accountCurrency(acc)}
	} else if pp.Mode == "api" {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Connect TikTok for Business", Path: "/settings/integrations"})
	}
	pickDaily(pp, *in, own)
	if s.TikTokMinUSD > 0 {
		if min, ok := h.convert(ctx, s.TikTokMinUSD, "USD", cur); ok && pp.Daily > 0 && pp.Daily < min {
			pp.Blockers = append(pp.Blockers, fmt.Sprintf("TikTok needs at least USD %d a day per ad group (about %s %d)", s.TikTokMinUSD, cur, min))
		}
	}
	pp.AccountDaily, pp.AccountTotal = pp.Daily, pp.Daily*int64(pp.Days)
	if pp.Account != nil && pp.Account.Currency != "" && !strings.EqualFold(pp.Account.Currency, cur) {
		if d, ok := h.convert(ctx, pp.Daily, cur, pp.Account.Currency); ok {
			pp.AccountDaily, pp.AccountTotal = d, d*int64(pp.Days)
		}
	}
	pp.Funding = FundingView{Status: "unknown", Link: s.TikTokLink("payment", adv),
		Detail: "TikTok does not let apps read the balance. Add funds on TikTok's payment page before the campaign starts."}
	start := h.RunAds.now()
	end := start.AddDate(0, 0, pp.Days-1)
	accCur := cur
	if pp.Account != nil && pp.Account.Currency != "" {
		accCur = pp.Account.Currency
	}
	if adv == "" {
		pp.Steps = append(pp.Steps, PlanStep{Label: "Sign in to TikTok Ads Manager (or create an account)", URL: s.TikTokLinkOr("signup")})
	}
	pp.Steps = append(pp.Steps,
		PlanStep{Label: "Download the approved video", Path: "/studio"},
		PlanStep{Label: fmt.Sprintf("Create a campaign with a daily budget of %s %d, scheduled %s to %s, and upload the video",
			accCur, pp.AccountDaily, start.Format("2 Jan"), end.Format("2 Jan 2006")), URL: s.TikTokLink("campaign", adv)},
		PlanStep{Label: fmt.Sprintf("Add at least %s %d on TikTok's payment page", accCur, pp.AccountTotal), URL: s.TikTokLink("payment", adv)},
	)
}

// --- route helpers ---

// PlanStatus maps a BuildPlan error to its refusal.
func planError(err error) (int, string, string) {
	switch {
	case errors.Is(err, errRunAdsOff):
		return http.StatusServiceUnavailable, "run_ads_off", "Run ads is not available on this server"
	case errors.Is(err, errNotApproved):
		return http.StatusConflict, "not_approved", "approve the video before running ads"
	case ent.IsNotFound(err):
		return http.StatusNotFound, "not_found", "the video is not available"
	}
	return 0, "", ""
}
