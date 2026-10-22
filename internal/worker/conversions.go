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

	"github.com/bengobox/marketflow-api/internal/encryption"
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
	keys   *encryption.KeyProvider
	logger *zap.Logger
	http   *http.Client
}

func (*ConversionsHandler) Subject() string { return "conversion.occurred" }
func (*ConversionsHandler) Durable() string { return "marketflow-worker-conversions" }

func (h *ConversionsHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	var ev ConversionEvent
	if err := json.Unmarshal(eventBody(msg.Data()), &ev); err != nil {
		return dropPoison(h.logger, msg, "decode conversion event", err)
	}
	if ev.TenantID == "" || ev.EventName == "" {
		return dropPoison(h.logger, msg, "missing tenant_id or event_name",
			fmt.Errorf("conversion event missing tenant_id or event_name"))
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

// dispatchGoogleAdsConversion uploads an offline conversion to Google Ads via
// the REST endpoint. Requires:
//
//   - Platform provider row google_ads/oauth { client_id, client_secret,
//     developer_token } — the app-level credentials shared by every tenant.
//   - Tenant provider row google_ads/oauth { refresh_token, customer_id,
//     login_customer_id? } — the FBO's Google Ads account OAuth grant.
//   - Tenant provider row google_ads/conversion_actions { {EventName}: {action_resource_name} }
//     — per-tenant mapping from logical event names ("Lead", "Purchase") to
//     Google Ads ConversionAction resource names
//     (`customers/{cid}/conversionActions/{id}`).
//
// We call the REST API directly rather than pulling in the Google Ads Go
// client library because (a) the client library requires gRPC + proto
// generation, (b) the single endpoint we need has a straightforward JSON
// shape, and (c) this keeps the marketflow-api binary ~30 MB smaller.
func (h *ConversionsHandler) dispatchGoogleAdsConversion(ctx context.Context, ev *ConversionEvent) error {
	if ev.Gclid == "" {
		return nil // no gclid → no attribution possible
	}
	// Tenant OAuth + customer id (per-FBO).
	tenant, err := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "oauth")
	if err != nil || tenant["customer_id"] == "" || tenant["refresh_token"] == "" {
		return nil
	}
	// Platform app credentials + developer token (shared).
	platform, err := h.loadTenantProvider(ctx, "platform", "google_ads", "ads")
	if err != nil || platform["client_id"] == "" || platform["client_secret"] == "" || platform["developer_token"] == "" {
		h.logger.Warn("google ads platform credentials missing; skipping conversion")
		return nil
	}
	// Per-tenant EventName → ConversionAction resource.
	actions, _ := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "conversion_actions")
	actionResource := actions[ev.EventName]
	if actionResource == "" {
		return nil // tenant hasn't mapped this event yet
	}

	accessToken, err := exchangeGoogleRefreshToken(ctx, h.httpClient(), platform["client_id"], platform["client_secret"], tenant["refresh_token"])
	if err != nil {
		return fmt.Errorf("google ads token exchange: %w", err)
	}

	customerID := strings.ReplaceAll(tenant["customer_id"], "-", "")
	loginCustomerID := strings.ReplaceAll(firstNonEmptyConv(tenant["login_customer_id"], platform["login_customer_id"]), "-", "")

	// Google Ads REST expects ISO-like datetime with `±HH:MM` offset, e.g.
	// "2026-04-16 09:30:00+03:00". Parse from RFC3339 and reformat.
	convTime, err := formatGoogleAdsTime(ev.OccurredAt)
	if err != nil {
		return fmt.Errorf("format time: %w", err)
	}

	payload := map[string]any{
		"conversions": []map[string]any{{
			"gclid":              ev.Gclid,
			"conversionAction":   actionResource,
			"conversionDateTime": convTime,
			"conversionValue":    ev.Value,
			"currencyCode":       firstNonEmptyConv(ev.Currency, "USD"),
		}},
		"partialFailure": true,
	}
	body, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("https://googleads.googleapis.com/v19/customers/%s:uploadClickConversions", customerID)
	req, _ := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("developer-token", platform["developer_token"])
	req.Header.Set("Content-Type", "application/json")
	if loginCustomerID != "" {
		req.Header.Set("login-customer-id", loginCustomerID)
	}

	resp, err := h.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("google ads %d: %s", resp.StatusCode, string(rb))
	}

	h.logger.Info("google ads conversion uploaded",
		zap.String("tenant", ev.TenantID),
		zap.String("event", ev.EventName),
		zap.String("action", actionResource),
	)
	return nil
}

// exchangeGoogleRefreshToken trades a long-lived refresh_token for a short-
// lived access_token. Access tokens are valid for ~3600s; caching is out of
// scope for MVP — one token exchange per conversion event is acceptable
// given the expected volume.
func exchangeGoogleRefreshToken(ctx context.Context, c *http.Client, clientID, clientSecret, refreshToken string) (string, error) {
	form := strings.NewReader(fmt.Sprintf(
		"client_id=%s&client_secret=%s&refresh_token=%s&grant_type=refresh_token",
		clientID, clientSecret, refreshToken,
	))
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://oauth2.googleapis.com/token", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("oauth %d: %s", resp.StatusCode, string(body))
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}
	return parsed.AccessToken, nil
}

func formatGoogleAdsTime(occurredAt string) (string, error) {
	t, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		// Fall back to now if the caller sent an invalid timestamp.
		t = time.Now()
	}
	// Google Ads wants "2006-01-02 15:04:05-07:00".
	return t.Format("2006-01-02 15:04:05-07:00"), nil
}

func firstNonEmptyConv(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ── helpers ───────────────────────────────────────────────────────────────

func (h *ConversionsHandler) loadTenantProvider(ctx context.Context, tenantID, ptype, pname string) (map[string]string, error) {
	return loadProviderSettings(ctx, h.db, h.keys, tenantID, ptype, pname)
}

// loadProviderSettings returns one provider's active settings for a tenant (or "platform") as
// key/value pairs, with secrets decrypted.
func loadProviderSettings(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, tenantID, ptype, pname string) (map[string]string, error) {
	rows, err := db.ProviderSetting.Query().
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
		// Tokens (Meta CAPI access_token, Google Ads refresh_token) are encrypted at rest.
		v, err := keys.SettingValue(ctx, db, r)
		if err != nil {
			return nil, fmt.Errorf("decrypt %s/%s %s for tenant %s: %w", ptype, pname, r.Key, tenantID, err)
		}
		out[r.Key] = v
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
