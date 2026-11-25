package adsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/bengobox/marketflow-api/internal/platform/adaccounts"
)

// Which ad account each platform's ads run on (owner, 2026-11-25). Every path here reads
// adPick or adUsable, so Boost, Run ads and WhatsApp use the same account; the choice itself is
// platform/adaccounts.

// ownerRecheck is how long a Meta answer about who owns an ad account is kept when it was not
// conclusive.
const ownerRecheck = 24 * time.Hour

// adPrefs are the tenant's choices.
func (h *Handler) adPrefs(ctx context.Context, tid uuid.UUID) adaccounts.Prefs {
	t, err := h.DB.Tenant.Get(ctx, tid)
	if err != nil {
		return adaccounts.Prefs{}
	}
	return adaccounts.Read(t.Metadata)
}

// adUsable are a platform's active accounts that are not hidden, the one ads run on first.
// legacy is an earlier choice by connected account id ("" for none).
func (h *Handler) adUsable(ctx context.Context, tid uuid.UUID, platform, legacy string) ([]*ent.ConnectedAccount, error) {
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(adaccounts.TypeOf[platform]), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldDisplayName)).Limit(50).All(ctx)
	if err != nil {
		return nil, err
	}
	if platform == adaccounts.Meta && len(rows) > 1 {
		h.learnOwners(ctx, rows)
	}
	return h.adPrefs(ctx, tid).Usable(platform, rows, legacy), nil
}

// adPick is the account a platform's ads run on, nil when there is none.
func (h *Handler) adPick(ctx context.Context, tid uuid.UUID, platform, legacy string) (*ent.ConnectedAccount, error) {
	list, err := h.adUsable(ctx, tid, platform, legacy)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// learnOwners asks Meta once which business portfolio owns each ad account connected before
// MarketFlow stored it, and keeps the answer on the row. An account Meta lists with no portfolio is
// personal only when the login may read portfolios (business_management); otherwise it stays unknown.
func (h *Handler) learnOwners(ctx context.Context, rows []*ent.ConnectedAccount) {
	if h.Meta == nil || h.Keys == nil {
		return
	}
	asked := 0
	for _, a := range rows {
		if _, known := a.Metadata[adaccounts.BusinessID]; known || asked >= 10 {
			continue
		}
		if at, err := time.Parse(time.RFC3339, str(a.Metadata[adaccounts.OwnerChecked])); err == nil && time.Since(at) < ownerRecheck {
			continue
		}
		tok, err := h.token(ctx, a)
		if err != nil || tok == "" {
			continue
		}
		asked++
		patch := map[string]any{adaccounts.OwnerChecked: time.Now().UTC().Format(time.RFC3339)}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		b, err := h.Meta.AdAccountBusiness(cctx, tok, a.ExternalID)
		cancel()
		switch {
		case err != nil:
			h.Logger.Debug("ad account owner not read", zap.String("account", a.ExternalID), zap.Error(err))
		case b.ID != "" || slices.Contains(a.Scopes, "business_management"):
			patch[adaccounts.BusinessID], patch[adaccounts.BusinessName] = b.ID, b.Name
		}
		raw, _ := json.Marshal(patch)
		if err := h.DB.ConnectedAccount.Update().Where(connectedaccount.IDEQ(a.ID)).Modify(func(u *entsql.UpdateBuilder) {
			u.Set(connectedaccount.FieldMetadata, entsql.ExprFunc(func(b *entsql.Builder) {
				b.WriteString("COALESCE(" + connectedaccount.FieldMetadata + ", '{}'::jsonb) || ")
				b.Arg(string(raw))
				b.WriteString("::jsonb")
			}))
		}).Exec(ctx); err != nil {
			h.Logger.Warn("ad account owner not saved", zap.Error(err))
		}
		if a.Metadata == nil {
			a.Metadata = map[string]any{}
		}
		for k, v := range patch {
			a.Metadata[k] = v
		}
	}
}

// AdAccountChoice is one connected ad account on the Ad accounts card.
type AdAccountChoice struct {
	ConnectedID string `json:"connected_account_id"`
	Platform    string `json:"platform"`
	ExternalID  string `json:"external_id"`
	Name        string `json:"name"`
	Currency    string `json:"currency"`
	// Owner is portfolio, personal or unknown (Meta only).
	Owner    string `json:"owner"`
	Business string `json:"business,omitempty"`
	// Chosen is the tenant's own choice; InUse is the account ads run on now.
	Chosen bool `json:"chosen"`
	InUse  bool `json:"in_use"`
	Hidden bool `json:"hidden"`
}

func (h *Handler) adChoices(ctx context.Context, tid uuid.UUID) ([]AdAccountChoice, error) {
	rows, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeIn(adaccounts.Types()...), connectedaccount.StatusEQ("active")).
		Order(ent.Asc(connectedaccount.FieldAccountType), ent.Asc(connectedaccount.FieldDisplayName)).Limit(100).All(ctx)
	if err != nil {
		return nil, err
	}
	by := map[string][]*ent.ConnectedAccount{}
	for _, a := range rows {
		p := adaccounts.PlatformOf(a.AccountType)
		by[p] = append(by[p], a)
	}
	if len(by[adaccounts.Meta]) > 1 {
		h.learnOwners(ctx, by[adaccounts.Meta])
	}
	prefs := h.adPrefs(ctx, tid)
	legacy := h.waBoostDefaults(ctx, tid).AdAccountConnID
	out := []AdAccountChoice{}
	for _, p := range []string{adaccounts.Meta, adaccounts.Google, adaccounts.TikTok} {
		lg := ""
		if p == adaccounts.Meta {
			lg = legacy
		}
		inUse := prefs.Pick(p, by[p], lg)
		for _, a := range by[p] {
			name := a.DisplayName
			if name == "" {
				name = a.ExternalID
			}
			out = append(out, AdAccountChoice{ConnectedID: a.ID.String(), Platform: p, ExternalID: a.ExternalID, Name: name,
				Currency: accountCurrency(a), Owner: adaccounts.Owner(a), Business: str(a.Metadata[adaccounts.BusinessName]),
				Chosen: prefs[p].Default == a.ExternalID, InUse: a == inUse, Hidden: prefs.Hidden(p, a.ExternalID)})
		}
	}
	return out, nil
}

