package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// ConversionEvent is the NATS payload published by marketflow-api when a
// tenant-defined goal is achieved (lead_submit, meeting_booked,
// purchase_completed, etc.). The worker fans it out to every connected ad
// platform using that tenant's stored OAuth tokens.
//
// Publishers: marketflow-api when it observes a matching Lead / Meeting /
// Order event. Subject: `conversion.occurred`.
type ConversionEvent struct {
	TenantID     string  `json:"tenant_id"`
	EventName    string  `json:"event_name"` // Lead | CompleteRegistration | Purchase | etc.
	Value        float64 `json:"value"`
	Currency     string  `json:"currency"`
	OccurredAt   string  `json:"occurred_at"` // RFC3339
	Gclid        string  `json:"gclid,omitempty"`
	Fbclid       string  `json:"fbclid,omitempty"`
	ClientUserID string  `json:"client_user_id,omitempty"` // Lead UUID for dedup
	Email        string  `json:"email,omitempty"`          // Raw — hashed before outbound
	Phone        string  `json:"phone,omitempty"`          // Raw — hashed before outbound
	Source       string  `json:"source,omitempty"`         // lead | meeting | order
}

// ConversionsHandler is the NATS consumer. Dispatches to Meta CAPI + Google
// Ads in parallel; failures are logged but don't block each other.
type ConversionsHandler struct {
	cfg    *Config
	db     *ent.Client
	logger *zap.Logger
	http   *http.Client
}

func (*ConversionsHandler) Subject() string { return "conversion.occurred" }
func (*ConversionsHandler) Durable() string { return "marketflow-worker-conversions" }

func (h *ConversionsHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev ConversionEvent
	if err := json.Unmarshal(msg.Data(), &ev); err != nil {
		return fmt.Errorf("decode conversion event: %w", err)
	}
	if ev.TenantID == "" || ev.EventName == "" {
		return fmt.Errorf("conversion event missing tenant_id or event_name")
	}
	if h.db == nil {
		h.logger.Warn("conversion received but DB not configured; skipping", zap.String("event", ev.EventName))
		return nil
	}

	// Meta CAPI and Google Ads dispatches are independent — errors on one
	// don't block the other. Both are rate-limit + back-off handled by the
	// provider clients.
	if err := h.dispatchMetaCAPI(ctx, &ev); err != nil {
		h.logger.Warn("meta capi dispatch failed",
			zap.String("tenant", ev.TenantID), zap.Error(err))
	}
	if err := h.dispatchGoogleAdsConversion(ctx, &ev); err != nil {
		h.logger.Warn("google ads conversion dispatch failed",
			zap.String("tenant", ev.TenantID), zap.Error(err))
	}
	return nil
}

// ── Meta CAPI ─────────────────────────────────────────────────────────────

// dispatchMetaCAPI sends a server-side event to Meta's Conversions API.
// Requires per-tenant `access_token` + `pixel_id` stored on ProviderSetting
// rows with provider_type=meta_ads, provider_name=capi.
func (h *ConversionsHandler) dispatchMetaCAPI(ctx context.Context, ev *ConversionEvent) error {
	settings, err := h.loadTenantProvider(ctx, ev.TenantID, "meta_ads", "capi")
	if err != nil {
		return nil // tenant hasn't enabled Meta CAPI — not an error
	}
	pixelID := settings["pixel_id"]
	token := settings["access_token"]
	if pixelID == "" || token == "" {
		return nil
	}
	apiVer := settings["graph_version"]
	if apiVer == "" {
		apiVer = "v21.0"
	}

	eventTime := time.Now().Unix()
	if t, err := time.Parse(time.RFC3339, ev.OccurredAt); err == nil {
		eventTime = t.Unix()
	}

	userData := map[string]any{}
	if ev.Email != "" {
		userData["em"] = []string{hashLower(ev.Email)}
	}
	if ev.Phone != "" {
		userData["ph"] = []string{hashDigits(ev.Phone)}
	}
	if ev.Fbclid != "" {
		userData["fbc"] = fmt.Sprintf("fb.1.%d.%s", eventTime, ev.Fbclid)
	}

	payload := map[string]any{
		"data": []map[string]any{
			{
				"event_name":      ev.EventName,
				"event_time":      eventTime,
				"action_source":   "website",
				"event_id":        ev.ClientUserID, // Meta dedups against pixel browser events
				"user_data":       userData,
				"custom_data": map[string]any{
					"currency": firstNonEmpty(ev.Currency, "USD"),
					"value":    ev.Value,
				},
			},
		},
	}
	body, _ := json.Marshal(payload)

	url := fmt.Sprintf("https://graph.facebook.com/%s/%s/events?access_token=%s",
		apiVer, pixelID, token)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("meta capi %d: %s", resp.StatusCode, truncate(string(b), 200))
	}
	h.logger.Info("meta capi event sent",
		zap.String("tenant", ev.TenantID),
		zap.String("event", ev.EventName),
	)
	return nil
}

