package marketingos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/reqid"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ResearchWinner is one competitor ad from the weekly scan, for the owner to look at. Competitor
// text is deliberately absent: only its shape, how long it ran and, for a top winner, its decide
// tags.
type ResearchWinner struct {
	Advertiser   string      `json:"advertiser"`
	Format       string      `json:"format"`
	DaysShown    int         `json:"total_days_shown"`
	StillRunning bool        `json:"still_running"`
	Link         string      `json:"link,omitempty"`
	Tags         *WinnerTags `json:"tags,omitempty"`
}

// ResearchCompetitor is one entry of the owner's competitor list and what the scan last found.
type ResearchCompetitor struct {
	Query   string           `json:"query"`
	ReadAt  string           `json:"read_at,omitempty"`
	Winners []ResearchWinner `json:"winners"`
}

// transparencyHost is the only host a winner may link to. The provider's other links carry its
// own API paths, which are not for owners.
const transparencyHost = "adstransparency.google.com"

// marketResearch reads the scan summary the weekly job keeps in the tenant's metadata.
func marketResearch(meta map[string]any) []ResearchCompetitor {
	out := []ResearchCompetitor{}
	raw, ok := meta[CompetitorScanKey]
	if !ok {
		return out
	}
	var scan struct {
		Competitors []struct {
			Query   string `json:"query"`
			ReadAt  string `json:"read_at"`
			Winners []struct {
				Advertiser   string `json:"advertiser"`
				Format       string `json:"format"`
				DaysShown    int    `json:"total_days_shown"`
				StillRunning bool   `json:"still_running"`
				DetailsLink  string `json:"details_link"`
				Tags         any    `json:"tags"`
			} `json:"winners"`
		} `json:"competitors"`
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, &scan) != nil {
		return out
	}
	for _, c := range scan.Competitors {
		rc := ResearchCompetitor{Query: c.Query, ReadAt: c.ReadAt, Winners: []ResearchWinner{}}
		for _, w := range c.Winners {
			rw := ResearchWinner{Advertiser: w.Advertiser, Format: w.Format, DaysShown: w.DaysShown, StillRunning: w.StillRunning, Tags: readWinnerTags(w.Tags)}
			if u, err := url.Parse(w.DetailsLink); err == nil && u.Scheme == "https" && u.Host == transparencyHost {
				rw.Link = w.DetailsLink
			}
			rc.Winners = append(rc.Winners, rw)
		}
		out = append(out, rc)
	}
	return out
}

// MarketResearch returns what the weekly scan found for this tenant, the scan's summary of its
// tagged winners, and the onboarding market search, if it ran.
func (s *Service) MarketResearch(ctx context.Context, tenantID uuid.UUID) ([]ResearchCompetitor, string, *MarketSuggestions, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, "", nil, err
	}
	return marketResearch(t.Metadata), marketSignals(t.Metadata).Summary, readMarketSuggestions(t.Metadata), nil
}

// ScanQuotaUse is one monthly budget as marketflow-ai counts it.
type ScanQuotaUse struct {
	PlatformUsed  int `json:"platform_used"`
	PlatformLimit int `json:"platform_limit"`
	TenantUsed    int `json:"tenant_used"`
	TenantLimit   int `json:"tenant_limit"`
	TenantLeft    int `json:"tenant_left"`
}

// ScanQuota is this month's competitor search budget and ad-details budget for one tenant.
type ScanQuota struct {
	Month   string       `json:"month"`
	Scan    ScanQuotaUse `json:"scan"`
	Details ScanQuotaUse `json:"details"`
}

// ScanQuotaReader reads a tenant's scan budget from marketflow-ai (no search is made).
type ScanQuotaReader func(ctx context.Context, tenantID uuid.UUID) (*ScanQuota, error)

// NewScanQuotaReader returns nil when marketflow-ai is not configured.
func NewScanQuotaReader(aiURL, key string) ScanQuotaReader {
	if aiURL == "" || key == "" {
		return nil
	}
	hc := reqid.Client(5 * time.Second)
	base := strings.TrimSuffix(aiURL, "/") + "/api/v1/internal/adintel/quota?tenant_id="
	return func(ctx context.Context, tenantID uuid.UUID) (*ScanQuota, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+url.QueryEscape(tenantID.String()), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-API-Key", key)
		resp, err := hc.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("scan quota: status %d", resp.StatusCode)
		}
		var q ScanQuota
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&q); err != nil {
			return nil, err
		}
		return &q, nil
	}
}

// GetResearch handles GET /api/v1/tenant/market-research.
// @Summary What the weekly competitor scan found
// @Tags MarketingOS
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/market-research [get]
func (h *Handler) GetResearch(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	competitors, summary, suggestions, err := h.svc.MarketResearch(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := map[string]any{"competitors": competitors, "suggestions": suggestions, "summary": summary, "quota": nil}
	// Quota left this month is extra: when marketflow-ai cannot be read the page still loads.
	if h.ScanQuota != nil {
		qctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		if q, err := h.ScanQuota(qctx, tid); err == nil {
			out["quota"] = q
		} else {
			h.logger.Debug("scan quota unavailable", zap.Error(err))
		}
		cancel()
	}
	httpapi.JSON(w, http.StatusOK, out)
}
