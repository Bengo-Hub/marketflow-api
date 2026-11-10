package adsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves /api/v1/ads.
type Handler struct {
	DB   *ent.Client
	Keys *encryption.KeyProvider
	Meta *meta.Client
	// HasFeature reports a plan feature (paid_ads_launch means no launch charge).
	HasFeature func(ctx context.Context, tenantID, feature string) bool
	// Charge spends launch credits through marketflow-ai; nil means unmetered (development).
	Charge func(ctx context.Context, tenantID uuid.UUID, units int64, key, desc string) (bool, error)
	// CanLaunch gates launches (the approving roles); nil allows every member.
	CanLaunch     func(ctx context.Context, tenantID uuid.UUID) bool
	LaunchCredits int
	// Enabled is the platform switch ProviderSetting ads/meta_enabled (Meta App Review).
	Enabled func(ctx context.Context) bool
	Logger  *zap.Logger
}

// DefaultLaunchCredits is the fixed charge for a client without a plan (platform config).
const DefaultLaunchCredits = 10

// AccountView is one ad account with its health.
type AccountView struct {
	ConnectedID string  `json:"connected_account_id"`
	ID          string  `json:"ad_account_id"`
	Name        string  `json:"name"`
	Currency    string  `json:"currency"`
	Country     string  `json:"country,omitempty"`
	Active      bool    `json:"active"`
	Problem     string  `json:"problem,omitempty"`
	Funding     Funding `json:"funding,omitempty"`
}

