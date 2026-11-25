package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/modules/adsetup"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/platform/adconversions"
)

// ConversionEvent is the conversion.occurred payload. The worker sends it to every connected ad
// platform of the tenant.
type ConversionEvent = adconversions.Event

// ConversionsHandler is the NATS consumer. Dispatches to Meta CAPI + Google
// Ads in parallel; failures are logged but don't block each other.
type ConversionsHandler struct {
	cfg    *Config
	db     *ent.Client
	keys   *encryption.KeyProvider
	logger *zap.Logger
	http   *http.Client

	// Google's hosts and platform settings; empty for Google's own and the provider row (tests set fakes).
	googleOAuth     google.OAuth
	googleAdsBase   string
	dataManagerBase string
	googleSettings  func(context.Context) (map[string]string, error)
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
	if token == "" {
		// The ads setup assistant saves the pixel and the ad account, not a token: the connection's
		// own token is used, so a reconnect renews it.
		token = h.metaAccountToken(ctx, ev.TenantID, settings[adsetup.CAPIAccountKey])
	}
	// A click-to-WhatsApp sale is matched by ctwa_clid on the WhatsApp dataset
	// (POST /{dataset_id}/events, action_source business_messaging); a website event would not
	// match the ad. Without the dataset it is skipped rather than sent wrongly.
	messaging := ev.CtwaClid != ""
	if messaging {
		pixelID = settings["dataset_id"]
		if pixelID == "" || settings["waba_id"] == "" {
			h.logger.Info("ctwa conversion skipped: no WhatsApp dataset configured", zap.String("tenant", ev.TenantID))
			return nil
		}
	}
	if pixelID == "" || token == "" {
		return nil
	}
	apiVer := settings["graph_version"]
	if apiVer == "" {
		apiVer = meta.GraphVersion
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

	row := metaCAPIRow(ev, eventTime, userData, settings["waba_id"])
	payload := map[string]any{"data": []map[string]any{row}}
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

// metaAccountToken is the token of the tenant's active Meta ad account ("" when it cannot be read).
func (h *ConversionsHandler) metaAccountToken(ctx context.Context, tenantID, actID string) string {
	tid, err := uuid.Parse(tenantID)
	if err != nil || actID == "" || h.keys == nil {
		return ""
	}
	a, err := h.db.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("meta_ad_account"),
			connectedaccount.ExternalIDEQ(actID), connectedaccount.StatusEQ("active")).Only(ctx)
	if err != nil || a.TokenCiphertext == "" {
		h.logger.Warn("meta capi: the ad account needs connecting again", zap.String("tenant", tenantID), zap.Error(err))
		return ""
	}
	tok, err := h.keys.Decrypt(ctx, a.TokenCiphertext)
	if err != nil {
		h.logger.Warn("meta capi: ad account token unreadable", zap.String("tenant", tenantID), zap.Error(err))
		return ""
	}
	return tok
}

// ── Google Ads Conversion Upload ─────────────────────────────────────────

// dispatchGoogleAdsConversion sends an offline conversion to Google through the Data Manager API,
// or the older Google Ads API when the platform switch says so (googleads.UploadAPI). Requires a
// connected Google Ads account, or the older rows:
//
//   - Platform provider row google_ads/ads { client_id, client_secret }: the app-level
//     credentials shared by every tenant (no developer token since 2026-09-09).
//   - Tenant provider row google_ads/oauth { refresh_token, customer_id,
//     login_customer_id? }: the tenant's Google Ads account OAuth grant.
//   - Tenant provider row google_ads/conversion_actions { {EventName}: {action_resource_name} }:
//     per-tenant mapping from logical event names ("Lead", "Purchase") to
//     Google Ads ConversionAction resource names
//     (`customers/{cid}/conversionActions/{id}`).
//
// Both are called over REST rather than with Google's client libraries: no gRPC and proto code,
// and a smaller binary.
func (h *ConversionsHandler) dispatchGoogleAdsConversion(ctx context.Context, ev *ConversionEvent) error {
	if ev.Gclid == "" && ev.Wbraid == "" && ev.Gbraid == "" {
		return nil // no Google click id, so no attribution possible
	}
	api := googleads.UploadAPI(ctx, h.db)
	if api == googleads.UploadAPIGoogleAds && ev.Gclid == "" {
		return nil // the older call is kept as it was: gclid only
	}
	// Since 2026-11-19 the business connects its Google Ads account in MarketFlow (one
	// ConnectedAccount per account, event mapping in its metadata). Those accounts are used
	// first; the older tenant provider rows below stay as a fallback for tenants set up by hand.
	if sent, err := h.uploadToConnectedAccounts(ctx, ev, api); sent || err != nil {
		return err
	}
	// Tenant OAuth + customer id (per-FBO).
	tenant, err := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "oauth")
	if err != nil || tenant["customer_id"] == "" || tenant["refresh_token"] == "" {
		return nil
	}
	// Platform app credentials (shared). No developer token since Google retired them.
	platform, err := h.loadTenantProvider(ctx, "platform", "google_ads", "ads")
	if err != nil || platform["client_id"] == "" || platform["client_secret"] == "" {
		h.logger.Warn("google ads platform credentials missing; skipping conversion")
		return nil
	}
	// Per-tenant EventName → ConversionAction resource.
	actions, _ := h.loadTenantProvider(ctx, ev.TenantID, "google_ads", "conversion_actions")
	actionResource := actions[ev.EventName]
	if actionResource == "" {
		return nil // tenant hasn't mapped this event yet
	}

	oauth := h.googleOAuth
	if oauth.HTTP == nil {
		oauth.HTTP = h.httpClient()
	}
	tok, err := oauth.Refresh(ctx, google.Client{ID: platform["client_id"], Secret: platform["client_secret"]}, tenant["refresh_token"])
	if err != nil {
		return fmt.Errorf("google ads token exchange: %w", err)
	}
	// Same caller as the connected-account path. These rows have no account to keep an outcome on,
	// so it is in the log only.
	requestID, err := h.sendToGoogle(ctx, ev, api, googleTarget{Access: tok.Access, Scopes: tok.Scopes, Settings: platform,
		CustomerID: tenant["customer_id"], Login: firstNonEmptyConv(tenant["login_customer_id"], platform["login_customer_id"]),
		Action: actionResource, Currency: firstNonEmptyConv(ev.Currency, "USD")})
	if err != nil {
		plain, _ := google.PlainUploadError(err)
		return fmt.Errorf("%s (%w)", plain, err)
	}

	h.logger.Info("google ads conversion uploaded",
		zap.String("tenant", ev.TenantID),
		zap.String("event", ev.EventName),
		zap.String("action", actionResource),
		zap.String("request", requestID),
	)
	return nil
}

