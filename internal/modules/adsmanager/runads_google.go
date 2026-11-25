package adsmanager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	_ "image/gif"
	_ "image/png"

	"github.com/google/uuid"
	"go.uber.org/zap"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
	"github.com/bengobox/marketflow-api/internal/platform/adaccounts"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"
)

// Google Ads in Run ads: a Performance Max campaign on the business's own Google Ads account,
// with the approved video from the business's YouTube channel, a budget (daily, or a campaign
// total over the run when run_ads.google_budget_period is "total"), start and end dates, the
// tenant's link, headlines and descriptions from the ad copy, the business name, logo and brand
// pictures, and the account's own conversion goals (maximise conversions).

// googleTarget is what the Google part of a plan runs on.
type googleTarget struct {
	acc        *ent.ConnectedAccount
	ads        google.Ads
	customer   string
	currency   string
	timeZone   string
	youtubeIDs []string
	finalURL   string
	logoURL    string
	pictures   []string
}

// googleCaller builds a Google Ads caller for a connected customer with the platform client's
// settings and a fresh token.
func (h *Handler) googleCaller(ctx context.Context, a *ent.ConnectedAccount) (google.Ads, error) {
	d := h.RunAds
	if d == nil || d.GoogleTokens == nil || d.GoogleExtra == nil {
		return google.Ads{}, errGoogleOff
	}
	extra, err := d.GoogleExtra(ctx)
	if err != nil {
		return google.Ads{}, errGoogleOff
	}
	tok, err := d.GoogleTokens.AccessToken(ctx, a)
	if err != nil {
		return google.Ads{}, err
	}
	hc := d.GoogleHTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	ads := google.AdsFromSettings(hc, extra, tok)
	// The account was connected directly, so it is called as itself, as in googleads.Handler;
	// a customer reached through a manager carries login_customer_id on the connection.
	ads.LoginCustomerID = google.CustomerDigits(str(a.Metadata["login_customer_id"]))
	if d.GoogleBase != "" {
		ads.Base = d.GoogleBase
	}
	return ads, nil
}