// Accounts handles GET /api/v1/ads/accounts: the tenant's connected Meta ad accounts with live
// health (at most 10 checked, in name order).
// @Summary Connected ad accounts with health
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/accounts [get]
func (h *Handler) Accounts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("meta_ad_account"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(10).All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, a := range rows {
		v := AccountView{ConnectedID: a.ID.String(), ID: a.ExternalID, Name: a.DisplayName}
		tok, err := h.token(r.Context(), a)
		if err != nil {
			v.Problem = "reconnect this ad account"
			out = append(out, v)
			continue
		}
		health, err := h.Meta.AdAccountHealth(r.Context(), tok, a.ExternalID)
		if err != nil {
			v.Problem = "Meta did not answer; try again"
			out = append(out, v)
			continue
		}
		q := BuildQuote(health, 1, 1, 0)
		v.Currency, v.Country, v.Active, v.Funding = health.Currency, health.Country, health.AccountStatus == meta.AccountActive, q.Funding
		if !v.Active {
			v.Problem = accountProblem(health)
		}
		out = append(out, v)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": out})
}

// CreateKESAccount handles POST /api/v1/ads/accounts/kes {business_id?, name?}: a KES ad account
// (Nairobi time) in the client's own business portfolio, for M-Pesa on Meta. It uses the user
// token of an ad account the client already connected (a USD one, say); a client with no ad
// account connects Meta again after creating one in Meta Business Suite.
// @Summary Create a KES ad account in the client's business portfolio
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 201 {object} AccountView
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /ads/accounts/kes [post]
func (h *Handler) CreateKESAccount(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.CanLaunch != nil && !h.CanLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can create an ad account", nil)
		return
	}
	var in struct {
		BusinessID string `json:"business_id"`
		Name       string `json:"name"`
		PageConnID string `json:"page_connected_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	src, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("meta_ad_account"), connectedaccount.StatusEQ("active")).
		First(ctx)
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "no_meta_user", "create an ad account in Meta Business Suite first, then connect Meta again", nil)
		return
	}
	tok, err := h.token(ctx, src)
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "not_connected", "reconnect Meta to continue", nil)
		return
	}
	businessID := in.BusinessID
	if businessID == "" {
		bs, err := h.Meta.Businesses(ctx, tok)
		if err != nil || len(bs) == 0 {
			httpapi.Error(w, http.StatusConflict, "no_business", "Meta shows no business portfolio for this login; create one in Meta Business Suite", nil)
			return
		}
		if len(bs) > 1 {
			httpapi.JSON(w, http.StatusConflict, map[string]any{"error": "choose the business portfolio for the new ad account", "code": "choose_business", "businesses": bs})
			return
		}
		businessID = bs[0].ID
	}
	pageID := ""
	if in.PageConnID != "" {
		if p, err := h.account(ctx, tid, in.PageConnID, "facebook_page"); err == nil {
			pageID = p.ExternalID
		}
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "MarketFlow KES"
	}
	actID, err := h.Meta.CreateKESAdAccount(ctx, tok, businessID, name, pageID)
	if err != nil {
		httpapi.Error(w, http.StatusBadGateway, "meta_refused", "Meta did not create the ad account: "+err.Error(), nil)
		return
	}
	row, err := h.DB.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("meta_ad_account").
		SetExternalID(actID).SetDisplayName(name).SetTokenCiphertext(src.TokenCiphertext).SetScopes(src.Scopes).
		SetMetadata(map[string]any{"currency": "KES", "business_id": businessID, "created_by_marketflow": true}).Save(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, AccountView{ConnectedID: row.ID.String(), ID: actID, Name: name, Currency: "KES", Active: true})
}

// QuoteInput is the body of POST /ads/quote.
type QuoteInput struct {
	AdAccountConnID string `json:"ad_account_connected_id"`
	Budget          int64  `json:"budget"`
	Days            int    `json:"days"`
}

// Quote handles POST /api/v1/ads/quote (gate G7): the ad budget the client pays Meta, any
// shortfall, and MarketFlow's launch credits.
// @Summary Quote a boost
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} Quote
// @Security bearerAuth
// @Router /ads/quote [post]
func (h *Handler) Quote(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	var in QuoteInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	q, _, _, err := h.quote(r.Context(), tid, in)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_account", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, q)
}

func (h *Handler) quote(ctx context.Context, tid uuid.UUID, in QuoteInput) (Quote, *ent.ConnectedAccount, string, error) {
	acc, err := h.account(ctx, tid, in.AdAccountConnID, "meta_ad_account")
	if err != nil {
		return Quote{}, nil, "", fmt.Errorf("choose a connected ad account")
	}
	tok, err := h.token(ctx, acc)
	if err != nil {
		return Quote{}, nil, "", fmt.Errorf("reconnect this ad account")
	}
	health, err := h.Meta.AdAccountHealth(ctx, tok, acc.ExternalID)
	if err != nil {
		return Quote{}, nil, "", fmt.Errorf("Meta did not answer about this ad account; try again")
	}
	credits := h.LaunchCredits
	if h.HasFeature != nil && h.HasFeature(ctx, tid.String(), "paid_ads_launch") {
		credits = 0
	}
	return BuildQuote(health, in.Budget, in.Days, credits), acc, tok, nil
}

// BoostInput is the body of POST /ads/boosts: the confirmed quote plus the creative.
type BoostInput struct {
	QuoteInput
	PageConnID string `json:"page_connected_id"`
	AssetID    string `json:"asset_id"`
	// ExtraAssetIDs are up to two more approved videos run as more ads in the same ad set (away
	// decision 23): the budget is shared, so the optimiser can pause the weaker ones.
	ExtraAssetIDs  []string `json:"extra_asset_ids,omitempty"`
	Objective      string   `json:"objective"` // whatsapp | leads | traffic
	Countries      []string `json:"countries"`
	AgeMin         int      `json:"age_min"`
	AgeMax         int      `json:"age_max"`
	Message        string   `json:"message"`
	Headline       string   `json:"headline"`
	Link           string   `json:"link"`
	WhatsAppNumber string   `json:"whatsapp_number"`
	Welcome        string   `json:"welcome"`
	// PrivacyURL is the business's privacy policy, required by Meta on a leads instant form.
	PrivacyURL string    `json:"privacy_url"`
	StartAt    time.Time `json:"start_at"`
}

var objectives = map[string]string{"whatsapp": "OUTCOME_ENGAGEMENT", "leads": "OUTCOME_LEADS", "traffic": "OUTCOME_TRAFFIC"}

// Boost handles POST /api/v1/ads/boosts: checks everything, charges the launch credits once,
// and queues the launch; the worker creates the PAUSED Meta objects and goes live after review
// and funding.
// @Summary Boost an approved video
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 201 {object} map[string]interface{}
// @Failure 402 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /ads/boosts [post]
func (h *Handler) Boost(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if h.Enabled != nil && !h.Enabled(ctx) {
		httpapi.Error(w, http.StatusConflict, "ads_disabled", "paid ads are not switched on yet", nil)
		return
	}
	if h.CanLaunch != nil && !h.CanLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can launch ads", nil)
		return
	}
	var in BoostInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	objective, okObj := objectives[in.Objective]
	switch {
	case !okObj:
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "objective must be whatsapp, leads or traffic", nil)
		return
	case strings.TrimSpace(in.Message) == "" || utf8.RuneCountInString(in.Message) > 2000:
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "the ad text is required (at most 2,000 characters)", nil)
		return
	case objective == "OUTCOME_LEADS" && !strings.HasPrefix(strings.TrimSpace(in.PrivacyURL), "https://"):
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "a leads ad needs your privacy policy link (https), which Meta shows on the form", nil)
		return
	case objective == "OUTCOME_TRAFFIC" && !strings.HasPrefix(in.Link, "https://"):
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "a link ad needs an https link", nil)
		return
	}
	if len(in.Countries) == 0 {
		in.Countries = []string{"KE"}
	}
	q, acc, _, err := h.quote(ctx, tid, in.QuoteInput)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_account", err.Error(), nil)
		return
	}
	if len(q.Blockers) > 0 {
		httpapi.Error(w, http.StatusConflict, "not_ready", strings.Join(q.Blockers, "; "), map[string]any{"quote": q})
		return
	}
	page, err := h.account(ctx, tid, in.PageConnID, "facebook_page")
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_page", "choose the Facebook Page the ad runs as", nil)
		return
	}
	assetID, err := uuid.Parse(in.AssetID)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "asset_id is required", nil)
		return
	}
	asset, err := h.DB.CreativeAsset.Query().
		Where(creativeasset.IDEQ(assetID), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if ok, _ := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(asset.VersionID), creativeversion.StatusEQ("approved")).Exist(ctx); !ok {
		httpapi.Error(w, http.StatusConflict, "not_approved", "approve the video before boosting it", nil)
		return
	}
	if len(in.ExtraAssetIDs) > maxExtraCreatives {
		httpapi.Error(w, http.StatusBadRequest, "too_many_creatives", "a boost runs at most three videos", nil)
		return
	}
	extras := make([]string, 0, len(in.ExtraAssetIDs))
	for _, raw := range in.ExtraAssetIDs {
		xid, err := uuid.Parse(raw)
		if err != nil || xid == asset.ID {
			httpapi.Error(w, http.StatusBadRequest, "invalid_request", "each extra video must be a different approved video", nil)
			return
		}
		x, err := h.DB.CreativeAsset.Query().
			Where(creativeasset.IDEQ(xid), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).Only(ctx)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_request", "an extra video was not found", nil)
			return
		}
		if ok, _ := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(x.VersionID), creativeversion.StatusEQ("approved")).Exist(ctx); !ok {
			httpapi.Error(w, http.StatusConflict, "not_approved", "approve every video before boosting it", nil)
			return
		}
		extras = append(extras, x.ID.String())
	}

	start := in.StartAt.UTC()
	if start.Before(time.Now().UTC()) {
		start = time.Now().UTC().Add(10 * time.Minute)
	}
	id := uuid.New()
	l := stored{Plan: storedPlan{AdAccountConnID: acc.ID.String(), PageConnID: page.ID.String(), Currency: q.Currency,
		Budget: q.Budget, Start: start, End: start.AddDate(0, 0, q.Days), Objective: objective, WhatsAppNumber: in.WhatsAppNumber,
		Countries: in.Countries, AgeMin: in.AgeMin, AgeMax: in.AgeMax, AssetID: asset.ID.String(), ExtraAssetIDs: extras, Message: strings.TrimSpace(in.Message),
		Headline: in.Headline, Link: in.Link, Welcome: in.Welcome, PrivacyURL: strings.TrimSpace(in.PrivacyURL), LaunchCharged: q.LaunchCredits > 0},
		State: State{Stage: StageCampaign}}
	name := "Boost " + time.Now().UTC().Format("2 Jan 2006")
	if in.Headline != "" {
		name = in.Headline
	}
	steps := boostSteps{
		create: func() error {
			return h.DB.Campaign.Create().SetID(id).SetTenantID(tid).SetName(name).SetCampaignType("lead_gen").SetPlatform("meta").
				SetStatus(CampaignPending).SetBudgetTotal(float64(q.Budget)).SetCurrency(q.Currency).
				SetStartDate(l.Plan.Start).SetEndDate(l.Plan.End).
				SetConnectedAccountID(acc.ID).SetCreativeVersionID(asset.VersionID).
				SetMetadata(map[string]any{"launch": l, "quote": q}).Exec(ctx)
		},
		activate: func() error {
			return h.DB.Campaign.UpdateOneID(id).SetStatus(CampaignLaunching).Exec(ctx)
		},
		discard: func() {
			if err := h.DB.Campaign.DeleteOneID(id).Exec(ctx); err != nil {
				h.Logger.Warn("unpaid boost draft not removed", zap.String("campaign", id.String()), zap.Error(err))
			}
		},
	}
	if q.LaunchCredits > 0 && h.Charge != nil {
		steps.charge = func() (bool, error) {
			return h.Charge(ctx, tid, int64(q.LaunchCredits), "ads-launch:"+id.String(), "Paid ad launch")
		}
	}
	switch err := commitBoost(ctx, steps); {
	case errors.Is(err, errNoCredits):
		httpapi.Error(w, http.StatusPaymentRequired, "no_credits", fmt.Sprintf("a launch needs %d AI credits; buy a bundle on your billing page", q.LaunchCredits), nil)
		return
	case errors.Is(err, errCreditsUnavailable):
		httpapi.Error(w, http.StatusServiceUnavailable, "credits_unavailable", "AI credits could not be checked right now, try again shortly", nil)
		return
	case err != nil:
		h.Logger.Error("boost not committed", zap.String("campaign", id.String()), zap.Error(err))
		h.fail(w, err)
		return
	}
	row, err := h.DB.Campaign.Get(ctx, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusCreated, view(row))
}

// ConfirmPaid handles POST /api/v1/ads/boosts/{id}/paid: the client says they topped up on Meta
// (needed when Meta does not show the prepaid balance).
// @Summary Confirm the ad budget was paid on Meta
// @Tags Ads
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/boosts/{id}/paid [post]
func (h *Handler) ConfirmPaid(w http.ResponseWriter, r *http.Request) {
	_, c, l, ok := h.load(w, r)
	if !ok {
		return
	}
	if c.Status != CampaignLaunching {
		httpapi.Error(w, http.StatusConflict, "conflict", "this boost is not waiting for funds", nil)
		return
	}
	l.State.PaidOK = true
	l.State.NextAt = time.Now().UTC()
	if err := SaveLaunch(r.Context(), h.DB, c.ID, l, CampaignLaunching); err != nil {
		h.fail(w, err)
		return
	}
	h.get(w, r, c.ID)
}

// Pause handles POST /api/v1/ads/boosts/{id}/pause: a live boost is paused on Meta; one still
// launching stops before going live.
// @Summary Pause a boost
// @Tags Ads
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/boosts/{id}/pause [post]
func (h *Handler) Pause(w http.ResponseWriter, r *http.Request) {
	tid, c, l, ok := h.load(w, r)
	if !ok {
		return
	}
	if h.CanLaunch != nil && !h.CanLaunch(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can pause ads", nil)
		return
	}
	if c.Status == CampaignActive && l.State.CampaignID != "" {
		acc, err := h.account(r.Context(), tid, l.Plan.AdAccountConnID, "meta_ad_account")
		if err != nil {
			httpapi.Error(w, http.StatusConflict, "not_connected", "reconnect the ad account to pause it", nil)
			return
		}
		tok, err := h.token(r.Context(), acc)
		if err == nil {
			err = h.Meta.SetStatus(r.Context(), tok, l.State.CampaignID, "PAUSED")
		}
		if err != nil {
			httpapi.Error(w, http.StatusBadGateway, "pause_failed", "Meta did not pause the campaign: "+err.Error(), nil)
			return
		}
	}
	if _, err := h.DB.Campaign.Update().Where(campaign.IDEQ(c.ID), campaign.StatusIn(CampaignLaunching, CampaignActive)).
		SetStatus(CampaignPaused).Save(r.Context()); err != nil {
		h.fail(w, err)
		return
	}
	h.get(w, r, c.ID)
}

// errNotResumable explains why a boost cannot be resumed.
type errNotResumable string

func (e errNotResumable) Error() string { return string(e) }

// resumable reports whether a boost can be switched back on at Meta: it must be paused and have
// gone live (a Meta campaign id). One paused before going live has nothing on Meta to resume.
func resumable(status, metaCampaignID string) error {
	switch {
	case status != CampaignPaused:
		return errNotResumable("only a paused boost can be resumed")
	case metaCampaignID == "":
		return errNotResumable("this boost was paused before it went live; start a new boost instead")
	}
	return nil
}

// Resume handles POST /api/v1/ads/boosts/{id}/resume: a paused boost that went live is switched
// back on at Meta, then marked active so the insights job and auto-pause watch it again.
// @Summary Resume a paused boost
// @Tags Ads
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /ads/boosts/{id}/resume [post]
func (h *Handler) Resume(w http.ResponseWriter, r *http.Request) {
	tid, c, l, ok := h.load(w, r)
	if !ok {
		return
	}
	if h.CanLaunch != nil && !h.CanLaunch(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can resume ads", nil)
		return
	}
	if err := resumable(c.Status, l.State.CampaignID); err != nil {
		httpapi.Error(w, http.StatusConflict, "not_resumable", err.Error(), nil)
		return
	}
	acc, err := h.account(r.Context(), tid, l.Plan.AdAccountConnID, "meta_ad_account")
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "not_connected", "reconnect the ad account to resume it", nil)
		return
	}
	tok, err := h.token(r.Context(), acc)
	if err == nil {
		err = h.Meta.SetStatus(r.Context(), tok, l.State.CampaignID, "ACTIVE")
	}
	if err != nil {
		h.Logger.Warn("meta resume failed", zap.String("campaign", c.ID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "resume_failed", "Meta did not resume the campaign; try again", nil)
		return
	}
	if _, err := h.DB.Campaign.Update().Where(campaign.IDEQ(c.ID), campaign.StatusEQ(CampaignPaused)).
		SetStatus(CampaignActive).Save(r.Context()); err != nil {
		h.fail(w, err)
		return
	}
	h.get(w, r, c.ID)
}

// Get handles GET /api/v1/ads/boosts/{id}.
// @Summary A boost with its launch progress
// @Tags Ads
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Campaign UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/boosts/{id} [get]
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	_, c, _, ok := h.load(w, r)
	if !ok {
		return
	}
	httpapi.JSON(w, http.StatusOK, view(c))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	c, err := h.DB.Campaign.Get(r.Context(), id)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view(c))
}

func view(c *ent.Campaign) map[string]any {
	out := map[string]any{"id": c.ID.String(), "name": c.Name, "status": c.Status, "budget": c.BudgetTotal, "currency": c.Currency,
		"start_date": c.StartDate, "end_date": c.EndDate}
	if l, ok := c.Metadata["launch"].(map[string]any); ok {
		out["launch"] = l["state"]
	}
	if q, ok := c.Metadata["quote"]; ok {
		out["quote"] = q
	}
	return out
}

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (uuid.UUID, *ent.Campaign, stored, bool) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return uuid.Nil, nil, stored{}, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return uuid.Nil, nil, stored{}, false
	}
	c, err := h.DB.Campaign.Query().Where(campaign.IDEQ(id), campaign.TenantIDEQ(tid), campaign.PlatformEQ("meta")).Only(r.Context())
	if err != nil {
		h.fail(w, err)
		return uuid.Nil, nil, stored{}, false
	}
	var l stored
	if b, err := json.Marshal(c.Metadata["launch"]); err == nil {
		_ = json.NewDecoder(bytes.NewReader(b)).Decode(&l)
	}
	return tid, c, l, true
}

func (h *Handler) account(ctx context.Context, tid uuid.UUID, connID, accountType string) (*ent.ConnectedAccount, error) {
	id, err := uuid.Parse(connID)
	if err != nil {
		return nil, err
	}
	return h.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(accountType), connectedaccount.StatusEQ("active")).
		Only(ctx)
}

func (h *Handler) token(ctx context.Context, a *ent.ConnectedAccount) (string, error) {
	if h.Keys == nil || a.TokenCiphertext == "" {
		return "", fmt.Errorf("no token")
	}
	return h.Keys.Decrypt(ctx, a.TokenCiphertext)
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if ent.IsNotFound(err) {
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	h.Logger.Error("ads request failed", zap.Error(err))
	httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
}

// maxExtraCreatives is how many extra videos a boost may run beside its main one.
const maxExtraCreatives = 2

// ResumeAd handles POST /api/v1/ads/boosts/{id}/ads/{ad_id}/resume: the owner switches back on an
// ad the budget optimiser paused (the undo in its email). Only an ad of this boost may be resumed;
// the optimiser never judges it again.
func (h *Handler) ResumeAd(w http.ResponseWriter, r *http.Request) {
	tid, c, l, ok := h.load(w, r)
	if !ok {
		return
	}
	if h.CanLaunch != nil && !h.CanLaunch(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can resume ads", nil)
		return
	}
	adID := chi.URLParam(r, "ad_id")
	if !boostOwnsAd(l.State, adID) {
		httpapi.Error(w, http.StatusNotFound, "not_found", "this ad is not part of the boost", nil)
		return
	}
	acc, err := h.account(r.Context(), tid, l.Plan.AdAccountConnID, "meta_ad_account")
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "not_connected", "reconnect the ad account to resume it", nil)
		return
	}
	tok, err := h.token(r.Context(), acc)
	if err == nil {
		err = h.Meta.SetStatus(r.Context(), tok, adID, "ACTIVE")
	}
	if err != nil {
		h.Logger.Warn("meta ad resume failed", zap.String("campaign", c.ID.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "resume_failed", "Meta did not switch the ad back on; try again", nil)
		return
	}
	h.get(w, r, c.ID)
}

// boostOwnsAd reports whether adID is the boost's main ad or one of its extras.
func boostOwnsAd(st State, adID string) bool {
	if adID == "" {
		return false
	}
	if st.AdID == adID {
		return true
	}
	for _, x := range st.Extras {
		if x.AdID == adID {
			return true
		}
	}
	return false
}
