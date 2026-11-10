package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MetaHandler handles Meta (Facebook/Instagram) lead generation webhooks.
//
// Meta's leadgen webhook flow:
//  1. GET /webhooks/meta/leadgen?hub.mode=subscribe&hub.verify_token=...&hub.challenge=...
//     → echo challenge if token matches platform-configured verify token.
//  2. POST /webhooks/meta/leadgen with X-Hub-Signature-256 header
//     → verify signature with platform app secret, parse entry.changes, upsert leads.
//
// All secrets (MF_META_APP_SECRET, MF_META_VERIFY_TOKEN) are platform-level
// ProviderSettings — never per-tenant. Tenants are resolved via page_id lookup
// on Tenant.meta_page_id.
type MetaHandler struct {
	leadService *leads.Service
	db          *sql.DB
	appSecret   string
	verifyToken string
	// allowUnsigned accepts events with no app secret set (local development only).
	allowUnsigned bool
	secret        func(ctx context.Context) string
	inbox         *InboxStore
	logger        *zap.Logger
}

// MetaConfig configures the MetaHandler with platform-level credentials.
type MetaConfig struct {
	DB          *sql.DB
	AppSecret   string // platform-level (X-Hub signature verification)
	VerifyToken string // platform-level (subscription verification)
	// AllowUnsigned lets events through when AppSecret is empty; set only in development.
	AllowUnsigned bool
	// Secret resolves the app secret per request (the platform provider row first, then the
	// env value), like the worker and the OAuth flows; nil uses AppSecret alone.
	Secret func(ctx context.Context) string
	// Inbox stores Page and Instagram comments and DMs (Sprint 22); nil skips them.
	Inbox *InboxStore
}

// NewMetaHandler creates a new Meta webhook handler.
func NewMetaHandler(leadService *leads.Service, cfg MetaConfig, logger *zap.Logger) *MetaHandler {
	return &MetaHandler{
		leadService:   leadService,
		db:            cfg.DB,
		appSecret:     cfg.AppSecret,
		verifyToken:   cfg.VerifyToken,
		allowUnsigned: cfg.AllowUnsigned,
		secret:        cfg.Secret,
		inbox:         cfg.Inbox,
		logger:        logger.Named("webhooks.meta"),
	}
}

type metaLeadgenEntry struct {
	ID      string              `json:"id"` // page_id
	Time    int64               `json:"time"`
	Changes []metaLeadgenChange `json:"changes"`
}

type metaLeadgenChange struct {
	Field string           `json:"field"`
	Value metaLeadgenValue `json:"value"`
}

type metaLeadgenValue struct {
	FormID      string `json:"form_id"`
	LeadgenID   string `json:"leadgen_id"`
	CreatedTime int64  `json:"created_time"`
	PageID      string `json:"page_id"`
	AdgroupID   string `json:"adgroup_id"`
	AdID        string `json:"ad_id"`
	CampaignID  string `json:"campaign_id,omitempty"`
}

type metaLeadgenWebhook struct {
	Object string             `json:"object"`
	Entry  []metaLeadgenEntry `json:"entry"`
}

// Verify handles GET /api/v1/webhooks/meta/leadgen subscription verification.
// @Summary Meta webhook subscription verification
// @Tags Webhooks
// @Produce plain
// @Param hub.mode query string false "subscribe"
// @Param hub.verify_token query string false "token"
// @Param hub.challenge query string false "challenge"
// @Success 200 {string} string "challenge echo"
// @Router /webhooks/meta/leadgen [get]
func (h *MetaHandler) Verify(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	mode := q.Get("hub.mode")
	token := q.Get("hub.verify_token")
	challenge := q.Get("hub.challenge")
	if mode == "subscribe" && h.verifyToken != "" && token == h.verifyToken {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(challenge))
		h.logger.Info("meta webhook verified")
		return
	}
	h.logger.Warn("meta webhook verify failed",
		zap.String("mode", mode), zap.Bool("token_set", h.verifyToken != ""))
	w.WriteHeader(http.StatusForbidden)
}

