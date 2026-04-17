package tiktok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const baseURL = "https://business-api.tiktok.com/open_api/v1.3"

// Client communicates with the TikTok Business API.
type Client struct {
	httpClient *http.Client
}

// NewClient creates a new TikTok Business API client.
func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// CampaignInput contains the fields for creating a TikTok campaign.
type CampaignInput struct {
	CampaignName  string  `json:"campaign_name"`
	ObjectiveType string  `json:"objective_type"` // LEAD_GENERATION
	BudgetMode    string  `json:"budget_mode"`    // BUDGET_MODE_DAY
	Budget        float64 `json:"budget"`
}

// Metrics holds campaign report metrics returned by TikTok.
type Metrics struct {
	Spend       float64 `json:"spend"`
	Impressions int64   `json:"impressions"`
	Clicks      int64   `json:"clicks"`
	Conversions int64   `json:"conversions"`
	Leads       int64   `json:"leads"`
}

// tiktokResponse is the generic response envelope from TikTok API.
type tiktokResponse struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// CreateCampaign creates a new campaign on TikTok Business.
func (c *Client) CreateCampaign(ctx context.Context, advertiserID, accessToken string, campaign CampaignInput) (string, error) {
	payload := map[string]any{
		"advertiser_id":  advertiserID,
		"campaign_name":  campaign.CampaignName,
		"objective_type": campaign.ObjectiveType,
		"budget_mode":    campaign.BudgetMode,
		"budget":         campaign.Budget,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal campaign payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/campaign/create/", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("tiktok campaign create: %w", err)
	}
	defer resp.Body.Close()

	var result tiktokResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode tiktok response: %w", err)
	}
	if result.Code != 0 {
		return "", fmt.Errorf("tiktok API error %d: %s", result.Code, result.Message)
	}

	var data struct {
		CampaignID string `json:"campaign_id"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return "", fmt.Errorf("decode campaign data: %w", err)
	}
	return data.CampaignID, nil
}

// UpdateCampaignStatus enables or disables a campaign.
func (c *Client) UpdateCampaignStatus(ctx context.Context, advertiserID, accessToken, campaignID, status string) error {
	payload := map[string]any{
		"advertiser_id":    advertiserID,
		"campaign_id":      campaignID,
		"operation_status": status, // ENABLE or DISABLE
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal update payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/campaign/update/", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Access-Token", accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("tiktok campaign update: %w", err)
	}
	defer resp.Body.Close()

	var result tiktokResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode tiktok response: %w", err)
	}
	if result.Code != 0 {
		return fmt.Errorf("tiktok API error %d: %s", result.Code, result.Message)
	}
	return nil
}

// GetCampaignReport retrieves campaign metrics from TikTok reporting API.
func (c *Client) GetCampaignReport(ctx context.Context, advertiserID, accessToken, campaignID string) (Metrics, error) {
	params := url.Values{}
	params.Set("advertiser_id", advertiserID)
	params.Set("report_type", "BASIC")
	params.Set("dimensions", `["campaign_id"]`)
	params.Set("metrics", `["spend","impressions","clicks","conversions","leads"]`)
	params.Set("filters", fmt.Sprintf(`[{"field_name":"campaign_ids","filter_type":"IN","filter_value":"[%q]"}]`, campaignID))
	params.Set("data_level", "AUCTION_CAMPAIGN")
	params.Set("lifetime", "true")

	reqURL := baseURL + "/report/integrated/get/?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return Metrics{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Access-Token", accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return Metrics{}, fmt.Errorf("tiktok report: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return Metrics{}, fmt.Errorf("read response: %w", err)
	}

	var result tiktokResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return Metrics{}, fmt.Errorf("decode tiktok response: %w", err)
	}
	if result.Code != 0 {
		return Metrics{}, fmt.Errorf("tiktok API error %d: %s", result.Code, result.Message)
	}

	var data struct {
		List []struct {
			Metrics Metrics `json:"metrics"`
		} `json:"list"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return Metrics{}, fmt.Errorf("decode report data: %w", err)
	}
	if len(data.List) == 0 {
		return Metrics{}, nil
	}
	return data.List[0].Metrics, nil
}
