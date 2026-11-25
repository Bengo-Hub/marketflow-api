package adsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/platform/adaccounts"
	"github.com/bengobox/marketflow-api/internal/platform/postcaption"
)

// Boost from WhatsApp (Sprint 17 step 6, owner 2026-11-19). After an ad is approved and posted
// from WhatsApp, the owner taps Boost it, sees a quote and taps Confirm boost. marketflow-ai checks
// the tap (single-use code bound to the run, the user and the number) and calls these internal
// routes as that user. They pick the boost the portal's dialog would start with (the approved
// video, the first active ad account and Page, the video's own text, the tenant's WhatsApp boost
// defaults) and then run the portal's own Quote, BoostOTP and Boost handlers unchanged: the
// platform switch, the launching roles, the agent ceiling, the code above the tenant's threshold,
// the launch credits, and a launch that stays PAUSED until Meta's review and the funding.

// WhatsAppBoostKey in Tenant.metadata holds the tenant's WhatsApp boost defaults:
// {"budget_kes": 3000, "days": 7, "objective": "whatsapp", "ad_account_connected_id": "...",
// "page_connected_id": "..."}. A missing or invalid value falls back to Handler.WABoost
// (platform config) and to the account choice in pickAccount.
const WhatsAppBoostKey = "whatsapp_boost"

// WABoostDefaults is the boost a WhatsApp Boost it tap quotes.
type WABoostDefaults struct {
	// BudgetKES is always in KES; an ad account in another currency gets it converted with
	// treasury's platform rate, so a USD account never reads 3000 as USD 3,000.
	BudgetKES int64  `json:"budget_kes"`
	Days      int    `json:"days"`
	Objective string `json:"objective"` // whatsapp | traffic (leads needs a privacy link, so the portal only)
	// AdAccountConnID and PageConnID are the tenant's chosen accounts (connected account ids).
	AdAccountConnID string `json:"ad_account_connected_id,omitempty"`
	PageConnID      string `json:"page_connected_id,omitempty"`
}

// The fallbacks when the platform config leaves them out: the portal dialog's own starting values.
const (
	DefaultWABoostBudgetKES = int64(3000)
	DefaultWABoostDays      = 7
)

// waBoostDefaults is the tenant's defaults over the platform's.
func (h *Handler) waBoostDefaults(ctx context.Context, tid uuid.UUID) WABoostDefaults {
	d := h.WABoost
	if d.BudgetKES <= 0 {
		d.BudgetKES = DefaultWABoostBudgetKES
	}
	if d.Days < MinDays || d.Days > MaxDays {
		d.Days = DefaultWABoostDays
	}
	if d.Objective != "whatsapp" && d.Objective != "traffic" {
		d.Objective = "whatsapp"
	}
	t, err := h.DB.Tenant.Get(ctx, tid)
	if err != nil {
		return d
	}
	return mergeWABoost(d, t.Metadata)
}

// mergeWABoost lays the tenant's valid whatsapp_boost values over the platform's.
func mergeWABoost(d WABoostDefaults, meta map[string]any) WABoostDefaults {
	m, ok := meta[WhatsAppBoostKey].(map[string]any)
	if !ok {
		return d
	}
	if v, ok := m["budget_kes"].(float64); ok && v > 0 {
		d.BudgetKES = int64(v)
	}
	if v, ok := m["ad_account_connected_id"].(string); ok {
		d.AdAccountConnID = strings.TrimSpace(v)
	}
	if v, ok := m["page_connected_id"].(string); ok {
		d.PageConnID = strings.TrimSpace(v)
	}
	if v, ok := m["days"].(float64); ok && int(v) >= MinDays && int(v) <= MaxDays {
		d.Days = int(v)
	}
	if v, ok := m["objective"].(string); ok && (v == "whatsapp" || v == "traffic") {
		d.Objective = v
	}
	return d
}

// osBoostInput is the body of the internal WhatsApp boost routes.
type osBoostInput struct {
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	VersionID string `json:"version_id"`
	// Budget and Days are what the quote showed; zero means the defaults (quote only).
	Budget  int64  `json:"budget,omitempty"`
	Days    int    `json:"days,omitempty"`
	OTPID   string `json:"otp_id,omitempty"`
	OTPCode string `json:"otp_code,omitempty"`
}

// waBoostPlan is the boost a WhatsApp tap starts, with the names for the message.
type waBoostPlan struct {
	Input       BoostInput
	AccountName string
	PageName    string
	Objective   string
}