// HandleLeadgen handles POST /api/v1/webhooks/meta/leadgen
// @Summary Meta lead generation webhook
// @Description Receives lead form submissions from Meta (Facebook/Instagram) ads with HMAC signature verification.
// @Tags Webhooks
// @Accept json
// @Produce json
// @Param X-Hub-Signature-256 header string true "HMAC-SHA256 signature of request body using platform Meta app secret"
// @Success 200 {object} map[string]string
// @Failure 401 {object} httpapi.ErrorResponse "Invalid signature"
// @Router /webhooks/meta/leadgen [post]
func (h *MetaHandler) HandleLeadgen(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.logger.Warn("read body", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}
	// Signature verification with the platform Meta app secret. With no secret set the events
	// are refused (fail closed), except in local development: unsigned events would let anyone
	// create leads and inbox items for any tenant.
	secret := h.appSecret
	if h.secret != nil {
		secret = h.secret(r.Context())
	}
	if secret == "" {
		if !h.allowUnsigned {
			h.logger.Error("meta webhook refused: no app secret is configured")
			httpapi.JSON(w, http.StatusUnauthorized, map[string]any{"error": "webhook not configured"})
			return
		}
	} else if !verifyMetaSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		h.logger.Warn("meta webhook signature invalid")
		httpapi.JSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid signature"})
		return
	}

	var payload metaLeadgenWebhook
	if err := json.Unmarshal(body, &payload); err != nil {
		h.logger.Warn("invalid meta webhook payload", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}

	// Process every leadgen entry. Meta expects a fast 200 regardless of errors.
	processed := 0
	for _, entry := range payload.Entry {
		for _, ch := range entry.Changes {
			if ch.Field != "leadgen" {
				continue
			}
			pageID := ch.Value.PageID
			if pageID == "" {
				pageID = entry.ID
			}
			tenantID, err := h.resolveTenantByPageID(r.Context(), pageID)
			if err != nil {
				h.logger.Warn("no tenant for page",
					zap.String("page_id", pageID), zap.Error(err))
				continue
			}
			input := leads.CreateInput{
				TenantID: tenantID,
				Source:   "meta_ad",
				LeadType: "customer",
				Metadata: map[string]any{
					"webhook_source":   "meta_leadgen",
					"webhook_received": time.Now().UTC().Format(time.RFC3339),
					"meta_leadgen_id":  ch.Value.LeadgenID,
					"meta_form_id":     ch.Value.FormID,
					"meta_page_id":     pageID,
					"meta_ad_id":       ch.Value.AdID,
					"meta_campaign_id": ch.Value.CampaignID,
				},
			}
			// Meta redelivers webhooks; one leadgen id is one lead.
			if h.leadExists(r.Context(), tenantID, ch.Value.LeadgenID) {
				processed++
				continue
			}
			// Link our Campaign: by Meta's campaign id when the webhook carries it, else by the
			// boost's ad or instant form (leadgen webhooks usually carry only those).
			if cid, ver, ok := h.campaignForLead(r.Context(), tenantID, ch.Value.CampaignID, ch.Value.AdID, ch.Value.FormID); ok {
				input.CampaignID = &cid
				if ver != nil {
					input.Metadata["creative_version_id"] = ver.String()
				}
			}
			if ch.Value.AdgroupID != "" {
				input.Metadata["adset_id"] = ch.Value.AdgroupID
			}
			// Only ids are stored here so Meta gets a fast 200. The lead service publishes
			// lead.created, and the worker's MetaLeadEnricher fetches /{leadgen_id} with the
			// Page token to fill in name, email, phone and answers, then creates the Contact.
			if _, err := h.leadService.Create(r.Context(), input); err != nil {
				h.logger.Error("create lead from meta webhook",
					zap.String("tenant", tenantID.String()), zap.Error(err))
				continue
			}
			processed++
		}
	}

	// The Page and Instagram objects send every subscribed field to this one callback, so the
	// same body may also carry comments and DMs.
	inboxSaved := 0
	if h.inbox != nil {
		inboxSaved = h.inbox.Store(r.Context(), ParseInbox(body))
	}
	h.logger.Info("meta webhook processed",
		zap.Int("leads", processed),
		zap.Int("inbox", inboxSaved),
		zap.Int("entries", len(payload.Entry)),
	)
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ok", "processed": processed})
}