var errGoogleOff = errors.New("google ads is not configured")

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func (h *Handler) planGoogle(ctx context.Context, tid uuid.UUID, pc *planCtx, pp *PlatformPlan, in *runads.TierInput, own int64, cur string) {
	pp.Mode = "api"
	gt := &googleTarget{}
	acc, _ := h.adPick(ctx, tid, adaccounts.Google, "")
	if acc == nil {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Connect your Google Ads account", Path: "/settings/integrations"})
	} else {
		gt.acc, gt.customer = acc, acc.ExternalID
		gt.currency, gt.timeZone = strings.ToUpper(str(acc.Metadata["currency"])), str(acc.Metadata["time_zone"])
		pp.Account = &PlanAccount{ConnectedID: acc.ID.String(), ExternalID: acc.ExternalID, Name: acc.DisplayName, Currency: gt.currency}
		if googleads.MappingOf(acc)["Lead"] == "" {
			pp.Notes = append(pp.Notes, "Google optimises for the account's own conversion goals. Map Lead to a conversion action in Settings, Integrations, Google Ads so it counts MarketFlow leads too.")
		}
	}
	// The video must be on the business's YouTube channel: Google Ads plays it from there.
	gt.youtubeIDs = h.youtubeIDs(ctx, tid, pc.version.ID)
	if len(gt.youtubeIDs) == 0 {
		yt, _ := h.DB.ConnectedAccount.Query().
			Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("youtube_channel"), connectedaccount.StatusEQ("active")).Exist(ctx)
		if yt {
			pp.Setup = append(pp.Setup, PlanStep{Label: "Post this video to your YouTube channel first; Google Ads plays it from there", Path: "/studio"})
		} else {
			pp.Setup = append(pp.Setup, PlanStep{Label: "Connect your YouTube channel and post this video to it; Google Ads plays it from there", Path: "/settings/integrations"})
		}
	}
	gt.finalURL = h.landingURL(ctx, pc)
	pp.Goal, pp.Link = "your account's conversion goals", gt.finalURL
	if gt.finalURL == "" {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Add your website link to your business profile", Path: "/settings/profile"})
	}
	gt.logoURL = shareddefaults.Str(pc.tenant, "logo_url")
	if !strings.HasPrefix(gt.logoURL, "https://") && !strings.HasPrefix(gt.logoURL, "http://") {
		gt.logoURL = ""
		pp.Setup = append(pp.Setup, PlanStep{Label: "Add your logo to your business profile (Google needs a square logo)", Path: "/settings/profile"})
	}
	if h.RunAds.BrandImages != nil {
		gt.pictures = h.RunAds.BrandImages(tid, brandimages.Keys(pc.tenant.Metadata, tid, 6))
	}
	if len(gt.pictures) == 0 {
		pp.Setup = append(pp.Setup, PlanStep{Label: "Add at least one brand picture (Google shows a landscape and a square picture with the video)", Path: "/settings/marketing-context"})
	}
	pc.google = gt
	if acc != nil {
		ads, err := h.googleCaller(ctx, acc)
		switch {
		case errors.Is(err, errGoogleOff):
			pp.Setup = append(pp.Setup, PlanStep{Label: "Google Ads is not configured on MarketFlow yet"})
		case err != nil:
			pp.Blockers = append(pp.Blockers, "Google ended this connection or did not renew it; connect Google Ads again")
		default:
			gt.ads = ads
			h.googleFunding(ctx, pc, pp)
			h.googleResults(ctx, gt, pp, in, cur)
			if gt.finalURL != "" {
				head, _, desc := googleTexts(pc, gt.finalURL)
				opts, err := ads.BudgetOptions(ctx, gt.customer, google.RecommendationInput{FinalURL: gt.finalURL, Headlines: head,
					Descriptions: desc, CountryCodes: countryCodes(pc.tenant)})
				if err == nil {
					for _, o := range opts {
						daily, ok := h.convert(ctx, o.DailyMicros/1_000_000, firstNonEmpty(gt.currency, cur), cur)
						if !ok || daily <= 0 {
							continue
						}
						est := []runads.Estimate{}
						for _, m := range []struct {
							name string
							v    int64
						}{{"impressions", o.Impressions}, {"clicks", o.Clicks}, {"conversions", o.Conversions}} {
							if m.v > 0 {
								est = append(est, runads.Estimate{Metric: m.name, Value: m.v, Per: "week", Source: runads.SourcePlatform, Label: "Google Ads budget recommendation"})
							}
						}
						in.Options = append(in.Options, runads.Option{Daily: daily, Recommended: o.Recommended, Estimates: est})
					}
					in.OptionsLabel = "Google Ads budget recommendation"
				} else {
					pp.Notes = append(pp.Notes, "Google gave no budget recommendation for this account, so the amounts below come from the next source.")
				}
			}
		}
	}
	pickDaily(pp, *in, own)
	pp.AccountDaily, pp.AccountTotal = pp.Daily, pp.Daily*int64(pp.Days)
	if gt.currency != "" && !strings.EqualFold(gt.currency, cur) && pp.Daily > 0 {
		d, ok := h.convert(ctx, pp.Daily, cur, gt.currency)
		if !ok {
			pp.Blockers = append(pp.Blockers, "the exchange rate for the Google Ads account's currency could not be read; try again shortly")
			return
		}
		pp.AccountDaily, pp.AccountTotal = d, d*int64(pp.Days)
	}
	if pc.settings.GoogleTotal {
		pp.Notes = append(pp.Notes, "Google gets the total for the whole run as its budget and paces it over the days.")
	} else {
		pp.Notes = append(pp.Notes, "Google may spend up to twice the daily budget on any day, so a short run can cost more than the total shown.")
	}
}

