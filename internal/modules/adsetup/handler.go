package adsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/platform/runads"
)

// Connected account types with a checklist (modules/integrations sets them).
const (
	typeGoogle = googleads.AccountType
	typeMeta   = "meta_ad_account"
	typeTikTok = "tiktok_advertiser"
	typePage   = "facebook_page"
)

var platformOf = map[string]string{typeGoogle: "google", typeMeta: "meta", typeTikTok: "tiktok"}

// The tenant's Meta Conversions API settings the conversions worker reads.
const (
	capiType, capiName = "meta_ads", "capi"
	capiPixel          = "pixel_id"
	// CAPIAccountKey names the connected ad account whose token sends the events.
	CAPIAccountKey = "ad_account_id"
)

// Handler serves /api/v1/ads-setup.
type Handler struct {
	DB     *ent.Client
	Keys   *encryption.KeyProvider
	Tokens *google.Tokens
	HTTP   *http.Client
	Meta   *meta.Client
	TikTok *tiktok.Client
	// Web reads the business's own website (public addresses only).
	Web    *http.Client
	Facts  func(ctx context.Context, tenantID uuid.UUID) (marketingos.BusinessFacts, error)
	Logger *zap.Logger
	// CanManage gates the fixes (the approving roles); nil allows nobody.
	CanManage func(ctx context.Context, tenantID uuid.UUID) bool

	mu    sync.Mutex
	cache map[uuid.UUID]cached
	locks sync.Map // account id to *sync.Mutex
}

type cached struct {
	at   time.Time
	list Checklist
}

