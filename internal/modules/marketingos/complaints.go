package marketingos

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ComplaintFinder asks marketflow-ai for complaint themes in a niche (the Fix step of Find, Flip,
// Fix). marketflow-ai owns the search, the quota and the credit; the reply passes through as is.
type ComplaintFinder func(ctx context.Context, tenantID uuid.UUID, query, country string) (status int, body []byte, err error)

// NewComplaintFinder returns nil when marketflow-ai is not configured.
func NewComplaintFinder(aiURL, key string) ComplaintFinder {
	if aiURL == "" || key == "" {
		return nil
	}
	hc := &http.Client{Timeout: 45 * time.Second}
	url := strings.TrimSuffix(aiURL, "/") + "/api/v1/internal/adintel/complaints"
	return func(ctx context.Context, tenantID uuid.UUID, query, country string) (int, []byte, error) {
		payload, _ := json.Marshal(map[string]string{"tenant_id": tenantID.String(), "query": query, "country": country})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		resp, err := hc.Do(req)
		if err != nil {
			return 0, nil, fmt.Errorf("complaint search: %w", err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return resp.StatusCode, body, err
	}
}

// FindComplaints handles POST /api/v1/tenant/market-research/complaints {query}: what customers of
// similar businesses complain about. Nothing is stored; the owner adds the themes they agree with
// to their complaints list, and only those can shape an ad.
// @Summary Find complaint themes in the business's market
// @Tags MarketingOS
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 402 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /tenant/market-research/complaints [post]
func (h *Handler) FindComplaints(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.manager(w, r, tid) {
		return
	}
	if h.Complaints == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "complaint search is not available right now", nil)
		return
	}
	var in struct {
		Query string `json:"query"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	in.Query = strings.TrimSpace(in.Query)
	if in.Query == "" || len([]rune(in.Query)) > 120 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_query", "describe the business and town in up to 120 characters", nil)
		return
	}
	country, err := h.svc.tenantCountryCode(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	status, body, err := h.Complaints(r.Context(), tid, in.Query, country)
	if err != nil {
		h.logger.Warn("complaint search failed", zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "search_failed", "the complaint search failed; try again later", nil)
		return
	}
	switch status {
	case http.StatusOK, http.StatusBadRequest, http.StatusPaymentRequired, http.StatusTooManyRequests:
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	default:
		h.logger.Warn("complaint search answered", zap.Int("status", status))
		httpapi.Error(w, http.StatusBadGateway, "search_failed", "the complaint search failed; try again later", nil)
	}
}

// tenantCountryCode is the tenant's two-letter country, or empty to use the search's default.
func (s *Service) tenantCountryCode(ctx context.Context, tenantID uuid.UUID) (string, error) {
	t, err := s.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if t.Country == nil || len(strings.TrimSpace(*t.Country)) != 2 {
		return "", nil
	}
	return strings.ToLower(strings.TrimSpace(*t.Country)), nil
}
