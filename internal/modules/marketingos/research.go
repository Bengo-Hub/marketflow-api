package marketingos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
)

// ResearchWinner is one competitor ad from the weekly scan, for the owner to look at. Competitor
// text is deliberately absent: only its shape and how long it ran.
type ResearchWinner struct {
	Advertiser   string `json:"advertiser"`
	Format       string `json:"format"`
	DaysShown    int    `json:"total_days_shown"`
	StillRunning bool   `json:"still_running"`
	Link         string `json:"link,omitempty"`
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
			rw := ResearchWinner{Advertiser: w.Advertiser, Format: w.Format, DaysShown: w.DaysShown, StillRunning: w.StillRunning}
			if u, err := url.Parse(w.DetailsLink); err == nil && u.Scheme == "https" && u.Host == transparencyHost {
				rw.Link = w.DetailsLink
			}
			rc.Winners = append(rc.Winners, rw)
		}
		out = append(out, rc)
	}
	return out
}

// MarketResearch returns what the weekly scan found for this tenant.
func (s *Service) MarketResearch(ctx context.Context, tenantID uuid.UUID) ([]ResearchCompetitor, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return marketResearch(t.Metadata), nil
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
	competitors, err := h.svc.MarketResearch(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"competitors": competitors})
}
