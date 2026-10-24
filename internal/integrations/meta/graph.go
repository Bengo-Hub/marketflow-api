// Package meta is a thin Meta Graph API client for campaign CRUD + insights.
// Scope is deliberately minimal — MarketFlow stores its own Campaign rows
// and mirrors status/budget to Meta via this client; insights are fetched
// daily into AdMetrics. No gRPC, no SDK, just HTTP + JSON.
package meta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GraphVersion is the one Graph API version MarketFlow calls (the Meta app is
// set to v26.0; v21.0 reached the end of Meta's two-year window). GraphURL and
// DialogURL are its bases; every Meta call site uses them.
const (
	GraphVersion = "v26.0"
	GraphURL     = "https://graph.facebook.com/" + GraphVersion
	DialogURL    = "https://www.facebook.com/" + GraphVersion + "/dialog/oauth"
)

const defaultGraphVersion = GraphVersion

// Client talks to graph.facebook.com with a tenant-specific page or ad
// account access token. Platform credentials (app_id / app_secret) are NOT
// needed here — they live in the OAuth exchange path.
type Client struct {
	httpClient *http.Client
	version    string
	baseURL    string
}

// NewClient builds a client with sane timeouts. version="" defaults to GraphVersion.
func NewClient(version string) *Client {
	if version == "" {
		version = defaultGraphVersion
	}
	return &Client{
		httpClient: &http.Client{Timeout: 20 * time.Second},
		version:    version,
		baseURL:    "https://graph.facebook.com",
	}
}

// WithSecret makes every call carry appsecret_proof (see proof.go).
func (c *Client) WithSecret(secret SecretSource) *Client {
	c.httpClient.Transport = NewProofTransport(c.httpClient.Transport, secret)
	return c
}

// CampaignInput is the minimal shape of a Meta campaign. Objective, special
// ad categories and targeting live on AdSet; we expose only what marketflow
// currently surfaces in the Campaign schema.
type CampaignInput struct {
	Name                string   `json:"name"`
	Objective           string   `json:"objective,omitempty"`       // OUTCOME_LEADS, OUTCOME_SALES, etc.
	Status              string   `json:"status,omitempty"`          // ACTIVE | PAUSED
	DailyBudget         int64    `json:"daily_budget,omitempty"`    // cents
	LifetimeBudget      int64    `json:"lifetime_budget,omitempty"` // cents
	SpecialAdCategories []string `json:"special_ad_categories,omitempty"`
}

// CampaignResponse is the envelope returned by POST /act_{id}/campaigns and
// GET /{campaign_id}.
type CampaignResponse struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
	Objective      string `json:"objective"`
	DailyBudget    string `json:"daily_budget"`
	LifetimeBudget string `json:"lifetime_budget"`
	CreatedTime    string `json:"created_time"`
	UpdatedTime    string `json:"updated_time"`
}

// CreateCampaign POSTs to /act_{adAccountID}/campaigns. adAccountID must
// NOT include the `act_` prefix — the helper adds it.
func (c *Client) CreateCampaign(ctx context.Context, accessToken, adAccountID string, in CampaignInput) (*CampaignResponse, error) {
	if accessToken == "" || adAccountID == "" {
		return nil, fmt.Errorf("meta: access token + ad account id required")
	}
	form := url.Values{}
	form.Set("access_token", accessToken)
	form.Set("name", in.Name)
	if in.Objective != "" {
		form.Set("objective", in.Objective)
	}
	if in.Status != "" {
		form.Set("status", in.Status)
	}
	if in.DailyBudget > 0 {
		form.Set("daily_budget", fmt.Sprintf("%d", in.DailyBudget))
	}
	if in.LifetimeBudget > 0 {
		form.Set("lifetime_budget", fmt.Sprintf("%d", in.LifetimeBudget))
	}
	if len(in.SpecialAdCategories) > 0 {
		cats, _ := json.Marshal(in.SpecialAdCategories)
		form.Set("special_ad_categories", string(cats))
	} else {
		form.Set("special_ad_categories", "[]")
	}
	endpoint := fmt.Sprintf("%s/%s/act_%s/campaigns", c.baseURL, c.version, url.PathEscape(strings.TrimPrefix(adAccountID, "act_")))
	resp, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return nil, err
	}
	return decodeCampaign(resp)
}

// UpdateCampaign PATCHes /{campaign_id} — Meta accepts POST with the same
// fields as an update, which we use for simplicity.
func (c *Client) UpdateCampaign(ctx context.Context, accessToken, campaignID string, in CampaignInput) (*CampaignResponse, error) {
	if accessToken == "" || campaignID == "" {
		return nil, fmt.Errorf("meta: access token + campaign id required")
	}
	form := url.Values{}
	form.Set("access_token", accessToken)
	if in.Name != "" {
		form.Set("name", in.Name)
	}
	if in.Status != "" {
		form.Set("status", in.Status)
	}
	if in.DailyBudget > 0 {
		form.Set("daily_budget", fmt.Sprintf("%d", in.DailyBudget))
	}
	if in.LifetimeBudget > 0 {
		form.Set("lifetime_budget", fmt.Sprintf("%d", in.LifetimeBudget))
	}
	endpoint := fmt.Sprintf("%s/%s/%s", c.baseURL, c.version, url.PathEscape(campaignID))
	resp, err := c.postForm(ctx, endpoint, form)
	if err != nil {
		return nil, err
	}
	return decodeCampaign(resp)
}

// DeleteCampaign issues DELETE /{campaign_id}.
func (c *Client) DeleteCampaign(ctx context.Context, accessToken, campaignID string) error {
	if accessToken == "" || campaignID == "" {
		return fmt.Errorf("meta: access token + campaign id required")
	}
	endpoint := fmt.Sprintf("%s/%s/%s?access_token=%s", c.baseURL, c.version, url.PathEscape(campaignID), url.QueryEscape(accessToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("meta delete %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return nil
}

// Insights is the aggregate metrics row for a campaign over a date range.
type Insights struct {
	CampaignID  string `json:"campaign_id"`
	Date        string `json:"date_start"`
	Impressions string `json:"impressions"`
	Clicks      string `json:"clicks"`
	Spend       string `json:"spend"`
	Leads       string `json:"leads"`
	CTR         string `json:"ctr"`
	CPL         string `json:"cost_per_result"`
	Currency    string `json:"currency"`
}

// GetCampaignInsights fetches today's metrics for a single campaign. Returns
// the most-recent daily row when Meta has one.
func (c *Client) GetCampaignInsights(ctx context.Context, accessToken, campaignID string) (*Insights, error) {
	if accessToken == "" || campaignID == "" {
		return nil, fmt.Errorf("meta: access token + campaign id required")
	}
	q := url.Values{}
	q.Set("access_token", accessToken)
	q.Set("fields", "campaign_id,date_start,impressions,clicks,spend,ctr,cost_per_result")
	q.Set("date_preset", "today")
	q.Set("time_increment", "1")
	endpoint := fmt.Sprintf("%s/%s/%s/insights?%s", c.baseURL, c.version, url.PathEscape(campaignID), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta insights %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	var env struct {
		Data []Insights `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return nil, err
	}
	if len(env.Data) == 0 {
		return &Insights{CampaignID: campaignID, Date: time.Now().UTC().Format("2006-01-02")}, nil
	}
	return &env.Data[0], nil
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("meta %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	return b, nil
}

func decodeCampaign(b []byte) (*CampaignResponse, error) {
	var cr CampaignResponse
	if err := json.Unmarshal(b, &cr); err != nil {
		return nil, fmt.Errorf("meta decode: %w", err)
	}
	return &cr, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