var (
	errNoAdAccount = errors.New("no_ad_account")
	errNoPage      = errors.New("no_page")
	// errNoRate is an ad account in another currency when treasury's rate cannot be read: the
	// default KES budget cannot be sized, so the boost is made in the Studio instead.
	errNoRate = errors.New("no_rate")
)

// accountCurrency is the currency stored on a connected ad account ("" when unknown).
func accountCurrency(a *ent.ConnectedAccount) string {
	s, _ := a.Metadata["currency"].(string)
	return strings.ToUpper(strings.TrimSpace(s))
}

// pickPage chooses among active Pages (sorted by name): the tenant's chosen one, else the first.
func pickPage(accounts []*ent.ConnectedAccount, preferred string) *ent.ConnectedAccount {
	if len(accounts) == 0 {
		return nil
	}
	for _, a := range accounts {
		if preferred != "" && a.ID.String() == preferred {
			return a
		}
	}
	return accounts[0]
}

// budgetIn converts the KES default into the ad account's currency (whole units, rounded up so a
// small budget never becomes 0).
func (h *Handler) budgetIn(ctx context.Context, kes int64, currency string) (int64, error) {
	if currency == "" || currency == "KES" {
		return kes, nil
	}
	if h.KESPer == nil {
		return 0, errNoRate
	}
	rate, err := h.KESPer(ctx, currency)
	if err != nil || rate <= 0 {
		return 0, errNoRate
	}
	n := int64(float64(kes) / rate)
	if float64(n)*rate < float64(kes) {
		n++
	}
	return n, nil
}

// waPlan picks the boost for an approved version: the tenant's chosen ad account and Page, else a
// KES ad account and the first Page by name. budget is in the ad account's currency (what the
// quote showed); zero means the tenant's default, converted from KES.
func (h *Handler) waPlan(ctx context.Context, tid, vid uuid.UUID, budget int64, days int) (*waBoostPlan, error) {
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		return nil, err
	}
	if err := brandimages.CheckUse(ctx, h.DB, tid, vid); err != nil {
		return nil, err
	}
	asset, err := h.DB.CreativeAsset.Query().
		Where(creativeasset.VersionIDEQ(vid), creativeasset.TenantIDEQ(tid), creativeasset.KindEQ("video"), creativeasset.PurgedAtIsNil()).
		First(ctx)
	if err != nil {
		return nil, err
	}
	active := func(accountType string) ([]*ent.ConnectedAccount, error) {
		return h.DB.ConnectedAccount.Query().
			Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(accountType), connectedaccount.StatusEQ("active")).
			Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	}
	d := h.waBoostDefaults(ctx, tid)
	acc, err := h.adPick(ctx, tid, adaccounts.Meta, d.AdAccountConnID)
	if err != nil {
		return nil, err
	}
	if acc == nil {
		return nil, errNoAdAccount
	}
	pages, err := active("facebook_page")
	if err != nil {
		return nil, err
	}
	page := pickPage(pages, d.PageConnID)
	if page == nil {
		return nil, errNoPage
	}
	if budget <= 0 {
		if budget, err = h.budgetIn(ctx, d.BudgetKES, accountCurrency(acc)); err != nil {
			return nil, err
		}
	}
	if days <= 0 {
		days = d.Days
	}
	// A video ad keeps its words on its end card; without any the business name stands in, as Meta
	// needs a text.
	msg := postcaption.Words(v.Copy)
	if msg == "" {
		if t, err := h.DB.Tenant.Get(ctx, tid); err == nil {
			msg = t.Name
		}
	}
	in := BoostInput{QuoteInput: QuoteInput{AdAccountConnID: acc.ID.String(), Budget: budget, Days: days},
		PageConnID: page.ID.String(), AssetID: asset.ID.String(), Objective: d.Objective, Message: msg}
	if d.Objective == "traffic" {
		in.Link = h.boostLink(ctx, v)
	}
	return &waBoostPlan{Input: in, AccountName: acc.DisplayName, PageName: page.DisplayName, Objective: d.Objective}, nil
}

