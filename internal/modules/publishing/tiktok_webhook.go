package publishing

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
)

// tiktokEvent is a TikTok for Developers webhook. content is itself a JSON string.
type tiktokEvent struct {
	ClientKey  string `json:"client_key"`
	Event      string `json:"event"`
	CreateTime int64  `json:"create_time"`
	UserOpenID string `json:"user_openid"`
	Content    string `json:"content"`
}

type tiktokPublishContent struct {
	PublishID   string `json:"publish_id"`
	PublishType string `json:"publish_type"`
	PostID      string `json:"post_id"`
	Reason      string `json:"reason"`
}

// TikTokWebhook handles POST /api/v1/webhooks/tiktok/events, the webhook of the TikTok developer
// app (Content Posting and Login Kit). Every event must carry a valid TikTok-Signature, keyed
// with the app's client secret from the platform settings, and name this app's client key.
//
//   - post.publish.*: the job with that publish_id is due at once, so the worker reads its status
//     now (status/fetch stays the source of truth, and the worker's own poll is the fallback).
//   - authorization.removed: the person removed MarketFlow in TikTok; the account is revoked and
//     its tokens dropped.
//
// Anything else is acknowledged and ignored.
// @Summary TikTok developer app webhook (post status, deauthorization)
// @Tags Webhooks
// @Accept json
// @Produce json
// @Success 200 {object} map[string]bool
// @Failure 401 {object} httpapi.ErrorResponse
// @Router /webhooks/tiktok/events [post]
func (h *Handler) TikTokWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			httpapi.Error(w, http.StatusRequestEntityTooLarge, "too_large", "body too large", nil)
			return
		}
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "unreadable body", nil)
		return
	}
	ctx := r.Context()
	app, err := tiktok.LoadApp(ctx, h.Store.DB, h.Store.Keys)
	if err != nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "not_configured", "TikTok app not configured", nil)
		return
	}
	now := time.Now()
	if h.now != nil {
		now = h.now()
	}
	if !tiktok.VerifySignature(r.Header.Get(tiktok.SignatureHeader), body, app.Secret, now) {
		httpapi.Error(w, http.StatusUnauthorized, "invalid_signature", "invalid signature", nil)
		return
	}
	var ev tiktokEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	if ev.ClientKey != "" && ev.ClientKey != app.Key {
		httpapi.JSON(w, http.StatusOK, map[string]bool{"ok": true}) // another app's event
		return
	}
	switch {
	case strings.HasPrefix(ev.Event, "post.publish."):
		var c tiktokPublishContent
		if json.Unmarshal([]byte(ev.Content), &c) == nil && c.PublishID != "" {
			n, err := h.Store.DB.PublishJob.Update().
				Where(
					publishjob.PlatformEQ(TikTok),
					publishjob.ExternalIDEQ(c.PublishID),
					publishjob.StatusIn(StatusUploading, StatusProcessing),
				).
				SetScheduledAt(now.UTC()).
				Save(ctx)
			if err != nil {
				h.Logger.Warn("tiktok webhook: job not marked due", zap.String("event", ev.Event), zap.Error(err))
				httpapi.Error(w, http.StatusInternalServerError, "server_error", "try again", nil)
				return
			}
			if n > 0 && h.Kick != nil {
				h.Kick()
			}
		}
	case ev.Event == "authorization.removed" && ev.UserOpenID != "":
		if _, err := h.Store.DB.ConnectedAccount.Update().
			Where(
				connectedaccount.AccountTypeEQ(tiktok.AccountCreator),
				connectedaccount.ExternalIDEQ(ev.UserOpenID),
				connectedaccount.StatusNEQ("revoked"),
			).
			SetStatus("revoked").ClearTokenCiphertext().ClearRefreshCiphertext().
			Save(ctx); err != nil {
			h.Logger.Warn("tiktok webhook: account not revoked", zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "server_error", "try again", nil)
			return
		}
	}
	httpapi.JSON(w, http.StatusOK, map[string]bool{"ok": true})
}
