package adsmanager

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/platform/adaccounts"
	"github.com/bengobox/marketflow-api/internal/platform/postcaption"
)

// BoostPreset is what the Boost dialog starts with, from the same settings a WhatsApp boost uses.
type BoostPreset struct {
	AdAccountConnID string `json:"ad_account_connected_id,omitempty"`
	PageConnID      string `json:"page_connected_id,omitempty"`
	Objective       string `json:"objective"`
	Days            int    `json:"days"`
	// Budgets is the default budget per ad account, in that account's own currency; an account whose
	// rate cannot be read is missing.
	Budgets map[string]int64 `json:"budgets"`
	Message string           `json:"message"`
	// Link is the ad's lead page, PrivacyURL the business's own policy ("" when it has none).
	Link          string `json:"link"`
	PrivacyURL    string `json:"privacy_url"`
	PrivacyPrompt string `json:"privacy_prompt,omitempty"`
}

// privacyPrompt is shown while the business has no privacy policy link of its own.
const privacyPrompt = "Add your own privacy policy link in Settings, Public profile. Meta shows it on lead forms and needs your business's own policy."

// adLink is the ad's lead page link for a paid ad on a platform ("" without one).
func (h *Handler) adLink(ctx context.Context, v *ent.CreativeVersion, source string) string {
	if h.AdLink == nil {
		return ""
	}
	return h.AdLink(ctx, v, source)
}

// boostLink is a boost's link: the ad copy's own, else the ad's lead page.
func (h *Handler) boostLink(ctx context.Context, v *ent.CreativeVersion) string {
	if l := str(v.Copy["link"]); l != "" {
		return l
	}
	return h.adLink(ctx, v, "facebook")
}

// BoostPresetFor handles GET /api/v1/ads/boost-preset?version_id=.
// @Summary What the Boost dialog starts with
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param version_id query string true "Version UUID"
// @Success 200 {object} BoostPreset
// @Security bearerAuth
// @Router /ads/boost-preset [get]
func (h *Handler) BoostPresetFor(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	vid, err := uuid.Parse(r.URL.Query().Get("version_id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "version_id is required", nil)
		return
	}
	v, err := h.DB.CreativeVersion.Query().Where(creativeversion.IDEQ(vid), creativeversion.TenantIDEQ(tid)).Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	d := h.waBoostDefaults(ctx, tid)
	p := BoostPreset{Objective: d.Objective, Days: d.Days, Budgets: map[string]int64{}, Message: postcaption.Words(v.Copy), Link: h.boostLink(ctx, v)}
	accs, err := h.adUsable(ctx, tid, adaccounts.Meta, d.AdAccountConnID)
	if err != nil {
		h.fail(w, err)
		return
	}
	for i, a := range accs {
		if i == 0 {
			p.AdAccountConnID = a.ID.String()
		}
		if n, err := h.budgetIn(ctx, d.BudgetKES, accountCurrency(a)); err == nil {
			p.Budgets[a.ID.String()] = n
		}
	}
	pages, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ("facebook_page"), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if pg := pickPage(pages, d.PageConnID); pg != nil {
		p.PageConnID = pg.ID.String()
	}
	if t, err := h.DB.Tenant.Get(ctx, tid); err == nil && h.PrivacyURL != nil {
		p.PrivacyURL = h.PrivacyURL(t)
	}
	if p.PrivacyURL == "" {
		p.PrivacyPrompt = privacyPrompt
	}
	httpapi.JSON(w, http.StatusOK, p)
}
