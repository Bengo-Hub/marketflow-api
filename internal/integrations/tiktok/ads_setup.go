package tiktok

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// TikTok Business API calls of the ads setup assistant (plan item 65). TikTok answers HTTP 200
// with an error code in the envelope.

// APIError is a TikTok refusal.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("tiktok API error %d: %s", e.Code, e.Message) }

// WithBase returns the client pointed at another host, for tests.
func (c *Client) WithBase(base string, hc *http.Client) *Client {
	out := *c
	out.baseURL, out.httpClient = base, hc
	return &out
}

// AdvertiserInfo is an ad account's status. Balance is nil when TikTok leaves it out.
type AdvertiserInfo struct {
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	Currency     string   `json:"currency"`
	OwnerBC      string   `json:"owner_bc_id"`
	RejectReason string   `json:"rejection_reason"`
	Balance      *float64 `json:"balance"`
}

// Advertiser reads one ad account.
func (c *Client) Advertiser(ctx context.Context, token, advertiserID string) (AdvertiserInfo, error) {
	ids, _ := json.Marshal([]string{advertiserID})
	var out struct {
		List []AdvertiserInfo `json:"list"`
	}
	if err := c.call(ctx, http.MethodGet, "/advertiser/info/", token, url.Values{"advertiser_ids": {string(ids)}}, nil, &out); err != nil {
		return AdvertiserInfo{}, err
	}
	if len(out.List) == 0 {
		return AdvertiserInfo{}, fmt.Errorf("tiktok returned no ad account")
	}
	return out.List[0], nil
}

// Pixel is a TikTok pixel.
type Pixel struct {
	Name     string `json:"pixel_name"`
	Activity string `json:"activity_status"`
	Events   []struct {
		Type string `json:"event_type"`
	} `json:"events"`
}

// Pixels lists the advertiser's pixels.
func (c *Client) Pixels(ctx context.Context, token, advertiserID string) ([]Pixel, error) {
	var out struct {
		Pixels []Pixel `json:"pixels"`
	}
	err := c.call(ctx, http.MethodGet, "/pixel/list/", token, url.Values{"advertiser_id": {advertiserID}, "page_size": {"20"}}, nil, &out)
	return out.Pixels, err
}

// CreatePixel makes a pixel. advertiser_id and pixel_name are the required fields (TikTok Business
// API SDK, PixelCreateBody, read 2026-11-25).
func (c *Client) CreatePixel(ctx context.Context, token, advertiserID, name string) error {
	return c.call(ctx, http.MethodPost, "/pixel/create/", token, nil, map[string]any{"advertiser_id": advertiserID, "pixel_name": name}, nil)
}

// LeadPages counts the advertiser's published instant forms.
func (c *Client) LeadPages(ctx context.Context, token, advertiserID string) (int, error) {
	var out struct {
		List []json.RawMessage `json:"list"`
	}
	err := c.call(ctx, http.MethodGet, "/page/get/", token,
		url.Values{"advertiser_id": {advertiserID}, "business_type": {"LEAD_GEN"}, "status": {"PUBLISHED"}}, nil, &out)
	return len(out.List), err
}

// AdStatus is an ad and its delivery status.
type AdStatus struct {
	Name   string `json:"ad_name"`
	Status string `json:"secondary_status"`
}

// AdStatuses reads the first 100 ads' statuses.
func (c *Client) AdStatuses(ctx context.Context, token, advertiserID string) ([]AdStatus, error) {
	var out struct {
		List []AdStatus `json:"list"`
	}
	err := c.call(ctx, http.MethodGet, "/ad/get/", token, url.Values{"advertiser_id": {advertiserID}, "page_size": {"100"},
		"fields": {`["ad_name","secondary_status"]`}}, nil, &out)
	return out.List, err
}

func (c *Client) call(ctx context.Context, method, path, token string, q url.Values, body, out any) error {
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Access-Token", token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env tiktokResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env); err != nil {
		return fmt.Errorf("decode tiktok response: %w", err)
	}
	if env.Code != 0 {
		return &APIError{Code: env.Code, Message: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, out)
}
