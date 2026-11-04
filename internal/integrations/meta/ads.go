package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Paid ads on the client's own ad account (Sprint 18). Contracts, and what is not confirmed:
// marketflow/docs/integrations/meta-paid-ads-apis.md. Every object is created PAUSED and only set
// ACTIVE after the owner's confirmation; ad money never passes through MarketFlow.

// TimezoneNairobi is Meta's timezone_id for Africa/Nairobi (TZ_AFRICA_NAIROBI).
const TimezoneNairobi = 78

// AdAccount is the health and funding view of an ad account.
type AdAccount struct {
	ID              string `json:"id"` // act_...
	AccountID       string `json:"account_id"`
	Name            string `json:"name"`
	AccountStatus   int    `json:"account_status"`
	DisableReason   int    `json:"disable_reason"`
	Currency        string `json:"currency"`
	TimezoneName    string `json:"timezone_name"`
	AmountSpent     string `json:"amount_spent"`
	SpendCap        string `json:"spend_cap"`
	Balance         string `json:"balance"`
	IsPrepayAccount bool   `json:"is_prepay_account"`
	FundingSource   string `json:"funding_source"`
	MinDailyBudget  int64  `json:"min_daily_budget"`
	FundingDetails  *struct {
		ID     string `json:"id"`
		Type   int    `json:"type"`
		Amount string `json:"amount"`
	} `json:"funding_source_details"`
}

// Account status values that matter here.
const (
	AccountActive = 1
	// FundingStoredBalance is funding_source_details.type for a prepaid stored balance.
	FundingStoredBalance = 20
)

const adAccountFields = "id,account_id,name,account_status,disable_reason,currency,timezone_name,amount_spent,spend_cap,balance,is_prepay_account,funding_source,funding_source_details,min_daily_budget"

// AdAccounts lists the ad accounts the token can use (GET /me/adaccounts).
func (c *Client) AdAccounts(ctx context.Context, token string) ([]AdAccount, error) {
	q := url.Values{"fields": {adAccountFields}, "limit": {"50"}, "access_token": {token}}
	var out struct {
		Data []AdAccount `json:"data"`
	}
	if err := c.getJSON(ctx, "/me/adaccounts?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("meta ad accounts: %w", err)
	}
	return out.Data, nil
}

// AdAccountHealth reads one ad account.
func (c *Client) AdAccountHealth(ctx context.Context, token, actID string) (*AdAccount, error) {
	q := url.Values{"fields": {adAccountFields}, "access_token": {token}}
	var a AdAccount
	if err := c.getJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"?"+q.Encode(), &a); err != nil {
		return nil, fmt.Errorf("meta ad account: %w", err)
	}
	return &a, nil
}

// CreateKESAdAccount makes a KES ad account in the client's own business portfolio
// (POST /{business_id}/adaccount), Nairobi time, with the client's Page as end advertiser.
func (c *Client) CreateKESAdAccount(ctx context.Context, token, businessID, name, pageID string) (string, error) {
	form := url.Values{
		"name": {name}, "currency": {"KES"}, "timezone_id": {strconv.Itoa(TimezoneNairobi)},
		"end_advertiser": {firstOr(pageID, "NONE")}, "media_agency": {"NONE"}, "partner": {"NONE"},
		"access_token": {token},
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(businessID)+"/adaccount", form, &out); err != nil {
		return "", fmt.Errorf("meta create ad account: %w", err)
	}
	return out.ID, nil
}

// Business is a business portfolio the user can manage.
type Business struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Businesses lists the user's business portfolios (GET /me/businesses, business_management).
func (c *Client) Businesses(ctx context.Context, token string) ([]Business, error) {
	q := url.Values{"fields": {"id,name"}, "limit": {"25"}, "access_token": {token}}
	var out struct {
		Data []Business `json:"data"`
	}
	if err := c.getJSON(ctx, "/me/businesses?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("meta businesses: %w", err)
	}
	return out.Data, nil
}

// AdSetInput is a lifetime-budget ad set; budgets are in the currency's minor unit.
type AdSetInput struct {
	Name             string
	CampaignID       string
	LifetimeBudget   int64
	Start, End       time.Time
	OptimizationGoal string // CONVERSATIONS (click to WhatsApp), LEAD_GENERATION, LINK_CLICKS
	DestinationType  string // WHATSAPP, or empty
	PageID           string
	WhatsAppNumber   string
	Countries        []string
	AgeMin, AgeMax   int
}

// CreateAdSet creates a PAUSED ad set. lifetime_budget always travels with end_time.
func (c *Client) CreateAdSet(ctx context.Context, token, actID string, in AdSetInput) (string, error) {
	if in.LifetimeBudget <= 0 || in.End.IsZero() || !in.End.After(in.Start) {
		return "", fmt.Errorf("meta ad set: a lifetime budget needs a later end time")
	}
	geo := map[string]any{"countries": in.Countries}
	targeting := map[string]any{"geo_locations": geo}
	if in.AgeMin > 0 {
		targeting["age_min"] = in.AgeMin
	}
	if in.AgeMax > 0 {
		targeting["age_max"] = in.AgeMax
	}
	promoted := map[string]any{"page_id": in.PageID}
	if in.WhatsAppNumber != "" {
		promoted["whatsapp_phone_number"] = in.WhatsAppNumber
	}
	tj, _ := json.Marshal(targeting)
	pj, _ := json.Marshal(promoted)
	form := url.Values{
		"name": {in.Name}, "campaign_id": {in.CampaignID},
		"lifetime_budget": {strconv.FormatInt(in.LifetimeBudget, 10)},
		"start_time":      {in.Start.UTC().Format(time.RFC3339)}, "end_time": {in.End.UTC().Format(time.RFC3339)},
		"billing_event": {"IMPRESSIONS"}, "bid_strategy": {"LOWEST_COST_WITHOUT_CAP"},
		"optimization_goal": {in.OptimizationGoal}, "targeting": {string(tj)}, "promoted_object": {string(pj)},
		"status": {"PAUSED"}, "access_token": {token},
	}
	if in.DestinationType != "" {
		form.Set("destination_type", in.DestinationType)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/adsets", form, &out); err != nil {
		return "", fmt.Errorf("meta ad set: %w", err)
	}
	return out.ID, nil
}

