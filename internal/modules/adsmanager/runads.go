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
	"github.com/bengobox/marketflow-api/internal/platform/adaccounts"
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

// PlatformPlan is one platform in the plan. Amounts are whole units of the plan's currency;
// AccountDaily and AccountTotal are what is sent to the platform in the account's currency, and
// LocalDaily and LocalTotal are the same in the tenant's currency, shown beside.
type PlatformPlan struct {
	Platform string `json:"platform"`
	Enabled  bool   `json:"enabled"`
	// Mode is automatic (Meta boost), api (Google Ads API, or TikTok once approved) or guided.
	Mode         string       `json:"mode"`
	Account      *PlanAccount `json:"account,omitempty"`
	Tier         string       `json:"tier"`
	Daily        int64        `json:"daily"`
	Days         int          `json:"days"`
	Total        int64        `json:"total"`
	AccountDaily int64        `json:"account_daily,omitempty"`
	AccountTotal int64        `json:"account_total,omitempty"`
	LocalDaily   int64        `json:"local_daily,omitempty"`
	LocalTotal   int64        `json:"local_total,omitempty"`
	// MonthSpend is what ads on the platform have spent this month; nil when there is no data.
	MonthSpend *int64 `json:"month_spend"`
	// ResultsNote says plainly where results come from, or that there are none yet.
	ResultsNote string            `json:"results_note,omitempty"`
	Tiers       []runads.Tier     `json:"tiers"`
	Estimates   []runads.Estimate `json:"estimates"`
	Funding     FundingView       `json:"funding"`
	Setup       []PlanStep        `json:"setup"`
	Steps       []PlanStep        `json:"steps"`
	Blockers    []string          `json:"blockers"`
	Notes       []string          `json:"notes"`
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
	VersionID string `json:"version_id"`
	Currency  string `json:"currency"`
	// LocalCurrency and LocalTotal are the tenant's own currency, shown beside ("" when the plan is
	// already in it or no rate can be read).
	LocalCurrency string         `json:"local_currency,omitempty"`
	LocalTotal    int64          `json:"local_total,omitempty"`
	Notes         []string       `json:"notes"`
	Days          int            `json:"days"`
	TotalCap      int64          `json:"total_cap"`
	Total         int64          `json:"total"`
	Platforms     []PlatformPlan `json:"platforms"`
	DaysHint      *DaysHint      `json:"days_hint,omitempty"`
	Blockers      []string       `json:"blockers"`
	Ready         bool           `json:"ready"`
	OTPRequired   bool           `json:"otp_required"`
	Fingerprint   string         `json:"fingerprint"`
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
	// Currency is what the amounts below were typed in ("" for the plan's). A plan now in another
	// currency converts them, so an amount is never relabelled.
	Currency  string           `json:"currency,omitempty"`
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
	cur      string // the plan's currency
	local    string // the tenant's currency
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
	return h.buildPlan(ctx, tid, vid, ch, false)
}

// BuildDraftPlan is BuildPlan for a finished video that still waits for approval, so one message
// can show what approving it would run. Running it needs the approval (BuildPlan).
func (h *Handler) BuildDraftPlan(ctx context.Context, tid, vid uuid.UUID, ch Choice) (*RunPlan, *planCtx, error) {
	return h.buildPlan(ctx, tid, vid, ch, true)
}