// readOSBoost reads the body and checks the user may launch ads, writing the refusal.
func (h *Handler) readOSBoost(w http.ResponseWriter, r *http.Request) (osBoostInput, uuid.UUID, uuid.UUID, uuid.UUID, context.Context, bool) {
	var in osBoostInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return in, uuid.Nil, uuid.Nil, uuid.Nil, nil, false
	}
	tid, err1 := uuid.Parse(in.TenantID)
	uid, err2 := uuid.Parse(in.UserID)
	vid, err3 := uuid.Parse(in.VersionID)
	if err1 != nil || err2 != nil || err3 != nil || uid == uuid.Nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id, user_id and version_id are required", nil)
		return in, uuid.Nil, uuid.Nil, uuid.Nil, nil, false
	}
	// The service key acts for the person who tapped: their own role in the tenant decides.
	ctx := httpapi.WithActingUser(r.Context(), uid)
	if h.Enabled != nil && !h.Enabled(ctx) {
		httpapi.Error(w, http.StatusConflict, "ads_disabled", "paid ads are not switched on yet", nil)
		return in, uuid.Nil, uuid.Nil, uuid.Nil, nil, false
	}
	if h.CanLaunch != nil && !h.CanLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can launch ads", nil)
		return in, uuid.Nil, uuid.Nil, uuid.Nil, nil, false
	}
	return in, tid, uid, vid, ctx, true
}

func (h *Handler) waPlanFailed(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, brandimages.ErrFaceUse):
		httpapi.Error(w, http.StatusConflict, "consent_ended", err.Error(), nil)
	case errors.Is(err, errNoAdAccount):
		httpapi.Error(w, http.StatusConflict, "no_ad_account", "connect a Meta ad account in MarketFlow first", nil)
	case errors.Is(err, errNoPage):
		httpapi.Error(w, http.StatusConflict, "no_page", "connect the Facebook Page the ad runs as first", nil)
	case errors.Is(err, errNoRate):
		httpapi.Error(w, http.StatusConflict, "no_rate", "the exchange rate for this ad account's currency could not be read, so set the budget in the Studio", nil)
	case ent.IsNotFound(err):
		httpapi.Error(w, http.StatusConflict, "no_video", "the video is not available any more", nil)
	default:
		h.fail(w, err)
	}
}

// OSTurnBoostQuote handles S2S POST /api/v1/internal/ads/os-turn/boost-quote: the quote for the
// default WhatsApp boost of an approved video. 200 {quote, budget, days, objective, ad_account,
// page}; 409 ads_disabled, no_ad_account, no_page or no_video; 403 when the user may not launch.
func (h *Handler) OSTurnBoostQuote(w http.ResponseWriter, r *http.Request) {
	in, tid, _, vid, ctx, ok := h.readOSBoost(w, r)
	if !ok {
		return
	}
	p, err := h.waPlan(ctx, tid, vid, in.Budget, in.Days)
	if err != nil {
		h.waPlanFailed(w, err)
		return
	}
	q, _, _, err := h.quote(ctx, tid, p.Input.QuoteInput)
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "invalid_account", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"quote": q, "budget": q.Budget, "days": q.Days, "objective": p.Objective,
		"ad_account": p.AccountName, "page": p.PageName, "ready": len(q.Blockers) == 0})
}

// OSTurnBoost handles S2S POST /api/v1/internal/ads/os-turn/boost: launches the quoted boost
// through the portal's Boost handler (201 the boost; 428 otp_required; the same refusals).
func (h *Handler) OSTurnBoost(w http.ResponseWriter, r *http.Request) {
	h.osDelegate(w, r, h.Boost)
}

// OSTurnBoostOTP handles S2S POST /api/v1/internal/ads/os-turn/boost-otp: sends the confirmation
// code for the quoted boost through the portal's BoostOTP handler.
func (h *Handler) OSTurnBoostOTP(w http.ResponseWriter, r *http.Request) {
	h.osDelegate(w, r, h.BoostOTP)
}

// osDelegate rebuilds the request as the portal would send it (X-Tenant-ID, the BoostInput body,
// the acting user) and hands it to next.
func (h *Handler) osDelegate(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	in, tid, _, vid, ctx, ok := h.readOSBoost(w, r)
	if !ok {
		return
	}
	if in.Budget <= 0 || in.Days <= 0 {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "budget and days from the quote are required", nil)
		return
	}
	p, err := h.waPlan(ctx, tid, vid, in.Budget, in.Days)
	if err != nil {
		h.waPlanFailed(w, err)
		return
	}
	p.Input.OTPID, p.Input.OTPCode = strings.TrimSpace(in.OTPID), strings.TrimSpace(in.OTPCode)
	body, err := json.Marshal(p.Input)
	if err != nil {
		h.fail(w, err)
		return
	}
	r2 := r.Clone(ctx)
	r2.Body = io.NopCloser(bytes.NewReader(body))
	r2.ContentLength = int64(len(body))
	r2.Header.Set("X-Tenant-ID", tid.String())
	r2.Header.Set("Content-Type", "application/json")
	next(w, r2)
}