func (h *Handler) canLaunch(ctx context.Context, tid uuid.UUID) bool {
	return h.CanLaunch == nil || h.CanLaunch(ctx, tid)
}

// AdAccounts handles GET /api/v1/ads/ad-accounts: every connected ad account with the tenant's
// choice per platform.
// @Summary Connected ad accounts and which one ads run on
// @Tags Ads
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/ad-accounts [get]
func (h *Handler) AdAccounts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	list, err := h.adChoices(r.Context(), tid)
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": list, "can_edit": h.canLaunch(r.Context(), tid)})
}

// SetAdAccount handles PUT /api/v1/ads/ad-accounts/{id} {default?, hidden?}: makes the account its
// platform's default, or hides or shows it. Hiding does not disconnect it: conversions and
// reports still use it.
// @Summary Choose or hide an ad account
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /ads/ad-accounts/{id} [put]
func (h *Handler) SetAdAccount(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if !h.canLaunch(ctx, tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can choose the ad account", nil)
		return
	}
	var in struct {
		Default *bool `json:"default"`
		Hidden  *bool `json:"hidden"`
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil || json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&in) != nil || (in.Default == nil && in.Hidden == nil) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "an account id and default or hidden are required", nil)
		return
	}
	a, err := h.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeIn(adaccounts.Types()...)).Only(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	prefs, err := h.adPrefs(ctx, tid).Set(adaccounts.PlatformOf(a.AccountType), a.ExternalID, in.Default, in.Hidden)
	if errors.Is(err, adaccounts.ErrHiddenDefault) || errors.Is(err, adaccounts.ErrNotAdAccount) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", err.Error(), nil)
		return
	}
	if err == nil {
		err = metastore.SetKeys(ctx, h.DB, tid, map[string]any{adaccounts.TenantKey: prefs})
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	h.AdAccounts(w, r)
}