// Account is one connected ad account.
type Account struct {
	ID         string `json:"connected_account_id"`
	Platform   string `json:"platform"`
	ExternalID string `json:"external_id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
}

// Checklist is an account with its checks.
type Checklist struct {
	Account   Account   `json:"account"`
	Items     []Item    `json:"items"`
	CheckedAt time.Time `json:"checked_at"`
	CanFix    bool      `json:"can_fix"`
}

// errStop is an answer for the caller rather than a failure.
type errStop struct {
	status    int
	code, msg string
}

func (e *errStop) Error() string { return e.msg }

func viewOf(a *ent.ConnectedAccount) Account {
	v := Account{ID: a.ID.String(), Platform: platformOf[a.AccountType], ExternalID: a.ExternalID, Name: a.DisplayName, Status: a.Status}
	if v.Name == "" {
		v.Name = a.ExternalID
	}
	return v
}

// List handles GET /api/v1/ads-setup: the connected ad accounts. It calls no provider.
// @Summary Ad accounts with a setup checklist
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads-setup [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeIn(typeGoogle, typeMeta, typeTikTok), connectedaccount.StatusNEQ("revoked")).
		Order(ent.Asc(connectedaccount.FieldAccountType), ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]Account, 0, len(rows))
	for _, a := range rows {
		out = append(out, viewOf(a))
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// Get handles GET /api/v1/ads-setup/{id}?refresh=1: the account's checklist, read from the provider
// when the page opens and kept for a short while.
// @Summary Ad account setup checklist
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id"
// @Param refresh query string false "1 reads the account again"
// @Success 200 {object} adsetup.Checklist
// @Security bearerAuth
// @Router /ads-setup/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	tid, a, ok := h.load(w, r)
	if !ok {
		return
	}
	cfg := LoadConfig(r.Context(), h.DB, tid)
	if r.URL.Query().Get("refresh") != "1" {
		h.mu.Lock()
		c, hit := h.cache[a.ID]
		h.mu.Unlock()
		// A reconnect or a mapping saved elsewhere changes the row, so an older copy is dropped.
		if hit && time.Since(c.at) < cfg.Cache && !a.UpdatedAt.After(c.at) {
			c.list.CanFix = h.canManage(r.Context(), tid)
			httpapi.JSON(w, http.StatusOK, c.list)
			return
		}
	}
	list, err := h.check(r.Context(), tid, a, cfg)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, list)
}

type fixRequest struct {
	Item  string            `json:"item"`
	Token string            `json:"token"`
	Input map[string]string `json:"input"`
}

// Fix handles POST /api/v1/ads-setup/{id}/fix {"item", "token", "input"}: runs one fix the owner
// has just seen. The account is read again first, and a proposal that changed is refused (409).
// @Summary Run one ad account setup fix
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id"
// @Success 200 {object} adsetup.Checklist
// @Security bearerAuth
// @Router /ads-setup/{id}/fix [post]
func (h *Handler) Fix(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	if !h.canManage(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can change this", nil)
		return
	}
	var in fixRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	_, a, ok := h.load(w, r)
	if !ok {
		return
	}
	// One fix at a time per account, so a double tap cannot create the same thing twice: the second
	// one reads the account after the first has finished and finds nothing to fix.
	l, _ := h.locks.LoadOrStore(a.ID, &sync.Mutex{})
	lock := l.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()
	// Once started, a fix finishes even if the browser goes away.
	ctx := context.WithoutCancel(r.Context())
	a, err := h.DB.ConnectedAccount.Get(ctx, a.ID)
	if err != nil {
		h.fail(w, err)
		return
	}
	cfg := LoadConfig(ctx, h.DB, tid)
	list, err := h.check(ctx, tid, a, cfg)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := runFix(ctx, list.Items, in); err != nil {
		h.record(ctx, tid, a, in, err)
		h.fail(w, err)
		return
	}
	h.record(ctx, tid, a, in, nil)
	if a, err = h.DB.ConnectedAccount.Get(ctx, a.ID); err == nil {
		list, err = h.check(ctx, tid, a, cfg)
	}
	if err != nil {
		h.fail(w, &errStop{http.StatusBadGateway, "recheck_failed", "The fix ran. The account could not be read again; refresh to see it."})
		return
	}
	httpapi.JSON(w, http.StatusOK, list)
}

// runFix runs the named fix when it is still the one the owner saw.
func runFix(ctx context.Context, items []Item, in fixRequest) error {
	for _, it := range items {
		if it.Key != in.Item {
			continue
		}
		if it.Fix == nil || it.Fix.run == nil {
			return &errStop{http.StatusConflict, "nothing_to_fix", "there is nothing to fix here any more"}
		}
		if it.Fix.Token != in.Token {
			return &errStop{http.StatusConflict, "changed", "the account changed since you opened this; look at the fix again"}
		}
		v, err := cleanInput(it.Fix.Fields, in.Input)
		if err != nil {
			return err
		}
		return it.Fix.run(ctx, v)
	}
	return &errStop{http.StatusNotFound, "not_found", "unknown check"}
}

// record writes the fix to the tenant's activity log.
func (h *Handler) record(ctx context.Context, tid uuid.UUID, a *ent.ConnectedAccount, in fixRequest, err error) {
	var stop *errStop
	var bad *InputError
	if errors.As(err, &stop) || errors.As(err, &bad) {
		return // nothing was sent to the provider
	}
	p := map[string]any{"item": in.Item, "platform": platformOf[a.AccountType], "account": a.ExternalID, "input": in.Input, "ok": err == nil}
	if err != nil {
		p["error"] = clip(safeErr(err), 300)
	}
	audit.Tenant(ctx, h.DB, h.Logger, tid, "ads_setup.fix", "connected_account", &a.ID, p)
}

// check reads the account and keeps the result.
func (h *Handler) check(ctx context.Context, tid uuid.UUID, a *ent.ConnectedAccount, cfg Config) (Checklist, error) {
	if a.Status != "active" {
		return Checklist{}, &errStop{http.StatusConflict, "reconnect", "this ad account needs connecting again in Settings, Integrations"}
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	t, err := h.DB.Tenant.Get(ctx, tid)
	if err != nil {
		return Checklist{}, err
	}
	var items []Item
	switch a.AccountType {
	case typeGoogle:
		items, err = h.google(ctx, t, a, cfg)
	case typeMeta:
		items, err = h.meta(ctx, t, a, cfg)
	case typeTikTok:
		items, err = h.tiktok(ctx, t, a, cfg)
	}
	if err != nil {
		return Checklist{}, err
	}
	list := Checklist{Account: viewOf(a), Items: items, CheckedAt: time.Now().UTC(), CanFix: h.canManage(ctx, tid)}
	h.mu.Lock()
	if h.cache == nil || len(h.cache) > 2000 {
		h.cache = map[uuid.UUID]cached{}
	}
	h.cache[a.ID] = cached{at: time.Now(), list: list}
	h.mu.Unlock()
	return list, nil
}

func (h *Handler) google(ctx context.Context, t *ent.Tenant, a *ent.ConnectedAccount, cfg Config) ([]Item, error) {
	_, extra, err := google.LoadClient(ctx, h.DB, h.Keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)
	if err != nil {
		return nil, &errStop{http.StatusServiceUnavailable, "oauth_not_configured", "the platform Google Ads client is not configured"}
	}
	tok, err := h.Tokens.AccessToken(ctx, a)
	if err != nil {
		var te *google.TokenError
		if errors.As(err, &te) && te.Revoked() {
			return nil, &errStop{http.StatusConflict, "reconnect", "Google ended this connection; connect the account again"}
		}
		return nil, fmt.Errorf("google ads token: %w", err)
	}
	ads := google.AdsFromSettings(h.HTTP, extra, tok)
	manager := ads.LoginCustomerID
	// As in Run ads: called as itself, through a manager only when the connection says so.
	ads.LoginCustomerID = googleads.LoginCustomer(a)
	return googleChecklist(ctx, googleInput{
		MaySend: googleads.MaySendConversions(googleads.UploadAPI(ctx, h.DB), a.Scopes), LastUpload: googleads.LastUpload(a),
		ReconnectURL: "/" + t.Slug + "/settings/integrations",
		Ads: ads, CustomerID: a.ExternalID, ManagerID: manager, Mapping: googleads.MappingOf(a),
		SaveMapping: func(ctx context.Context, m map[string]string) error { return h.saveMapping(ctx, a, m) },
		BillingURL:  runads.Platform(ctx, h.DB).GoogleBillingLink(a.ExternalID),
		Consented:   func(ctx context.Context) (int, error) { return h.consented(ctx, t.ID) },
		Profile:     func(ctx context.Context) Profile { return loadProfile(ctx, t, h.facts(t.ID), h.Web) },
		Cfg:         cfg,
	}), nil
}

func (h *Handler) facts(tid uuid.UUID) func(context.Context) (marketingos.BusinessFacts, error) {
	if h.Facts == nil {
		return nil
	}
	return func(ctx context.Context) (marketingos.BusinessFacts, error) { return h.Facts(ctx, tid) }
}

// saveMapping stores the mapping the way googleads.SetMapping does.
func (h *Handler) saveMapping(ctx context.Context, a *ent.ConnectedAccount, m map[string]string) error {
	clean, bad := googleads.CleanMapping(a.ExternalID, m)
	if bad != "" {
		return badInput("%s", bad)
	}
	return googleads.PatchMetadata(ctx, h.DB, a.ID, map[string]any{googleads.MetaConversionActions: clean})
}

// consented counts contacts with an email, the email opt-in and a recorded source for it.
func (h *Handler) consented(ctx context.Context, tid uuid.UUID) (int, error) {
	rows, err := h.DB.Contact.Query().
		Where(contact.TenantID(tid), contact.SubscribedEmail(true), contact.EmailNotNil(), contact.EmailNEQ("")).
		Select(contact.FieldID, contact.FieldMetadata).All(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, c := range rows {
		if s, _ := c.Metadata["consent_source"].(string); strings.TrimSpace(s) != "" && s != "legacy_unverified" {
			n++
		}
	}
	return n, nil
}

func (h *Handler) token(ctx context.Context, a *ent.ConnectedAccount) (string, error) {
	if h.Keys == nil || a.TokenCiphertext == "" {
		return "", errors.New("no token")
	}
	return h.Keys.Decrypt(ctx, a.TokenCiphertext)
}

func (h *Handler) meta(ctx context.Context, t *ent.Tenant, a *ent.ConnectedAccount, cfg Config) ([]Item, error) {
	tok, err := h.token(ctx, a)
	if err != nil || tok == "" {
		return nil, &errStop{http.StatusConflict, "reconnect", "the Meta ad account needs to be connected again"}
	}
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(t.ID), connectedaccount.AccountTypeEQ(typePage), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(5).All(ctx)
	if err != nil {
		return nil, err
	}
	pages := make([]metaPage, 0, len(rows))
	for _, p := range rows {
		pt, _ := h.token(ctx, p)
		pages = append(pages, metaPage{ID: p.ExternalID, Name: p.DisplayName, Token: pt})
	}
	in := metaInput{Client: h.Meta, Token: tok, ActID: a.ExternalID, Scopes: a.Scopes, Pages: pages, Business: t.Name, Now: time.Now(), Cfg: cfg,
		StartSending: func(ctx context.Context, pixel string) error {
			return h.saveSettings(ctx, t.ID, map[string]string{capiPixel: pixel, CAPIAccountKey: a.ExternalID})
		}}
	if row, err := h.DB.ProviderSetting.Query().Where(providersetting.TenantIDEQ(t.ID.String()), providersetting.ProviderTypeEQ(capiType),
		providersetting.ProviderNameEQ(capiName), providersetting.KeyEQ(capiPixel), providersetting.IsActiveEQ(true)).First(ctx); err == nil {
		in.SendingTo = strings.TrimSpace(row.Value)
	} else if !ent.IsNotFound(err) {
		return nil, err
	}
	if s, ok := t.Metadata["socials"].(map[string]any); ok {
		in.Website, _ = s["website"].(string)
	}
	return metaChecklist(ctx, in), nil
}

// saveSettings stores the tenant's Conversions API settings. None of them is a secret.
func (h *Handler) saveSettings(ctx context.Context, tid uuid.UUID, kv map[string]string) error {
	for k, v := range kv {
		row, err := h.DB.ProviderSetting.Query().Where(providersetting.TenantIDEQ(tid.String()), providersetting.ProviderTypeEQ(capiType),
			providersetting.ProviderNameEQ(capiName), providersetting.KeyEQ(k), providersetting.EnvironmentEQ("production")).Only(ctx)
		switch {
		case ent.IsNotFound(err):
			err = h.DB.ProviderSetting.Create().SetTenantID(tid.String()).SetProviderType(capiType).SetProviderName(capiName).
				SetEnvironment("production").SetKey(k).SetValue(v).SetIsSecret(false).SetIsEncrypted(false).
				SetIsActive(true).SetStatus("active").Exec(ctx)
		case err == nil:
			err = h.DB.ProviderSetting.UpdateOne(row).SetValue(v).SetIsSecret(false).SetIsEncrypted(false).
				SetIsActive(true).SetStatus("active").Exec(ctx)
		}
		if err != nil {
			return fmt.Errorf("save %s: %w", k, err)
		}
	}
	return nil
}

func (h *Handler) tiktok(ctx context.Context, t *ent.Tenant, a *ent.ConnectedAccount, cfg Config) ([]Item, error) {
	tok, err := h.token(ctx, a)
	if err != nil || tok == "" {
		return nil, &errStop{http.StatusConflict, "reconnect", "the TikTok ad account needs to be connected again"}
	}
	s := runads.Platform(ctx, h.DB)
	return tiktokChecklist(ctx, tiktokInput{Client: h.TikTok, Token: tok, AdvertiserID: a.ExternalID, Business: t.Name,
		HomeURL: s.TikTokLink("home", a.ExternalID), PaymentURL: s.TikTokLink("payment", a.ExternalID), Cfg: cfg}), nil
}

func (h *Handler) canManage(ctx context.Context, tid uuid.UUID) bool {
	return h.CanManage != nil && h.CanManage(ctx, tid)
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (uuid.UUID, *ent.ConnectedAccount, bool) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return uuid.Nil, nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "account id required", nil)
		return uuid.Nil, nil, false
	}
	a, err := h.DB.ConnectedAccount.Query().Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid),
		connectedaccount.AccountTypeIn(typeGoogle, typeMeta, typeTikTok)).Only(r.Context())
	if err != nil {
		h.fail(w, err)
		return uuid.Nil, nil, false
	}
	return tid, a, true
}

// fail answers in plain words. A provider's refusal is shown as it was given.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	var stop *errStop
	var bad *InputError
	var ge *google.AdsError
	var me *meta.GraphError
	var te *tiktok.APIError
	switch {
	case errors.As(err, &stop):
		httpapi.Error(w, stop.status, stop.code, stop.msg, nil)
	case errors.As(err, &bad):
		httpapi.Error(w, http.StatusBadRequest, "invalid_input", bad.Msg, nil)
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusNotFound, "not_found", "ad account not found", nil)
	case errors.As(err, &ge) && ge.Status < 500:
		httpapi.Error(w, http.StatusBadGateway, "provider_refused", "Google refused: "+ge.Message, nil)
	case errors.As(err, &me) && me.Status < 500:
		httpapi.Error(w, http.StatusBadGateway, "provider_refused", "Meta refused: "+me.Message, nil)
	case errors.As(err, &te):
		httpapi.Error(w, http.StatusBadGateway, "provider_refused", "TikTok refused: "+te.Message, nil)
	default:
		h.Logger.Warn("ads setup", zap.String("error", safeErr(err)))
		httpapi.Error(w, http.StatusBadGateway, "provider_unavailable", "the ad platform did not answer; refresh to see whether anything changed", nil)
	}
}