// googleResults reads the account's own campaign results from Google (every campaign on it, not
// only MarketFlow's) for the estimates and this month's spend. A read that fails says so and
// keeps what is stored.
func (h *Handler) googleResults(ctx context.Context, gt *googleTarget, pp *PlatformPlan, in *runads.TierInput, cur string) {
	from := firstNonEmpty(gt.currency, cur)
	a, ok1 := h.kesPer(ctx, from)
	b, ok2 := h.kesPer(ctx, cur)
	if !ok1 || !ok2 {
		return
	}
	sum := func(rows []google.CampaignResult) *runads.Past {
		p := &runads.Past{}
		for _, r := range rows {
			p.Spend += r.Cost * a / b
			p.Impressions += r.Impressions
			p.Clicks += r.Clicks
			p.Leads += int64(r.Conversions)
		}
		if p.Spend <= 0 {
			return nil
		}
		return p
	}
	now := h.RunAds.now()
	rows, err := gt.ads.CampaignResultsBetween(ctx, gt.customer, now.AddDate(0, 0, -pastDays), now)
	if err != nil {
		h.Logger.Debug("google campaign results", zap.Error(err))
		pp.ResultsNote = "Google did not answer about past results just now."
		return
	}
	in.Past = sum(rows)
	rows, err = gt.ads.CampaignResults(ctx, gt.customer, "THIS_MONTH")
	if err != nil {
		return
	}
	var spent int64
	if p := sum(rows); p != nil {
		spent = int64(math.Ceil(p.Spend))
	}
	pp.MonthSpend = &spent
}

// googleFunding reads whether the account can spend.
func (h *Handler) googleFunding(ctx context.Context, pc *planCtx, pp *PlatformPlan) {
	gt := pc.google
	link := pc.settings.GoogleBillingLink(gt.customer)
	st, err := gt.ads.SpendStatus(ctx, gt.customer)
	yes, no := true, false
	var ae *google.AdsError
	switch {
	case errors.As(err, &ae) && ae.Code == google.ErrCustomerNotEnabled:
		pp.Funding = FundingView{CanSpend: &no, Status: "not_enabled", Link: link,
			Detail: "This Google Ads account is not set up yet. Finish billing on Google Ads (sign in and pick account " + gt.customer + ")."}
	case err != nil:
		pp.Funding = FundingView{Status: "unknown", Link: link, Detail: "Google did not say whether this account can spend; check billing on Google Ads."}
	case st.CanSpend:
		pp.Funding = FundingView{CanSpend: &yes, Status: "approved", Link: link, Detail: "Billing is set up on Google Ads."}
	case st.CustomerStatus != "" && st.CustomerStatus != "ENABLED":
		pp.Funding = FundingView{CanSpend: &no, Status: strings.ToLower(st.CustomerStatus), Link: link,
			Detail: "Google lists this account as " + strings.ToLower(st.CustomerStatus) + "; it cannot run ads."}
		pp.Blockers = append(pp.Blockers, "the Google Ads account is "+strings.ToLower(st.CustomerStatus))
	default:
		status := strings.ToLower(st.Billing)
		if status == "" {
			status = "no_billing"
		}
		pp.Funding = FundingView{CanSpend: &no, Status: status, Link: link,
			Detail: "Add a payment method on Google Ads (sign in and pick account " + gt.customer + "); the campaign waits until billing is approved."}
	}
}

