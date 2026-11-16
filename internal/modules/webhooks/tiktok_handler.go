package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
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
	leadService   *leads.Service
	db            *sql.DB
	sharedSecret  string
	signingSecret string
	allowOpen     bool
	logger        *zap.Logger
	now           func() time.Time
}

// TikTokConfig holds configuration for the TikTok webhook handler.
type TikTokConfig struct {
	DB           *sql.DB
	SharedSecret string // shared secret the TikTok webhook URL carries (?secret=)
	// SigningSecret is the key of the TikTok-Signature header (the TikTok app's client secret).
	// When it is set and an event carries the header, the signature decides; the query secret is
	// the fallback for events without one.
	SigningSecret string
	// AllowUnsigned accepts events when SharedSecret is empty; set only in development.
	AllowUnsigned bool
}

// NewTikTokHandler creates a new TikTok webhook handler.
func NewTikTokHandler(leadService *leads.Service, cfg TikTokConfig, logger *zap.Logger) *TikTokHandler {
	return &TikTokHandler{
		leadService:   leadService,
		db:            cfg.DB,
		sharedSecret:  cfg.SharedSecret,
		signingSecret: cfg.SigningSecret,
		allowOpen:     cfg.AllowUnsigned,
		logger:        logger.Named("webhooks.tiktok"),
		now:           time.Now,
	}
}

// maxWebhookBody is the largest webhook body read (audit M3).
const maxWebhookBody = 1 << 20

// readWebhookBody reads at most maxWebhookBody bytes; tooLarge is set when the body is bigger.
func readWebhookBody(w http.ResponseWriter, r *http.Request) (body []byte, tooLarge bool, err error) {
	body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	var mbe *http.MaxBytesError
	return body, errors.As(err, &mbe), err
}

// tiktokSignatureHeader carries "t=<unix seconds>,s=<hex HMAC-SHA256 of "<t>.<body>">".
const (
	tiktokSignatureHeader = "TikTok-Signature"
	tiktokSignatureSkew   = 5 * time.Minute
)

// verifyTikTokSignature checks a TikTok-Signature header against the raw body.
func verifyTikTokSignature(header string, body []byte, secret string, now time.Time) bool {
	if secret == "" {
		return false
	}
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "t":
			ts = v
		case "s":
			sig = v
		}
	}
	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || sig == "" {
		return false
	}
	if d := now.Sub(time.Unix(sec, 0)); d > tiktokSignatureSkew || d < -tiktokSignatureSkew {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(strings.ToLower(sig)))
}

// authorised decides whether an event is TikTok's: a valid signature first, else the query
// secret. It never logs either secret.
func (h *TikTokHandler) authorised(r *http.Request, body []byte) (bool, string) {
	if sig := r.Header.Get(tiktokSignatureHeader); sig != "" && h.signingSecret != "" {
		if !verifyTikTokSignature(sig, body, h.signingSecret, h.now()) {
			return false, "invalid signature"
		}
		return true, ""
	}
	if h.sharedSecret == "" {
		if !h.allowOpen {
			return false, "webhook not configured"
		}
		return true, ""
	}
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(h.sharedSecret)) != 1 {
		return false, "invalid secret"
	}
	return true, ""
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
	body, tooLarge, err := readWebhookBody(w, r)
	if tooLarge {
		httpapi.JSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "body too large"})
		return
	}
	if err != nil {
		h.logger.Warn("read body", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "received"})
		return
	}
	// The TikTok-Signature header when the event has one, else the shared secret in the query.
	// With neither configured the endpoint is closed (fail closed), except in local development:
	// an open endpoint lets anyone create leads.
	if ok, why := h.authorised(r, body); !ok {
		h.logger.Warn("tiktok webhook refused", zap.String("reason", why))
		httpapi.JSON(w, http.StatusUnauthorized, map[string]any{"error": why})
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

	if h.leadExists(ctx, tenantID, payload.Lead.LeadID) {
		httpapi.JSON(w, http.StatusOK, map[string]any{"status": "ok", "duplicate": true})
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

// resolveTenantByAdvertiserID finds the tenant that connected this TikTok advertiser (a
// tiktok_advertiser ConnectedAccount written by the TikTok OAuth callback). A revoked link does
// not count; an expired token still does, since the lead is the tenant's either way. When two
// tenants connected the same advertiser the lead is refused rather than given to a guess.
func (h *TikTokHandler) resolveTenantByAdvertiserID(ctx context.Context, advertiserID string) (uuid.UUID, error) {
	if h.db == nil {
		return uuid.Nil, fmt.Errorf("db not configured")
	}
	if advertiserID == "" {
		return uuid.Nil, fmt.Errorf("no advertiser id")
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT DISTINCT tenant_id FROM connected_accounts
		  WHERE platform = 'tiktok' AND account_type = 'tiktok_advertiser' AND external_id = $1 AND status <> 'revoked'
		  LIMIT 2`, advertiserID)
	if err != nil {
		return uuid.Nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return uuid.Nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return uuid.Nil, err
	}
	switch len(ids) {
	case 0:
		return uuid.Nil, fmt.Errorf("no tenant connected advertiser %s", advertiserID)
	case 1:
		return ids[0], nil
	default:
		return uuid.Nil, fmt.Errorf("advertiser %s is connected by more than one tenant", advertiserID)
	}
}

// leadExists reports whether this tenant already has the lead for a TikTok lead id, so a
// redelivered webhook does not create a second lead.
func (h *TikTokHandler) leadExists(ctx context.Context, tenantID uuid.UUID, leadID string) bool {
	if h.db == nil || leadID == "" {
		return false
	}
	var one int
	err := h.db.QueryRowContext(ctx,
		`SELECT 1 FROM leads WHERE tenant_id = $1 AND source = 'tiktok' AND metadata->>'tiktok_lead_id' = $2 LIMIT 1`,
		tenantID, leadID,
	).Scan(&one)
	return err == nil
}