// googleTarget is where and how one conversion is sent.
type googleTarget struct {
	Access     string
	Scopes     []string          // granted scopes, empty when unknown
	Settings   map[string]string // the platform google_ads/ads row
	CustomerID string
	Login      string // manager account, "" for direct access
	Action     string // customers/{id}/conversionActions/{id}
	Currency   string
}

// sendToGoogle sends one conversion through the configured API. requestID is Data Manager's, whose
// result is read later; the Google Ads API answers at once and gives none.
func (h *ConversionsHandler) sendToGoogle(ctx context.Context, ev *ConversionEvent, api string, t googleTarget) (requestID string, err error) {
	at := conversionTime(ev.OccurredAt)
	if api == googleads.UploadAPIGoogleAds {
		ads := google.AdsFromSettings(h.httpClient(), t.Settings, t.Access)
		ads.LoginCustomerID = google.CustomerDigits(t.Login)
		if h.googleAdsBase != "" {
			ads.Base = h.googleAdsBase
		}
		return "", ads.UploadClickConversions(ctx, t.CustomerID, []google.ClickConversion{{GCLID: ev.Gclid, ConversionAction: t.Action,
			DateTime: at.Format("2006-01-02 15:04:05-07:00"), Value: ev.Value, Currency: t.Currency}})
	}
	if !googleads.MaySendConversions(api, t.Scopes) {
		return "", google.ErrReconnect
	}
	dm := google.DataManager{HTTP: h.httpClient(), Base: h.dataManagerBase, AccessToken: t.Access}
	id, warnings, err := dm.SendConversion(ctx, google.OfflineConversion{ConversionAction: t.Action, LoginCustomerID: t.Login,
		GCLID: ev.Gclid, WBRAID: ev.Wbraid, GBRAID: ev.Gbraid, TransactionID: ev.ConversionID,
		At: at, Value: ev.Value, Currency: t.Currency})
	if len(warnings) > 0 {
		h.logger.Warn("google ignored parts of a conversion", zap.String("tenant", ev.TenantID), zap.String("request", id), zap.Strings("warnings", warnings))
	}
	return id, err
}