// youtubeIDs are the YouTube ids of the version's videos posted to YouTube.
func (h *Handler) youtubeIDs(ctx context.Context, tid, vid uuid.UUID) []string {
	assets, err := h.DB.CreativeAsset.Query().Where(creativeasset.VersionIDEQ(vid), creativeasset.TenantIDEQ(tid)).IDs(ctx)
	if err != nil || len(assets) == 0 {
		return nil
	}
	jobs, err := h.DB.PublishJob.Query().
		Where(publishjob.TenantIDEQ(tid), publishjob.AssetIDIn(assets...), publishjob.PlatformEQ("youtube"), publishjob.StatusEQ("published")).
		Limit(3).All(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, j := range jobs {
		if j.ExternalID != "" {
			out = append(out, j.ExternalID)
		}
	}
	return out
}

// landingURL is the ad's link: the ad copy's own link, else the ad's lead page, else the business
// website, else the tenant's MarketFlow public page.
func (h *Handler) landingURL(ctx context.Context, pc *planCtx) string {
	for _, u := range []string{str(pc.version.Copy["link"]), h.adLink(ctx, pc.version, "google"), str(pc.tenant.Metadata["website"]), str(pc.tenant.Metadata["website_url"])} {
		if p, err := url.Parse(u); err == nil && (p.Scheme == "https" || p.Scheme == "http") && p.Host != "" {
			return u
		}
	}
	if h.RunAds.PublicPage != nil {
		return h.RunAds.PublicPage(pc.tenant.Slug)
	}
	return ""
}

func countryCodes(t *ent.Tenant) []string {
	if t.Country != nil && *t.Country != "" {
		return []string{strings.ToUpper(*t.Country)}
	}
	return []string{"KE"}
}

// googleTexts are the headlines (30), long headlines (90) and descriptions (90) from the ad copy
// and the business's own facts; nothing is made up.
func googleTexts(pc *planCtx, finalURL string) (head, long, desc []string) {
	c := pc.version.Copy
	name := strings.TrimSpace(pc.tenant.Name)
	host := ""
	if p, err := url.Parse(finalURL); err == nil {
		host = strings.TrimPrefix(p.Host, "www.")
	}
	var captions []string
	if sc, ok := c["script"].(map[string]any); ok {
		if scenes, ok := sc["scenes"].([]any); ok {
			for _, s := range scenes {
				if m, ok := s.(map[string]any); ok {
					captions = append(captions, str(m["caption"]))
				}
			}
		}
	}
	add := func(list []string, max int, cands ...string) []string {
		for _, s := range cands {
			s = clip(strings.Join(strings.Fields(s), " "), max)
			if s == "" {
				continue
			}
			dup := false
			for _, x := range list {
				if strings.EqualFold(x, s) {
					dup = true
				}
			}
			if !dup {
				list = append(list, s)
			}
		}
		return list
	}
	head = add(nil, 30, append([]string{str(c["headline"]), str(c["subline"]), str(c["cta"]), name}, captions...)...)
	if host != "" {
		head = add(head, 30, "Visit "+host)
	}
	if len(head) > 15 {
		head = head[:15]
	}
	long = add(nil, 90, strings.TrimSpace(str(c["headline"])+" "+str(c["subline"])), str(c["headline"]), name)
	if len(long) > 1 {
		long = long[:1]
	}
	desc = add(nil, 90, str(c["subline"]), shareddefaults.Str(pc.tenant, "tagline"), strings.Join(captions, " "), str(c["cta"]))
	if name != "" {
		desc = add(desc, 90, "Contact "+name+" to find out more.")
	}
	if len(desc) > 5 {
		desc = desc[:5]
	}
	return head, long, desc
}

// clip cuts s to max runes on a word boundary.
func clip(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)[:max]
	cut := string(r)
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:-")
}

// --- launch ---

// PlatformResult is what running one platform of a confirmed plan did.
type PlatformResult struct {
	Platform   string     `json:"platform"`
	Status     string     `json:"status"` // created, guided, failed, skipped
	CampaignID string     `json:"campaign_id,omitempty"`
	External   string     `json:"external_id,omitempty"`
	Message    string     `json:"message"`
	Steps      []PlanStep `json:"steps,omitempty"`
}

