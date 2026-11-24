package google

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Campaign creation and the reads Run ads needs (owner 2026-11-24): whether the account can
// spend, Google's own budget options for a Performance Max campaign, and one atomic
// googleAds:mutate that makes the budget, the campaign, its location, the assets and the asset
// group together. Contracts: developers.google.com/google-ads/api/docs/performance-max
// (create-campaign, asset-requirements, read 2026-11-24). The REST shapes below are written from
// those pages and the field names of v26; they are tested against a fake server only, so the
// first real run is watched (marketflow/docs/integrations/google-ads-campaigns.md).

// SpendStatus is whether a customer can spend: the account's own status and its billing setup.
type SpendStatus struct {
	// CustomerStatus is ENABLED, CANCELED, SUSPENDED or CLOSED ("" when unread).
	CustomerStatus string `json:"customer_status"`
	// Billing is the newest billing setup's status: APPROVED, APPROVED_HELD, PENDING, CANCELLED,
	// or "" when there is none.
	Billing  string `json:"billing"`
	CanSpend bool   `json:"can_spend"`
}

// SpendStatus reads the customer status and billing setups.
func (a Ads) SpendStatus(ctx context.Context, customerID string) (SpendStatus, error) {
	var st SpendStatus
	rows, err := a.search(ctx, customerID, "SELECT customer.status FROM customer LIMIT 1")
	if err != nil {
		return st, err
	}
	if len(rows) > 0 {
		m, _ := rows[0]["customer"].(map[string]any)
		st.CustomerStatus, _ = m["status"].(string)
	}
	rows, err = a.search(ctx, customerID, "SELECT billing_setup.id, billing_setup.status FROM billing_setup")
	if err != nil {
		return st, err
	}
	for _, r := range rows {
		m, _ := r["billingSetup"].(map[string]any)
		s, _ := m["status"].(string)
		// APPROVED wins over any other setup; otherwise the last one read is shown.
		if st.Billing != "APPROVED" {
			st.Billing = s
		}
	}
	st.CanSpend = st.CustomerStatus == "ENABLED" && st.Billing == "APPROVED"
	return st, nil
}

// BudgetOption is one of Google's budget options for a new campaign: the daily amount and the
// weekly results Google estimates for it.
type BudgetOption struct {
	DailyMicros int64 `json:"daily_micros"`
	Recommended bool  `json:"recommended"`
	Impressions int64 `json:"impressions"`
	Clicks      int64 `json:"clicks"`
	Conversions int64 `json:"conversions"`
}

// RecommendationInput describes the campaign about to be made, for Google's budget options.
type RecommendationInput struct {
	FinalURL     string
	Headlines    []string
	Descriptions []string
	CountryCodes []string
}

// BudgetOptions asks RecommendationService.GenerateRecommendations for CAMPAIGN_BUDGET on a
// Performance Max campaign that does not exist yet. Google may return nothing when it has too
// little data; that is no options and no error.
func (a Ads) BudgetOptions(ctx context.Context, customerID string, in RecommendationInput) ([]BudgetOption, error) {
	body := map[string]any{
		"recommendationTypes":    []string{"CAMPAIGN_BUDGET"},
		"advertisingChannelType": "PERFORMANCE_MAX",
		"biddingInfo":            map[string]any{"biddingStrategyType": "MAXIMIZE_CONVERSIONS"},
		"assetGroupInfo":         []map[string]any{{"finalUrl": in.FinalURL, "headline": in.Headlines, "description": in.Descriptions}},
	}
	if len(in.CountryCodes) > 0 {
		body["countryCodes"] = in.CountryCodes
	}
	var out struct {
		Recommendations []struct {
			Budget *struct {
				Recommended any `json:"recommendedBudgetAmountMicros"`
				Options     []struct {
					Amount any `json:"budgetAmountMicros"`
					Impact struct {
						Potential map[string]any `json:"potentialMetrics"`
					} `json:"impact"`
				} `json:"budgetOptions"`
			} `json:"campaignBudgetRecommendation"`
		} `json:"recommendations"`
	}
	if err := a.do(ctx, http.MethodPost, "/customers/"+CustomerDigits(customerID)+"/recommendations:generate", body, &out); err != nil {
		return nil, err
	}
	var opts []BudgetOption
	for _, r := range out.Recommendations {
		if r.Budget == nil {
			continue
		}
		rec := int64Of(r.Budget.Recommended)
		for _, o := range r.Budget.Options {
			amt := int64Of(o.Amount)
			if amt <= 0 {
				continue
			}
			opts = append(opts, BudgetOption{DailyMicros: amt, Recommended: amt == rec,
				Impressions: int64Of(o.Impact.Potential["impressions"]), Clicks: int64Of(o.Impact.Potential["clicks"]),
				Conversions: int64Of(o.Impact.Potential["conversions"])})
		}
	}
	return opts, nil
}

