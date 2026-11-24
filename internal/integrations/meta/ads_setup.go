package meta

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Graph calls of the ads setup assistant (plan item 65). Every read is its own call, so a
// permission Meta has not granted hides one check, not all of them.

// WithBase returns the client pointed at another host, for tests.
func (c *Client) WithBase(base string, hc *http.Client) *Client {
	out := *c
	out.baseURL, out.httpClient = base, hc
	return &out
}

// Pixel is a Meta pixel (dataset) on an ad account.
type Pixel struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	LastFired   string `json:"last_fired_time"`
	Unavailable bool   `json:"is_unavailable"`
}

// Pixels lists the ad account's pixels.
func (c *Client) Pixels(ctx context.Context, token, actID string) ([]Pixel, error) {
	return listOf[Pixel](ctx, c, token, "/"+url.PathEscape(actPrefix(actID))+"/adspixels", "id,name,last_fired_time,is_unavailable")
}

// CreatePixel makes the ad account's pixel; Meta refuses when the account already has one.
func (c *Client) CreatePixel(ctx context.Context, token, actID, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.postJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/adspixels",
		url.Values{"name": {name}, "access_token": {token}}, &out); err != nil {
		return "", fmt.Errorf("meta pixel: %w", err)
	}
	return out.ID, nil
}

// MatchQuality is one event's Event Match Quality score (0 to 10).
type MatchQuality struct {
	Event string
	Score float64
}

// DatasetQuality reads the web events' match quality (Dataset Quality API).
func (c *Client) DatasetQuality(ctx context.Context, token, datasetID string) ([]MatchQuality, error) {
	q := url.Values{"dataset_id": {datasetID}, "fields": {"web{event_match_quality,event_name}"}, "access_token": {token}}
	var raw struct {
		Web []struct {
			Event string `json:"event_name"`
			EMQ   struct {
				Score float64 `json:"composite_score"`
			} `json:"event_match_quality"`
		} `json:"web"`
	}
	if err := c.getJSON(ctx, "/dataset_quality?"+q.Encode(), &raw); err != nil {
		return nil, fmt.Errorf("meta dataset quality: %w", err)
	}
	out := make([]MatchQuality, 0, len(raw.Web))
	for _, w := range raw.Web {
		out = append(out, MatchQuality{Event: w.Event, Score: w.EMQ.Score})
	}
	return out, nil
}

// PageTasks returns what the connected login may do on each Page it granted, by Page id
// (ADVERTISE, MANAGE, CREATE_CONTENT and so on).
func (c *Client) PageTasks(ctx context.Context, token string) (map[string][]string, error) {
	rows, err := listOf[struct {
		ID    string   `json:"id"`
		Tasks []string `json:"tasks"`
	}](ctx, c, token, "/me/accounts", "id,tasks")
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Tasks
	}
	return out, nil
}

// PromotePages lists the ids of the Pages this ad account may advertise. A Page missing from it
// is not proof it cannot be advertised (seen live 2026-11-25 with full Page access).
func (c *Client) PromotePages(ctx context.Context, token, actID string) ([]string, error) {
	rows, err := listOf[struct {
		ID string `json:"id"`
	}](ctx, c, token, "/"+url.PathEscape(actPrefix(actID))+"/promote_pages", "id")
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out, nil
}

// LeadForm is an instant form on a Page.
type LeadForm struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"` // ACTIVE, ARCHIVED, DELETED, DRAFT
}

// LeadForms lists the Page's instant forms (Page token).
func (c *Client) LeadForms(ctx context.Context, pageToken, pageID string) ([]LeadForm, error) {
	return listOf[LeadForm](ctx, c, pageToken, "/"+url.PathEscape(pageID)+"/leadgen_forms", "id,name,status")
}

// LeadTerms reports whether Meta's lead ads terms are accepted on the Page.
func (c *Client) LeadTerms(ctx context.Context, pageToken, pageID string) (bool, error) {
	var out struct {
		Accepted *bool `json:"leadgen_tos_accepted"`
	}
	if err := c.fieldsOf(ctx, pageToken, pageID, "leadgen_tos_accepted", &out); err != nil {
		return false, err
	}
	if out.Accepted == nil {
		return false, fmt.Errorf("meta did not say whether the lead ads terms are accepted")
	}
	return *out.Accepted, nil
}