// launchGoogle creates the campaign once per confirmed plan: the MarketFlow campaign row is keyed
// by the run, and the Google campaign carries the run in its name and is looked up before the
// create, so a retry never makes a second campaign.
func (h *Handler) launchGoogle(ctx context.Context, tid uuid.UUID, pc *planCtx, pp PlatformPlan, runKey string) PlatformResult {
	res := PlatformResult{Platform: runads.Google}
	gt := pc.google
	if gt == nil || gt.acc == nil || gt.ads.AccessToken == "" {
		res.Status, res.Message = "failed", "the Google Ads account is not ready"
		return res
	}
	row, err := h.runRow(ctx, tid, pc, pp, runKey, "google")
	if err != nil {
		res.Status, res.Message = "failed", "the campaign could not be recorded"
		return res
	}
	res.CampaignID = row.ID.String()
	if rn := str(nested(row.Metadata, "google")["campaign_resource"]); rn != "" {
		res.Status, res.External, res.Message = "created", rn, "The Google Ads campaign was already created for this plan."
		return res
	}
	name := clip("MarketFlow "+firstNonEmpty(str(pc.version.Copy["headline"]), pc.tenant.Name), 80) + " " + runKey[:8]
	fail := func(msg string) PlatformResult {
		h.saveRunRow(ctx, row.ID, CampaignFailed, map[string]any{"error": msg, "name": name})
		res.Status, res.Message = "failed", msg
		return res
	}
	rn, err := gt.ads.FindCampaign(ctx, gt.customer, name)
	if err != nil {
		// Without this answer a create could be a second campaign.
		return fail("Google Ads did not answer; try again shortly")
	}
	if rn != "" {
		h.saveRunRow(ctx, row.ID, CampaignActive, map[string]any{"campaign_resource": rn, "customer_id": gt.customer, "name": name})
		res.Status, res.External, res.Message = "created", rn, "The Google Ads campaign was already created for this plan."
		return res
	}
	logo, err := h.fetchImage(ctx, gt.logoURL, 1, 128, 128)
	if err != nil {
		return fail("the logo could not be used for Google (" + err.Error() + ")")
	}
	var landscape, square []byte
	for _, u := range gt.pictures {
		if landscape == nil {
			landscape, _ = h.fetchImage(ctx, u, 1.91, 600, 314)
		}
		if square == nil {
			square, _ = h.fetchImage(ctx, u, 1, 300, 300)
		}
	}
	if landscape == nil || square == nil {
		return fail("no brand picture is large enough for Google (at least 600 by 314 pixels); add a bigger one")
	}
	head, long, desc := googleTexts(pc, gt.finalURL)
	loc := time.UTC
	if l, err := time.LoadLocation(firstNonEmpty(gt.timeZone, "Africa/Nairobi")); err == nil {
		loc = l
	}
	start := h.RunAds.now().In(loc)
	spec := google.PMaxSpec{Name: name, Start: start, End: start.AddDate(0, 0, pp.Days-1), Status: pc.settings.GoogleStatus,
		FinalURL: gt.finalURL, Headlines: head, LongHeadlines: long, Descriptions: desc, BusinessName: clip(pc.tenant.Name, 25),
		Logo: logo, Landscape: landscape, Square: square, YouTubeIDs: gt.youtubeIDs}
	if pc.settings.GoogleTotal {
		spec.TotalMicros = pp.AccountTotal * 1_000_000
	} else {
		spec.DailyMicros = pp.AccountDaily * 1_000_000
	}
	if g := google.CountryGeoTarget(countryCodes(pc.tenant)[0]); g > 0 {
		spec.GeoTargets = []int64{g}
	}
	if miss := spec.Check(); len(miss) > 0 {
		return fail("Google needs " + strings.Join(miss, ", "))
	}
	rn, err = gt.ads.CreatePMax(ctx, gt.customer, spec)
	if err != nil {
		var ae *google.AdsError
		if errors.As(err, &ae) {
			return fail("Google Ads refused the campaign: " + ae.Message)
		}
		h.Logger.Warn("google ads campaign create", zap.Error(err))
		return fail("Google Ads did not answer; try again shortly")
	}
	h.saveRunRow(ctx, row.ID, CampaignActive, map[string]any{"campaign_resource": rn, "customer_id": gt.customer, "name": name,
		"created_at": h.RunAds.now().UTC().Format(time.RFC3339)})
	res.Status, res.External = "created", rn
	res.Message = "Google Ads campaign created. Google reviews the ads, usually within a day, and they run once billing is set up."
	return res
}

