package publishing

import (
	"context"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
)

// TikTokCreatorInfoFunc reads creator_info for an account (TikTokDirect.CreatorInfo in production).
type TikTokCreatorInfoFunc func(ctx context.Context, a Account) (CreatorInfo, error)

// TikTokCreatorInfo handles GET /api/v1/publishing/accounts/{id}/tiktok-creator-info. TikTok's
// posting guidelines ask the publish screen to show who will post, offer only the privacy levels
// the creator may use, and grey out interactions the creator turned off, so the dialog reads
// this every time a TikTok account is picked.
// @Summary TikTok creator info for the publish dialog
// @Tags Publishing
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Connected account id (a TikTok account)"
// @Success 200 {object} CreatorInfo
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/accounts/{id}/tiktok-creator-info [get]
func (h *Handler) TikTokCreatorInfo(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_account", "invalid account id", nil)
		return
	}
	ctx := r.Context()
	isTikTok, err := h.Store.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(aid), connectedaccount.TenantIDEQ(tid),
			connectedaccount.AccountTypeEQ(tiktok.AccountCreator), connectedaccount.StatusEQ("active")).
		Exist(ctx)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !isTikTok {
		httpapi.Error(w, http.StatusBadRequest, "invalid_account", "not a connected TikTok account", nil)
		return
	}
	var src Accounts = h.Store
	if h.Tokens != nil {
		src = h.Tokens
	}
	acc, err := src.Account(ctx, tid.String(), aid.String())
	if err != nil {
		var te *AccountTokenError
		if errors.As(err, &te) {
			httpapi.Error(w, http.StatusConflict, "reconnect_required", te.Error(), nil)
			return
		}
		h.fail(w, err)
		return
	}
	read := h.TikTokInfo
	if read == nil {
		read = NewTikTokDirect(tiktok.OpenAPIBase, "", false).CreatorInfo
	}
	info, err := read(ctx, acc)
	if err != nil {
		h.Logger.Warn("tiktok creator info failed", zap.String("account", aid.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "tiktok_unavailable",
			"TikTok did not answer just now. Try again in a minute.", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, info)
}