// PageInstagram returns the linked Instagram professional account ("" when none).
func (c *Client) PageInstagram(ctx context.Context, pageToken, pageID string) (string, error) {
	var out struct {
		IG struct {
			ID string `json:"id"`
		} `json:"instagram_business_account"`
	}
	err := c.fieldsOf(ctx, pageToken, pageID, "instagram_business_account", &out)
	return out.IG.ID, err
}

// PageWhatsApp reads the Page's WhatsApp number. has is nil when Meta leaves every field out,
// which happens without the permission and must not read as "no number".
func (c *Client) PageWhatsApp(ctx context.Context, pageToken, pageID string) (number string, has *bool, err error) {
	var out struct {
		Number string `json:"whatsapp_number"`
		Has    *bool  `json:"has_whatsapp_number"`
		HasBiz *bool  `json:"has_whatsapp_business_number"`
	}
	if err = c.fieldsOf(ctx, pageToken, pageID, "whatsapp_number,has_whatsapp_number,has_whatsapp_business_number", &out); err != nil {
		return "", nil, err
	}
	if out.Number == "" && out.Has == nil && out.HasBiz == nil {
		return "", nil, nil
	}
	yes := out.Number != "" || (out.Has != nil && *out.Has) || (out.HasBiz != nil && *out.HasBiz)
	return out.Number, &yes, nil
}

// OwnedDomain is a domain in the business portfolio that owns the ad account. Status is empty
// when Meta leaves it out.
type OwnedDomain struct {
	Name   string `json:"domain_name"`
	Status string `json:"status"`
}

// BusinessDomains returns the business portfolio that owns the ad account ("" when it is a
// personal account) and the domains in it.
func (c *Client) BusinessDomains(ctx context.Context, token, actID string) (businessID string, domains []OwnedDomain, err error) {
	var acc struct {
		Business struct {
			ID string `json:"id"`
		} `json:"business"`
	}
	if err = c.fieldsOf(ctx, token, actPrefix(actID), "business", &acc); err != nil || acc.Business.ID == "" {
		return acc.Business.ID, nil, err
	}
	// No fields parameter: Meta documents the edge without naming the domain's own fields.
	domains, err = listOf[OwnedDomain](ctx, c, token, "/"+url.PathEscape(acc.Business.ID)+"/owned_domains", "")
	return acc.Business.ID, domains, err
}

// ProblemAd is an ad Meta rejected or flagged.
type ProblemAd struct {
	Name   string `json:"name"`
	Status string `json:"effective_status"`
	Issues []struct {
		Summary string `json:"error_summary"`
	} `json:"issues_info"`
}

// ProblemAds lists disapproved ads and ads with issues.
func (c *Client) ProblemAds(ctx context.Context, token, actID string) ([]ProblemAd, error) {
	f, _ := json.Marshal([]map[string]any{{"field": "effective_status", "operator": "IN", "value": []string{"DISAPPROVED", "WITH_ISSUES"}}})
	q := url.Values{"fields": {"name,effective_status,issues_info"}, "filtering": {string(f)}, "limit": {"25"}, "access_token": {token}}
	var out struct {
		Data []ProblemAd `json:"data"`
	}
	if err := c.getJSON(ctx, "/"+url.PathEscape(actPrefix(actID))+"/ads?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("meta ads: %w", err)
	}
	return out.Data, nil
}

func (c *Client) fieldsOf(ctx context.Context, token, id, fields string, out any) error {
	q := url.Values{"fields": {fields}, "access_token": {token}}
	return c.getJSON(ctx, "/"+url.PathEscape(id)+"?"+q.Encode(), out)
}

func listOf[T any](ctx context.Context, c *Client, token, path, fields string) ([]T, error) {
	q := url.Values{"limit": {"100"}, "access_token": {token}}
	if fields != "" {
		q.Set("fields", fields)
	}
	var out struct {
		Data []T `json:"data"`
	}
	if err := c.getJSON(ctx, path+"?"+q.Encode(), &out); err != nil {
		return nil, fmt.Errorf("meta %s: %w", path, err)
	}
	return out.Data, nil
}