// FindCampaign returns the resource name of the non-removed campaign called name, or "".
func (a Ads) FindCampaign(ctx context.Context, customerID, name string) (string, error) {
	q := "SELECT campaign.resource_name FROM campaign WHERE campaign.name = '" + gaqlQuote(name) + "' AND campaign.status != 'REMOVED' LIMIT 1"
	rows, err := a.search(ctx, customerID, q)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	m, _ := rows[0]["campaign"].(map[string]any)
	rn, _ := m["resourceName"].(string)
	return rn, nil
}

func gaqlQuote(s string) string {
	return strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(s)
}

// PMaxSpec is one Performance Max campaign. Text limits are Google's (headline 30, long headline
// and description 90, business name 25 characters); the caller trims.
type PMaxSpec struct {
	Name string
	// DailyMicros is the daily budget; with TotalMicros > 0 the budget is a campaign total over
	// the custom period from Start to End instead.
	DailyMicros   int64
	TotalMicros   int64
	Start, End    time.Time // dates in the account's time zone; End's day is the last day
	Status        string    // ENABLED or PAUSED
	FinalURL      string
	Headlines     []string // 3 to 15
	LongHeadlines []string // 1 to 5
	Descriptions  []string // 2 to 5
	BusinessName  string
	Logo          []byte // 1:1, at least 128x128
	Landscape     []byte // 1.91:1, at least 600x314
	Square        []byte // 1:1, at least 300x300
	YouTubeIDs    []string
	// GeoTargets are geo target constant ids (2404 is Kenya); none targets everywhere.
	GeoTargets []int64
}

// Check reports what the spec still lacks for Google, in plain words.
func (s PMaxSpec) Check() []string {
	var miss []string
	if len(s.Headlines) < 3 {
		miss = append(miss, "three short headlines")
	}
	if len(s.LongHeadlines) < 1 {
		miss = append(miss, "a long headline")
	}
	if len(s.Descriptions) < 2 {
		miss = append(miss, "two descriptions")
	}
	if strings.TrimSpace(s.BusinessName) == "" {
		miss = append(miss, "the business name")
	}
	if len(s.Logo) == 0 {
		miss = append(miss, "a logo")
	}
	if len(s.Landscape) == 0 || len(s.Square) == 0 {
		miss = append(miss, "a landscape and a square picture")
	}
	if !strings.HasPrefix(s.FinalURL, "https://") && !strings.HasPrefix(s.FinalURL, "http://") {
		miss = append(miss, "a website link")
	}
	if s.DailyMicros <= 0 && s.TotalMicros <= 0 {
		miss = append(miss, "a budget")
	}
	return miss
}