// ── Google Ads Conversion Upload ─────────────────────────────────────────

// dispatchGoogleAdsConversion uploads an offline conversion via the Google
// Ads API. Requires tenant ProviderSetting rows with provider_type=google_ads,
// provider_name=oauth (for refresh_token + customer_id + conversion_action).
// This is a placeholder HTTP call — the real implementation uses the Google
// Ads client library's ConversionUploadService once the Standard-access
// developer token is provisioned (see
// docs/integrations/google-ads-api-access-design.md).
func (h *ConversionsHandler) dispatchGoogleAdsConversion(ctx context.Context, ev *ConversionEvent) error {
	if ev.Gclid == "" {
		return nil // no gclid → no attribution possible
	}
	settings, err := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "oauth")
	if err != nil || settings["customer_id"] == "" || settings["refresh_token"] == "" {
		return nil
	}
	// Conversion actions are mapped per tenant in a provider_name=conversion_actions row.
	actions, _ := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "conversion_actions")
	actionResource := actions[ev.EventName]
	if actionResource == "" {
		return nil // tenant hasn't mapped this event to a Google Ads conversion action yet
	}

	h.logger.Info("google ads conversion queued",
		zap.String("tenant", ev.TenantID),
		zap.String("event", ev.EventName),
		zap.String("customer_id", settings["customer_id"]),
		zap.String("action", actionResource),
	)

	// TODO: real implementation requires the Google Ads Go client:
	//   import "google.golang.org/api/googleads/v19/services"
	//   svc.ConversionUploadService.UploadClickConversions(ctx, &UploadClickConversionsRequest{
	//     CustomerId: settings["customer_id"],
	//     Conversions: []*ClickConversion{{
	//       Gclid:              ev.Gclid,
	//       ConversionAction:   actionResource,
	//       ConversionDateTime: formatGoogleAdsTime(ev.OccurredAt),
	//       ConversionValue:    ev.Value,
	//       CurrencyCode:       ev.Currency,
	//     }},
	//     PartialFailure: true,
	//   })
	// The design + compliance posture is documented in
	// docs/integrations/google-ads-api-access-design.md; flip from this stub
	// to the real client once Google grants Standard access.
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────

func (h *ConversionsHandler) loadTenantProvider(ctx context.Context, tenantID, ptype, pname string) (map[string]string, error) {
	rows, err := h.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ(ptype),
			providersetting.ProviderNameEQ(pname),
			providersetting.IsActiveEQ(true),
		).All(ctx)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("no active %s/%s for tenant %s", ptype, pname, tenantID)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out, nil
}

func (h *ConversionsHandler) httpClient() *http.Client {
	if h.http == nil {
		h.http = &http.Client{Timeout: 15 * time.Second}
	}
	return h.http
}

// Meta CAPI requires SHA-256-hashed lowercase emails / digit-only phones.
func hashLower(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:])
}
func hashDigits(s string) string {
	var b strings.Builder
	for _, c := range s {
		if c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