// verifyMetaSignature checks X-Hub-Signature-256 ("sha256=<hex>") over the raw body.
func verifyMetaSignature(secret string, body []byte, header string) bool {
	if secret == "" || len(header) < len("sha256=") {
		return false
	}
	given := header[len("sha256="):]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(given))
}

// resolveTenantByPageID looks up the Tenant by its linked Meta page_id.
// Requires Tenant.meta_page_id field (new; add via Ent schema + migration).
// leadExists reports whether this tenant already has the lead for a Meta leadgen id.
func (h *MetaHandler) leadExists(ctx context.Context, tenantID uuid.UUID, leadgenID string) bool {
	if h.db == nil || leadgenID == "" {
		return false
	}
	var one int
	err := h.db.QueryRowContext(ctx,
		`SELECT 1 FROM leads
		  WHERE tenant_id = $1 AND source = 'meta_ad' AND metadata->>'meta_leadgen_id' = $2
		  LIMIT 1`, tenantID, leadgenID,
	).Scan(&one)
	return err == nil
}

// campaignForLead finds the Campaign a Meta lead came from, with the creative version a boost
// ran: by Meta's campaign id first, then by the boost's ad id or instant form id, which the ads
// worker saves in metadata.launch.state. A tenant has few campaigns, so the jsonb match is cheap.
func (h *MetaHandler) campaignForLead(ctx context.Context, tenantID uuid.UUID, metaCampaignID, adID, formID string) (uuid.UUID, *uuid.UUID, bool) {
	if h.db == nil {
		return uuid.Nil, nil, false
	}
	if id, ok := h.campaignByMetaID(ctx, tenantID, metaCampaignID); ok {
		var ver *uuid.UUID
		_ = h.db.QueryRowContext(ctx, `SELECT creative_version_id FROM campaigns WHERE id = $1`, id).Scan(&ver)
		return id, ver, true
	}
	if adID == "" && formID == "" {
		return uuid.Nil, nil, false
	}
	var id uuid.UUID
	var ver *uuid.UUID
	err := h.db.QueryRowContext(ctx,
		`SELECT id, creative_version_id FROM campaigns
		  WHERE tenant_id = $1
		    AND ((metadata->'launch'->'state'->>'meta_ad_id' = $2 AND $2 <> '')
		      OR (metadata->'launch'->'state'->>'meta_lead_form_id' = $3 AND $3 <> ''))
		  LIMIT 1`, tenantID, adID, formID,
	).Scan(&id, &ver)
	return id, ver, err == nil
}

// campaignByMetaID finds the tenant's Campaign that Meta's numeric campaign id belongs to.
func (h *MetaHandler) campaignByMetaID(ctx context.Context, tenantID uuid.UUID, metaCampaignID string) (uuid.UUID, bool) {
	if h.db == nil || metaCampaignID == "" {
		return uuid.Nil, false
	}
	var id uuid.UUID
	err := h.db.QueryRowContext(ctx,
		`SELECT id FROM campaigns WHERE tenant_id = $1 AND meta_campaign_id = $2 LIMIT 1`,
		tenantID, metaCampaignID,
	).Scan(&id)
	return id, err == nil
}

func (h *MetaHandler) resolveTenantByPageID(ctx context.Context, pageID string) (uuid.UUID, error) {
	if h.db == nil {
		return uuid.Nil, fmt.Errorf("db not configured")
	}
	var id uuid.UUID
	// Every Page a tenant connected is a ConnectedAccount row, so leads from any of them are
	// attributed, not only from the default Page. If two tenants connected the same Page, the
	// latest connect wins.
	err := h.db.QueryRowContext(ctx,
		`SELECT tenant_id FROM connected_accounts
		  WHERE external_id = $1 AND account_type = 'facebook_page' AND status = 'active'
		  ORDER BY updated_at DESC
		  LIMIT 1`, pageID,
	).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, err
	}
	// Tenants that connected before ConnectedAccount; the metadata fallback covers rows
	// written before the meta_page_id column existed.
	err = h.db.QueryRowContext(ctx,
		`SELECT id FROM tenants
		  WHERE meta_page_id = $1
		     OR metadata->>'meta_page_id' = $1
		  LIMIT 1`, pageID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}
