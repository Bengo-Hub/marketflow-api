package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TikTokHandler handles TikTok lead form webhook submissions.
//
// TikTok doesn't sign webhooks consistently, so verification is optional
// via a shared secret query parameter. The handler upserts leads and publishes
// a lead.created event via the outbox (handled by the lead service).
type TikTokHandler struct {
	leadService  *leads.Service
	db           *sql.DB
	sharedSecret string
	logger       *zap.Logger
}

// TikTokConfig holds configuration for the TikTok webhook handler.
type TikTokConfig struct {
	DB           *sql.DB
	SharedSecret string // optional shared secret for verification
}

// NewTikTokHandler creates a new TikTok webhook handler.
func NewTikTokHandler(leadService *leads.Service, cfg TikTokConfig, logger *zap.Logger) *TikTokHandler {
	return &TikTokHandler{
		leadService:  leadService,
		db:           cfg.DB,
		sharedSecret: cfg.SharedSecret,
		logger:       logger.Named("webhooks.tiktok"),
	}
}

// tiktokLeadWebhook represents a TikTok lead form submission webhook payload.
type tiktokLeadWebhook struct {
	Event     string `json:"event"`
	Timestamp int64  `json:"timestamp"`
	Lead      struct {
		LeadID       string `json:"lead_id"`
		FormID       string `json:"form_id"`
		CampaignID   string `json:"campaign_id"`
		AdvertiserID string `json:"advertiser_id"`
		AdID         string `json:"ad_id"`
		AdGroupID    string `json:"adgroup_id"`
		CreativeID   string `json:"creative_id"`
		Fields       []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	} `json:"lead"`
}

// HandleLeads handles POST /api/v1/webhooks/tiktok/leads.
// @Summary TikTok lead form webhook
// @Description Receives lead form submissions from TikTok ads. Upserts leads with source=tiktok.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Success 200 {object} map[string]string
// @Failure 401 {object} httpapi.ErrorResponse
// @Router /webhooks/tiktok/leads [post]
func (h *TikTokHandler) HandleLeads(w http.ResponseWriter, r *http.Request) {
	// Optional shared secret verification via query param.
	if h.sharedSecret != "" {
		secret := r.URL.Query().Get("secret")
		if secret != h.sharedSecret {
			h.logger.Warn("tiktok webhook secret mismatch")
			httpapi.JSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid secret"})
			return
		}
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Warn("read body", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	var payload tiktokLeadWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Warn("invalid tiktok webhook payload", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	ctx := r.Context()

	// Resolve tenant by advertiser_id.
	tenantID, err := h.resolveTenantByAdvertiserID(ctx, payload.Lead.AdvertiserID)
	if err != nil {
		h.logger.Warn("no tenant for tiktok advertiser",
			zap.String("advertiser_id", payload.Lead.AdvertiserID), zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received", "warning": "tenant not resolved"})
		return
	}

	// Extract lead fields from the form submission.
	var email, phone, firstName, lastName string
	for _, f := range payload.Lead.Fields {
		switch f.Name {
		case "email":
			email = f.Value
		case "phone_number", "phone":
			phone = f.Value
		case "first_name", "name":
			firstName = f.Value
		case "last_name":
			lastName = f.Value
		}
	}

	input := leads.CreateInput{
		TenantID: tenantID,
		Source:   "tiktok",
		LeadType: "customer",
		Metadata: map[string]any{
			"webhook_source":       "tiktok_leadgen",
			"webhook_received":     time.Now().UTC().Format(time.RFC3339),
			"tiktok_lead_id":       payload.Lead.LeadID,
			"tiktok_form_id":       payload.Lead.FormID,
			"tiktok_campaign_id":   payload.Lead.CampaignID,
			"tiktok_ad_id":         payload.Lead.AdID,
			"tiktok_adgroup_id":    payload.Lead.AdGroupID,
			"tiktok_advertiser_id": payload.Lead.AdvertiserID,
		},
	}
	if email != "" {
		input.Email = &email
	}
	if phone != "" {
		input.Phone = &phone
	}
	if firstName != "" {
		input.FirstName = &firstName
	}
	if lastName != "" {
		input.LastName = &lastName
	}

	if _, err := h.leadService.Create(ctx, input); err != nil {
		h.logger.Error("create lead from tiktok webhook",
			zap.String("tenant", tenantID.String()), zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received", "error": "create_failed"})
		return
	}

	h.logger.Info("tiktok lead webhook processed",
		zap.String("tenant", tenantID.String()),
		zap.String("lead_id", payload.Lead.LeadID),
	)
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

// resolveTenantByAdvertiserID looks up the tenant by its linked TikTok advertiser_id.
// Checks integration_connections first, then falls back to tenant metadata.
func (h *TikTokHandler) resolveTenantByAdvertiserID(ctx context.Context, advertiserID string) (uuid.UUID, error) {
	if h.db == nil {
		return uuid.Nil, fmt.Errorf("db not configured")
	}
	var id uuid.UUID
	err := h.db.QueryRowContext(ctx,
		`SELECT COALESCE(
			(SELECT tenant_id FROM integration_connections WHERE provider = 'tiktok' AND external_account_id = $1 LIMIT 1),
			(SELECT id FROM tenants WHERE metadata->>'tiktok_advertiser_id' = $1 LIMIT 1)
		)`, advertiserID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