// UploadAdVideo hands Meta the approved asset's public URL (POST /act_{id}/advideos file_url).
func (c *Client) UploadAdVideo(ctx context.Context, token, actID, fileURL, title string) (string, error) {
	form := url.Values{"file_url": {fileURL}, "title": {truncate(title, 250)}, "access_token": {token}}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/advideos", form, &out); err != nil {
		return "", fmt.Errorf("meta ad video: %w", err)
	}
	return out.ID, nil
}

// CreativeInput is a video ad creative.
type CreativeInput struct {
	Name, PageID, InstagramUserID string
	VideoID, ThumbnailURL         string
	Message, Title                string
	// CTA is WHATSAPP_MESSAGE (click to WhatsApp), LEARN_MORE or SIGN_UP with Link.
	CTA, Link      string
	WelcomeMessage string
}

// CreateAdCreative creates the creative (POST /act_{id}/adcreatives).
func (c *Client) CreateAdCreative(ctx context.Context, token, actID string, in CreativeInput) (string, error) {
	cta := map[string]any{"type": in.CTA}
	if in.CTA == "WHATSAPP_MESSAGE" {
		cta["value"] = map[string]any{"app_destination": "WHATSAPP"}
	} else if in.Link != "" {
		cta["value"] = map[string]any{"link": in.Link}
	}
	video := map[string]any{"video_id": in.VideoID, "message": in.Message, "title": in.Title, "call_to_action": cta}
	if in.ThumbnailURL != "" {
		video["image_url"] = in.ThumbnailURL
	}
	spec := map[string]any{"page_id": in.PageID, "video_data": video}
	if in.InstagramUserID != "" {
		spec["instagram_user_id"] = in.InstagramUserID
	}
	if in.WelcomeMessage != "" {
		spec["page_welcome_message"] = in.WelcomeMessage
	}
	sj, _ := json.Marshal(spec)
	form := url.Values{"name": {in.Name}, "object_story_spec": {string(sj)}, "access_token": {token}}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/adcreatives", form, &out); err != nil {
		return "", fmt.Errorf("meta ad creative: %w", err)
	}
	return out.ID, nil
}

// CreateAd creates a PAUSED ad; Meta reviews it whatever its status.
func (c *Client) CreateAd(ctx context.Context, token, actID, name, adSetID, creativeID string) (string, error) {
	cj, _ := json.Marshal(map[string]string{"creative_id": creativeID})
	form := url.Values{"name": {name}, "adset_id": {adSetID}, "creative": {string(cj)}, "status": {"PAUSED"}, "access_token": {token}}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/ads", form, &out); err != nil {
		return "", fmt.Errorf("meta ad: %w", err)
	}
	return out.ID, nil
}

// SetStatus sets ACTIVE or PAUSED on a campaign, ad set or ad (POST /{id} status=).
func (c *Client) SetStatus(ctx context.Context, token, objectID, status string) error {
	if status != "ACTIVE" && status != "PAUSED" {
		return fmt.Errorf("meta status: %q is not ACTIVE or PAUSED", status)
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(objectID), url.Values{"status": {status}, "access_token": {token}}, &out); err != nil {
		return fmt.Errorf("meta status: %w", err)
	}
	return nil
}

// AdReview is an ad's delivery and review state.
type AdReview struct {
	EffectiveStatus string `json:"effective_status"`
	Feedback        struct {
		Global map[string]map[string]string `json:"global"`
	} `json:"ad_review_feedback"`
}

// Reasons flattens the review feedback for the owner.
func (r AdReview) Reasons() []string {
	var out []string
	for _, by := range r.Feedback.Global {
		for _, desc := range by {
			out = append(out, desc)
		}
	}
	return out
}

// GetAdReview reads an ad's effective status and review feedback.
func (c *Client) GetAdReview(ctx context.Context, token, adID string) (*AdReview, error) {
	q := url.Values{"fields": {"effective_status,ad_review_feedback"}, "access_token": {token}}
	var r AdReview
	if err := c.getJSON(ctx, "/"+url.PathEscape(adID)+"?"+q.Encode(), &r); err != nil {
		return nil, fmt.Errorf("meta ad review: %w", err)
	}
	return &r, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/"+c.version+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return graphErr(resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

func (c *Client) postJSON(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+c.version+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 300 {
		return graphErr(resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

// GraphError is a Graph API error; Code lets callers react (613 and 80004 are rate limits).
type GraphError struct {
	Status  int
	Code    int
	Message string
}

func (e *GraphError) Error() string {
	return fmt.Sprintf("%d (code %d): %s", e.Status, e.Code, e.Message)
}

// RateLimited reports Meta's ads rate limit errors.
func (e *GraphError) RateLimited() bool { return e.Code == 613 || e.Code == 80004 || e.Code == 17 }

func graphErr(status int, b []byte) error {
	var ge struct {
		Error struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &ge) == nil && ge.Error.Message != "" {
		return &GraphError{Status: status, Code: ge.Error.Code, Message: ge.Error.Message}
	}
	return &GraphError{Status: status, Message: truncate(string(b), 200)}
}

func actPrefix(id string) string { return "act_" + strings.TrimPrefix(id, "act_") }

func firstOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