func nested(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}

// runRow finds or makes the MarketFlow campaign row of one platform of one confirmed plan.
func (h *Handler) runRow(ctx context.Context, tid uuid.UUID, pc *planCtx, pp PlatformPlan, runKey, platform string) (*ent.Campaign, error) {
	rows, err := h.DB.Campaign.Query().
		Where(campaign.TenantIDEQ(tid), campaign.PlatformEQ(platform), campaign.CreativeVersionIDEQ(pc.version.ID)).
		Order(ent.Desc(campaign.FieldCreatedAt)).Limit(20).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if str(r.Metadata["run_key"]) == runKey {
			return r, nil
		}
	}
	start := h.RunAds.now().UTC()
	q := h.DB.Campaign.Create().SetTenantID(tid).SetName(clip("Run ads "+platform+" "+start.Format("2 Jan 2006"), 120)).
		SetPlatform(platform).SetStatus("draft").SetCampaignType("lead_gen").
		SetBudgetDaily(float64(pp.Daily)).SetBudgetTotal(float64(pp.Total)).SetCurrency(pc.cur).
		SetStartDate(start).SetEndDate(start.AddDate(0, 0, pp.Days)).SetCreativeVersionID(pc.version.ID).
		SetMetadata(map[string]any{"run_key": runKey, "run_ads": pp})
	if pp.Account != nil {
		if id, err := uuid.Parse(pp.Account.ConnectedID); err == nil {
			q.SetConnectedAccountID(id)
		}
	}
	return q.Save(ctx)
}

// saveRunRow sets the row's status and merges detail into metadata[platform key].
func (h *Handler) saveRunRow(ctx context.Context, id uuid.UUID, status string, detail map[string]any) {
	row, err := h.DB.Campaign.Get(ctx, id)
	if err != nil {
		return
	}
	md := map[string]any{}
	for k, v := range row.Metadata {
		md[k] = v
	}
	key := row.Platform
	cur := nested(md, key)
	for k, v := range detail {
		cur[k] = v
	}
	md[key] = cur
	if err := h.DB.Campaign.UpdateOneID(id).SetStatus(status).SetMetadata(md).Exec(ctx); err != nil {
		h.Logger.Warn("run ads row not saved", zap.Error(err))
	}
}

// fetchImage downloads a public image, crops its centre to ratio (width/height), checks the
// minimum size, scales it down to at most 1200 pixels wide and returns a JPEG.
func (h *Handler) fetchImage(ctx context.Context, src string, ratio float64, minW, minH int) ([]byte, error) {
	if src == "" {
		return nil, fmt.Errorf("no image")
	}
	hc := h.RunAds.Fetch
	if hc == nil {
		return nil, fmt.Errorf("images cannot be fetched on this server")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the image could not be downloaded")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the image could not be downloaded")
	}
	img, _, err := image.Decode(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return nil, fmt.Errorf("the image format is not readable")
	}
	return cropJPEG(img, ratio, minW, minH)
}

// cropJPEG is the centre crop of img at ratio, scaled to at most 1200 wide, as JPEG.
func cropJPEG(img image.Image, ratio float64, minW, minH int) ([]byte, error) {
	b := img.Bounds()
	w, hgt := b.Dx(), b.Dy()
	cw, ch := w, int(float64(w)/ratio)
	if ch > hgt {
		ch, cw = hgt, int(float64(hgt)*ratio)
	}
	if cw < minW || ch < minH {
		return nil, fmt.Errorf("the image is too small (needs %d by %d pixels)", minW, minH)
	}
	x0, y0 := b.Min.X+(w-cw)/2, b.Min.Y+(hgt-ch)/2
	crop := image.Rect(x0, y0, x0+cw, y0+ch)
	outW, outH := cw, ch
	if outW > 1200 {
		outW, outH = 1200, int(1200/ratio)
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, crop, xdraw.Src, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 88}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