func (h *Handler) buildPlan(ctx context.Context, tid, vid uuid.UUID, ch Choice, draft bool) (*RunPlan, *planCtx, error) {
	if h.RunAds == nil {
		return nil, nil, errRunAdsOff
	}
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return nil, nil, err
	}
	if v.Status != "approved" && !(draft && v.Status == "ready") {
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
	local := h.tenantCurrency(ctx, tid)
	// Plans are in the ads currency. Without its rate they fall back to the tenant's own.
	cur, notes := firstNonEmpty(settings.Currency, runads.DefaultCurrency), []string{}
	if cur != local {
		if _, ok := h.convert(ctx, 1, local, cur); !ok {
			notes = append(notes, "The exchange rate for "+cur+" could not be read, so this plan is in "+local+".")
			cur = local
		}
	}
	amounts := firstNonEmpty(settings.AmountsCurrency, local)
	pc := &planCtx{tenant: t, version: v, asset: asset, settings: settings, cur: cur, local: local}
	plan := &RunPlan{VersionID: vid.String(), Currency: cur, Days: settings.Days, Blockers: []string{}, Notes: notes}
	// typed turns an amount the owner typed into the plan's currency.
	typed := func(n int64) (int64, bool) { return h.convertDown(ctx, n, firstNonEmpty(ch.Currency, cur), cur) }
	noRate := func(from string) string {
		return "the exchange rate from " + from + " to " + cur + " could not be read; try again shortly"
	}
	if ch.Days >= runads.MinDays && ch.Days <= runads.MaxDays {
		plan.Days = ch.Days
	}
	// A cap that cannot be read in the plan's currency stops the plan; it is never dropped.
	capOK, capFrom := true, cur
	switch {
	case ch.TotalCap != nil && *ch.TotalCap >= 0:
		plan.TotalCap, capOK = typed(*ch.TotalCap)
		capFrom = ch.Currency
	case settings.TotalCapSet:
		plan.TotalCap, capOK = h.convertDown(ctx, settings.TotalCap, amounts, cur)
		capFrom = amounts
	case settings.TotalCapKES > 0:
		plan.TotalCap, capOK = h.convertDown(ctx, settings.TotalCapKES, "KES", cur)
		capFrom = "KES"
	}
	if !capOK {
		plan.Blockers = append(plan.Blockers, "total cap: "+noRate(capFrom))
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
		own, ownOK, ownFrom := int64(0), true, amounts
		if c.Daily > 0 {
			own, ownOK = typed(c.Daily)
			ownFrom = ch.Currency
		} else if settings.DailyBudget[p] > 0 {
			own, ownOK = h.convertDown(ctx, settings.DailyBudget[p], amounts, cur)
		}
		if !ownOK {
			pp.Blockers = append(pp.Blockers, noRate(ownFrom))
		}
		in := runads.TierInput{Days: plan.Days, Past: h.pastResults(ctx, tid, p, cur)}
		pp.MonthSpend = h.monthSpend(ctx, tid, p, cur)
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
		// A platform still to be set up is left out unless the owner switches it on, so a business
		// with one connected platform can confirm its plan; its to-dos stay in the plan.
		if len(pp.Setup) > 0 && c.Enabled == nil {
			pp.Enabled = false
		}
		if pp.Daily <= 0 && pp.Enabled {
			pp.Blockers = append(pp.Blockers, "choose a daily budget")
		}
		pp.Total = pp.Daily * int64(pp.Days)
		if pp.ResultsNote == "" {
			pp.ResultsNote = resultsNote(p, in.Past)
		}
		if n, ok := h.convert(ctx, pp.Daily, cur, local); ok && cur != local {
			pp.LocalDaily, pp.LocalTotal = n, n*int64(pp.Days)
		}
		if pp.Enabled {
			plan.Total += pp.Total
		}
		plan.Platforms = append(plan.Platforms, pp)
	}
	if n, ok := h.convert(ctx, plan.Total, cur, local); ok && cur != local {
		plan.LocalCurrency, plan.LocalTotal = local, n
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
	plan.Fingerprint = planFingerprint(plan, endedRuns(v.Metadata, h.RunAds.now()))
	return plan, pc, nil
}

// planFingerprint binds a confirmation to what the owner saw: the video, the days, the cap and,
// for every platform on, its account, amounts and mode. ended counts the video's runs that are
// over, so the same plan can be confirmed again later as a new run.
func planFingerprint(p *RunPlan, ended int) string {
	parts := []string{p.VersionID, strconv.Itoa(ended), strconv.Itoa(p.Days), strconv.FormatInt(p.TotalCap, 10), p.Currency}
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

// endedRuns counts the runs in metadata.ad_runs whose end has passed.
func endedRuns(meta map[string]any, now time.Time) int {
	n := 0
	for fp := range nested(meta, "ad_runs") {
		end, err := time.Parse(time.RFC3339, str(nested(nested(meta, "ad_runs"), fp)["ends_at"]))
		if err == nil && end.Before(now) {
			n++
		}
	}
	return n
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

// convertDown is convert rounded down (at least 1), for an amount that is a limit the owner set:
// a converted cap or budget never allows more than they said, apart from amounts under one unit.
func (h *Handler) convertDown(ctx context.Context, amount int64, from, to string) (int64, bool) {
	if strings.EqualFold(strings.TrimSpace(from), strings.TrimSpace(to)) || amount == 0 {
		return amount, true
	}
	a, ok1 := h.kesPer(ctx, from)
	b, ok2 := h.kesPer(ctx, to)
	if !ok1 || !ok2 {
		return 0, false
	}
	return max(1, int64(math.Floor(float64(amount)*a/b+1e-9))), true
}

// tenantCurrency is the tenant's own currency.
func (h *Handler) tenantCurrency(ctx context.Context, tid uuid.UUID) string {
	if h.RunAds != nil && h.RunAds.Currency != nil {
		if c := strings.ToUpper(strings.TrimSpace(h.RunAds.Currency(ctx, tid))); c != "" {
			return c
		}
	}
	return "KES"
}

// resultsNote says plainly where a platform's past results come from, or that there are none.
func resultsNote(platform string, past *runads.Past) string {
	switch {
	case platform == runads.TikTok && past == nil:
		return "TikTok results are not read yet, so there is no data for TikTok."
	case past == nil:
		return "No results from " + platformName(platform) + " yet, so there is no data for it."
	}
	return ""
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

// pastDays is how far back a plan reads past results.
const pastDays = 180

// pastResults sums the tenant's stored ad metrics on a platform over the last 180 days, in cur.
func (h *Handler) pastResults(ctx context.Context, tid uuid.UUID, platform, cur string) *runads.Past {
	return h.storedResults(ctx, tid, platform, cur, h.RunAds.now().AddDate(0, 0, -pastDays))
}

// monthSpend is the stored ad spend on a platform since the 1st (UTC), nil with no data.
func (h *Handler) monthSpend(ctx context.Context, tid uuid.UUID, platform, cur string) *int64 {
	now := h.RunAds.now().UTC()
	p := h.storedResults(ctx, tid, platform, cur, time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC))
	if p == nil {
		return nil
	}
	n := int64(math.Ceil(p.Spend))
	return &n
}

// storedResults sums the stored ad metrics since a day in cur (rows in another currency are
// converted, or left out without a rate). nil means nothing was spent or stored.
func (h *Handler) storedResults(ctx context.Context, tid uuid.UUID, platform, cur string, since time.Time) *runads.Past {
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
		pp.Setup = append(pp.Setup, PlanStep{Label: "Paid Meta ads are not switched on for MarketFlow yet"})
	}
	d := h.waBoostDefaults(ctx, tid)
	acc, _ := h.adPick(ctx, tid, adaccounts.Meta, d.AdAccountConnID)
	pages, _ := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("facebook_page"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	page := pickPage(pages, d.PageConnID)
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
	acc, _ := h.adPick(ctx, tid, adaccounts.TikTok, "")
	adv := ""
	if acc != nil {
		pc.tiktok = acc
		adv = acc.ExternalID
		pp.Account = &PlanAccount{ConnectedID: acc.ID.String(), ExternalID: acc.ExternalID, Name: acc.DisplayName, Currency: accountCurrency(acc)}
	} else {
		// No advertiser account: left out of the plan and its total (owner, 2026-11-25).
		pp.Setup = append(pp.Setup, PlanStep{Label: "Connect a TikTok for Business account (create one on TikTok first if you have none)", Path: "/settings/integrations"})
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
		PlanStep{Label: fmt.Sprintf("Create a campaign; in the ad group set a lifetime budget of %s %d (not a daily one, so the total holds), scheduled %s to %s, and upload the video",
			accCur, pp.AccountTotal, start.Format("2 Jan"), end.Format("2 Jan 2006")), URL: s.TikTokLink("campaign", adv)},
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