// conversionTime reads the event's time, now when the caller sent an invalid one.
func conversionTime(occurredAt string) time.Time {
	t, err := time.Parse(time.RFC3339, occurredAt)
	if err != nil {
		return time.Now()
	}
	return t
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

// metaEventName is Meta's standard event for a MarketFlow conversion.
func metaEventName(name string) string {
	if name == adconversions.Booking {
		return "Schedule"
	}
	return name
}

// metaCAPIRow builds one Conversions API event. A click-to-WhatsApp sale is a business_messaging
// event matched by ctwa_clid and the WhatsApp Business Account; anything else is a website event
// matched by hashed contact data and fbclid.
func metaCAPIRow(ev *ConversionEvent, eventTime int64, userData map[string]any, wabaID string) map[string]any {
	row := map[string]any{
		"event_name":    metaEventName(ev.EventName),
		"event_time":    eventTime,
		"action_source": "website",
		"event_id":      firstNonEmpty(ev.ConversionID, ev.ClientUserID), // one id per conversion, so a second sale of a lead counts
		"user_data":     userData,
		"custom_data": map[string]any{
			"currency": firstNonEmpty(ev.Currency, "USD"),
			"value":    ev.Value,
		},
	}
	if ev.CtwaClid != "" {
		row["action_source"] = "business_messaging"
		row["messaging_channel"] = "whatsapp"
		row["user_data"] = map[string]any{"whatsapp_business_account_id": wabaID, "ctwa_clid": ev.CtwaClid}
	}
	return row
}

// uploadToConnectedAccounts sends the conversion to every active connected Google Ads account of
// the tenant whose mapping names this event. sent is false when no account maps it, so the caller
// tries the older tenant provider rows. One account's failure does not stop the others.
// Each account keeps the outcome (metadata conversion_upload), so a missing permission or a
// refusal shows in MarketFlow and not only in the log.
func (h *ConversionsHandler) uploadToConnectedAccounts(ctx context.Context, ev *ConversionEvent, api string) (sent bool, err error) {
	tid, perr := uuid.Parse(ev.TenantID)
	if perr != nil {
		return false, nil
	}
	accs, qerr := h.db.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(googleads.AccountType), connectedaccount.StatusEQ("active")).
		Limit(20).All(ctx)
	if qerr != nil {
		return false, fmt.Errorf("google ads accounts: %w", qerr)
	}
	var tokens *google.Tokens
	var extra map[string]string
	var errs []error
	save := func(a *ent.ConnectedAccount, serr error) {
		if serr != nil {
			h.logger.Warn("save conversion outcome", zap.String("account", a.ExternalID), zap.Error(serr))
		}
	}
	for _, a := range accs {
		action := googleads.MappingOf(a)[ev.EventName]
		if action == "" {
			continue
		}
		if tokens == nil {
			load := h.googleSettings
			if load == nil {
				load = func(ctx context.Context) (map[string]string, error) {
					_, x, lerr := google.LoadClient(ctx, h.db, h.keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
					return x, lerr
				}
			}
			x, lerr := load(ctx)
			if lerr != nil {
				h.logger.Warn("google ads platform client missing; skipping conversion", zap.Error(lerr))
				return true, nil
			}
			extra = x
			tokens = google.NewTokensFor(h.db, h.keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
		}
		sent = true
		access, aerr := tokens.AccessToken(ctx, a)
		if aerr != nil {
			save(a, googleads.SaveUpload(ctx, h.db, a.ID, googleads.Failure(aerr, ev.EventName, time.Now())))
			errs = append(errs, fmt.Errorf("account %s token: %w", a.ExternalID, aerr))
			continue
		}
		t := googleTarget{Access: access, Scopes: a.Scopes, Settings: extra, CustomerID: a.ExternalID, Action: action}
		if api == googleads.UploadAPIDataManager {
			t.Login = googleads.LoginCustomer(a)
		}
		cur, _ := a.Metadata["currency"].(string)
		t.Currency = firstNonEmptyConv(ev.Currency, cur, "USD")
		requestID, uerr := h.sendToGoogle(ctx, ev, api, t)
		now := time.Now()
		if uerr != nil {
			fail := googleads.Failure(uerr, ev.EventName, now)
			save(a, googleads.SaveUpload(ctx, h.db, a.ID, fail))
			errs = append(errs, fmt.Errorf("account %s upload: %s (%w)", a.ExternalID, fail.Reason, uerr))
			continue
		}
		done := googleads.Upload{Status: googleads.UploadAccepted, Event: ev.EventName, At: now.UTC()}
		if requestID != "" {
			done.Status = googleads.UploadSent // Google says later whether it counted
		}
		save(a, googleads.Track(ctx, h.db, a, requestID, firstCheck(now, ev.EventName), done))
		h.logger.Info("google ads conversion uploaded", zap.String("tenant", ev.TenantID), zap.String("event", ev.EventName),
			zap.String("account", a.ExternalID), zap.String("request", requestID))
	}
	return sent, errors.Join(errs...)
}