// CreatePMax makes the campaign in one googleAds:mutate (all or nothing, temporary negative ids)
// and returns the campaign's resource name.
func (a Ads) CreatePMax(ctx context.Context, customerID string, s PMaxSpec) (string, error) {
	if miss := s.Check(); len(miss) > 0 {
		return "", fmt.Errorf("the campaign needs %s", strings.Join(miss, ", "))
	}
	cid := CustomerDigits(customerID)
	rn := func(kind string, id int) string { return "customers/" + cid + "/" + kind + "/" + strconv.Itoa(id) }
	budgetRN, campaignRN, groupRN := rn("campaignBudgets", -1), rn("campaigns", -2), rn("assetGroups", -3)

	budget := map[string]any{"resourceName": budgetRN, "name": s.Name + " budget", "deliveryMethod": "STANDARD", "explicitlyShared": false}
	if s.TotalMicros > 0 {
		budget["period"] = "CUSTOM_PERIOD"
		budget["totalAmountMicros"] = strconv.FormatInt(s.TotalMicros, 10)
	} else {
		budget["period"] = "DAILY"
		budget["amountMicros"] = strconv.FormatInt(s.DailyMicros, 10)
	}
	status := s.Status
	if status != "PAUSED" {
		status = "ENABLED"
	}
	ops := []map[string]any{
		{"campaignBudgetOperation": map[string]any{"create": budget}},
		{"campaignOperation": map[string]any{"create": map[string]any{
			"resourceName": campaignRN, "name": s.Name, "status": status,
			"advertisingChannelType": "PERFORMANCE_MAX", "campaignBudget": budgetRN,
			"maximizeConversions":            map[string]any{},
			"brandGuidelinesEnabled":         false,
			"containsEuPoliticalAdvertising": "DOES_NOT_CONTAIN_EU_POLITICAL_ADVERTISING",
			"startDateTime":                  s.Start.Format("2006-01-02") + " 00:00:00",
			"endDateTime":                    s.End.Format("2006-01-02") + " 23:59:59",
		}}},
	}
	for _, g := range s.GeoTargets {
		ops = append(ops, map[string]any{"campaignCriterionOperation": map[string]any{"create": map[string]any{
			"campaign": campaignRN, "location": map[string]any{"geoTargetConstant": "geoTargetConstants/" + strconv.FormatInt(g, 10)}}}})
	}
	// The asset group comes before its asset links; the assets before both.
	type link struct {
		asset string
		field string
	}
	var assets []map[string]any
	var links []link
	next := -10
	add := func(field string, create map[string]any) {
		id := rn("assets", next)
		next--
		create["resourceName"] = id
		assets = append(assets, map[string]any{"assetOperation": map[string]any{"create": create}})
		links = append(links, link{id, field})
	}
	for _, t := range s.Headlines {
		add("HEADLINE", map[string]any{"textAsset": map[string]any{"text": t}})
	}
	for _, t := range s.LongHeadlines {
		add("LONG_HEADLINE", map[string]any{"textAsset": map[string]any{"text": t}})
	}
	for _, t := range s.Descriptions {
		add("DESCRIPTION", map[string]any{"textAsset": map[string]any{"text": t}})
	}
	add("BUSINESS_NAME", map[string]any{"textAsset": map[string]any{"text": s.BusinessName}})
	add("LOGO", map[string]any{"name": s.Name + " logo", "type": "IMAGE", "imageAsset": map[string]any{"data": s.Logo}})
	add("MARKETING_IMAGE", map[string]any{"name": s.Name + " landscape", "type": "IMAGE", "imageAsset": map[string]any{"data": s.Landscape}})
	add("SQUARE_MARKETING_IMAGE", map[string]any{"name": s.Name + " square", "type": "IMAGE", "imageAsset": map[string]any{"data": s.Square}})
	for _, v := range s.YouTubeIDs {
		add("YOUTUBE_VIDEO", map[string]any{"name": s.Name + " video " + v, "youtubeVideoAsset": map[string]any{"youtubeVideoId": v}})
	}
	ops = append(ops, assets...)
	ops = append(ops, map[string]any{"assetGroupOperation": map[string]any{"create": map[string]any{
		"resourceName": groupRN, "campaign": campaignRN, "name": s.Name, "finalUrls": []string{s.FinalURL}, "status": "ENABLED"}}})
	for _, l := range links {
		ops = append(ops, map[string]any{"assetGroupAssetOperation": map[string]any{"create": map[string]any{
			"assetGroup": groupRN, "asset": l.asset, "fieldType": l.field}}})
	}
	var out struct {
		Responses []map[string]map[string]any `json:"mutateOperationResponses"`
	}
	if err := a.do(ctx, http.MethodPost, "/customers/"+cid+"/googleAds:mutate", map[string]any{"mutateOperations": ops}, &out); err != nil {
		return "", err
	}
	for _, r := range out.Responses {
		if c, ok := r["campaignResult"]; ok {
			if s, _ := c["resourceName"].(string); s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("google ads: the campaign was not in the mutate response")
}

// CountryGeoTarget is Google's geo target constant for a country: 2000 plus the ISO 3166-1
// numeric code (Kenya 404 is 2404). Only the countries MarketFlow serves are listed.
func CountryGeoTarget(country string) int64 {
	numeric := map[string]int64{"KE": 404, "UG": 800, "TZ": 834, "RW": 646, "NG": 566, "GH": 288, "ZA": 710,
		"ET": 231, "US": 840, "GB": 826, "AE": 784, "IN": 356}
	if n, ok := numeric[strings.ToUpper(strings.TrimSpace(country))]; ok {
		return 2000 + n
	}
	return 0
}
