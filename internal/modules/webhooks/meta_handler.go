package webhooks

import (
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MetaHandler handles Meta (Facebook/Instagram) lead generation webhooks.
type MetaHandler struct {
	leadService *leads.Service
	logger      *zap.Logger
}

// NewMetaHandler creates a new Meta webhook handler.
func NewMetaHandler(leadService *leads.Service, logger *zap.Logger) *MetaHandler {
	return &MetaHandler{
		leadService: leadService,
		logger:      logger.Named("webhooks.meta"),
	}
}

type metaLeadgenEntry struct {
	ID      string             `json:"id"`
	Time    int64              `json:"time"`
	Changes []metaLeadgenChange `json:"changes"`
}

type metaLeadgenChange struct {
	Field string             `json:"field"`
	Value metaLeadgenValue   `json:"value"`
}

type metaLeadgenValue struct {
	FormID       string `json:"form_id"`
	LeadgenID    string `json:"leadgen_id"`
	CreatedTime  int64  `json:"created_time"`
	PageID       string `json:"page_id"`
	AdgroupID    string `json:"adgroup_id"`
	AdID         string `json:"ad_id"`
	CampaignID   string `json:"campaign_id,omitempty"`
}

type metaLeadgenWebhook struct {
	Object string             `json:"object"`
	Entry  []metaLeadgenEntry `json:"entry"`
}

type metaLeadFormData struct {
	TenantID   string `json:"tenant_id"`
	Email      string `json:"email"`
	Phone      string `json:"phone"`
	FirstName  string `json:"first_name"`
	LastName   string `json:"last_name"`
	CampaignID string `json:"campaign_id"`
	Interest   string `json:"interest"`
}

// HandleLeadgen handles POST /api/v1/webhooks/meta/leadgen
// @Summary Handle Meta lead generation webhook
// @Description Receives lead form submissions from Meta (Facebook/Instagram) ads
// @Tags Webhooks
// @Accept json
// @Produce json
// @Param request body metaLeadFormData true "Meta lead form data"
// @Success 200 {object} map[string]string
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /webhooks/meta/leadgen [post]
func (h *MetaHandler) HandleLeadgen(w http.ResponseWriter, r *http.Request) {
	var req metaLeadFormData
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		h.logger.Warn("invalid meta webhook payload", zap.Error(err))
		// Meta requires fast 200 response; log but don't fail
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		h.logger.Warn("invalid tenant_id in meta webhook", zap.String("tenant_id", req.TenantID))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	input := leads.CreateInput{
		TenantID: tenantID,
		Source:   "meta_ad",
		LeadType: "customer",
	}

	if req.Email != "" {
		input.Email = &req.Email
	}
	if req.Phone != "" {
		input.Phone = &req.Phone
	}
	if req.FirstName != "" {
		input.FirstName = &req.FirstName
	}
	if req.LastName != "" {
		input.LastName = &req.LastName
	}
	if req.CampaignID != "" {
		if cid, err := uuid.Parse(req.CampaignID); err == nil {
			input.CampaignID = &cid
		}
	}
	if req.Interest != "" {
		input.Interest = &req.Interest
	}
	input.Metadata = map[string]any{
		"webhook_source":    "meta_leadgen",
		"webhook_received":  time.Now().UTC().Format(time.RFC3339),
	}

	if _, err := h.leadService.Create(r.Context(), input); err != nil {
		h.logger.Error("failed to create lead from meta webhook", zap.Error(err))
		// Still return 200 so Meta doesn't retry
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "error", "message": "failed to process lead"})
		return
	}

	h.logger.Info("meta lead created from webhook",
		zap.String("tenant_id", tenantID.String()),
	)

	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ok"})
}
