package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// DeliveryPoint is one point of Meta's daily outcomes curve: a daily spend and what Meta
// estimates it reaches and gets. Spend is in the ad account's minor unit like every Marketing API
// budget field (not stated on the delivery_estimate reference; the plan divides by the account's
// offset and the sandbox run confirms it).
type DeliveryPoint struct {
	Spend       float64 `json:"spend"`
	Reach       float64 `json:"reach"`
	Impressions float64 `json:"impressions"`
	Actions     float64 `json:"actions"`
}

// DeliveryEstimate is Meta's estimate for an audience and optimisation goal on an ad account.
type DeliveryEstimate struct {
	Curve []DeliveryPoint `json:"daily_outcomes_curve"`
	DAU   int64           `json:"estimate_dau"`
	MAULo int64           `json:"estimate_mau_lower_bound"`
	MAUHi int64           `json:"estimate_mau_upper_bound"`
	Ready bool            `json:"estimate_ready"`
}

// DeliveryEstimate reads GET /act_{id}/delivery_estimate for a goal (CONVERSATIONS, LINK_CLICKS,
// LEAD_GENERATION, REACH) and countries. Meta returns a single all-zero point when it is not
// confident; the caller treats that as no estimate.
func (c *Client) DeliveryEstimate(ctx context.Context, token, actID, goal string, countries []string) (*DeliveryEstimate, error) {
	if len(countries) == 0 {
		countries = []string{"KE"}
	}
	spec, err := json.Marshal(map[string]any{"geo_locations": map[string]any{"countries": countries}})
	if err != nil {
		return nil, err
	}
	q := url.Values{"optimization_goal": {goal}, "targeting_spec": {string(spec)}, "access_token": {token}}
	var out struct {
		Data []DeliveryEstimate `json:"data"`
	}
	if err := c.getJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/delivery_estimate?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("meta delivery estimate: %w", err)
	}
	if len(out.Data) == 0 {
		return &DeliveryEstimate{}, nil
	}
	return &out.Data[0], nil
}
