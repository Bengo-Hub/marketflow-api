package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// Meta requires two callbacks on every app that reads user data (App settings,
// Basic: "User data deletion" and Facebook Login: "Deauthorize callback URL").
// Both receive a signed_request naming the app-scoped user id of the person who
// connected; we delete what we hold for that person: the Page, Instagram and ad
// account tokens they granted (ConnectedAccount rows) and the tenant's Meta Page
// connection settings. Leads and contacts the tenant collected are the tenant's
// business records, not data Meta gave us about this person, and stay.

const (
	eventMetaDeletion     = "meta.data_deletion"
	eventMetaDeauthorize  = "meta.deauthorize"
	signedRequestMaxBytes = 8 << 10
)

// MetaFBUserKey is where the connecting person's app-scoped Facebook user id is
// kept: a meta_ads/page setting and ConnectedAccount metadata, so the deletion
// callback can find what they granted.
const MetaFBUserKey = "fb_user_id"

var errBadSignedRequest = errors.New("invalid signed_request")

// parseSignedRequest verifies Meta's signed_request (base64url signature and
// payload joined by a dot, HMAC-SHA256 over the encoded payload with the app
// secret) and returns the payload.
func parseSignedRequest(sr, appSecret string) (map[string]any, error) {
	parts := strings.SplitN(sr, ".", 2)
	if len(parts) != 2 || appSecret == "" {
		return nil, errBadSignedRequest
	}
	sig, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[0], "="))
	if err != nil {
		return nil, errBadSignedRequest
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write([]byte(parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errBadSignedRequest
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, errBadSignedRequest
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, errBadSignedRequest
	}
	if alg, _ := payload["algorithm"].(string); !strings.EqualFold(alg, "HMAC-SHA256") {
		return nil, errBadSignedRequest
	}
	return payload, nil
}

// metaSignedUser reads and verifies the signed_request of a callback and returns
// the app-scoped user id. It writes the error response itself.
func (h *Handler) metaSignedUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, signedRequestMaxBytes)
	if err := r.ParseForm(); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "form body expected", nil)
		return "", false
	}
	creds, err := h.loadPlatform(r.Context(), "meta_ads", "oauth")
	secret := ""
	if err == nil {
		secret = creds["app_secret"]
	}
	if secret == "" {
		secret = h.metaAppSecret
	}
	payload, err := parseSignedRequest(r.PostFormValue("signed_request"), secret)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_signed_request", "the signed_request could not be verified", nil)
		return "", false
	}
	uid := fmt.Sprint(payload["user_id"])
	if uid == "" || uid == "<nil>" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_signed_request", "no user_id in signed_request", nil)
		return "", false
	}
	return uid, true
}

// MetaDataDeletion handles POST /api/v1/integrations/meta/data-deletion, Meta's
// data deletion request callback. It deletes the person's tokens and
// connections and answers {url, confirmation_code} as Meta requires; the url is
// the public status page.
// @Summary Meta data deletion request callback
// @Tags Integrations
// @Accept x-www-form-urlencoded
// @Produce json
// @Param signed_request formData string true "Meta signed_request"
// @Success 200 {object} map[string]string
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /integrations/meta/data-deletion [post]
func (h *Handler) MetaDataDeletion(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.metaSignedUser(w, r)
	if !ok {
		return
	}
	counts, err := h.eraseMetaUser(r.Context(), uid)
	if err != nil {
		h.logger.Error("meta data deletion failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "deletion_failed", "the deletion could not be completed, Meta will retry", nil)
		return
	}
	code := confirmationCode()
	h.recordMetaEvent(r.Context(), eventMetaDeletion, uid, map[string]any{"confirmation_code": code, "status": "completed", "deleted": counts})
	statusURL := strings.TrimRight(h.deletionStatusURL, "/") + "?code=" + url.QueryEscape(code)
	httpapi.JSON(w, http.StatusOK, map[string]string{"url": statusURL, "confirmation_code": code})
}

// MetaDeauthorize handles POST /api/v1/integrations/meta/deauthorize, called
// when a person removes the app: their tokens no longer work, so they are deleted.
// @Summary Meta deauthorize callback
// @Tags Integrations
// @Accept x-www-form-urlencoded
// @Param signed_request formData string true "Meta signed_request"
// @Success 200
// @Failure 400 {object} httpapi.ErrorResponse
// @Router /integrations/meta/deauthorize [post]
func (h *Handler) MetaDeauthorize(w http.ResponseWriter, r *http.Request) {
	uid, ok := h.metaSignedUser(w, r)
	if !ok {
		return
	}
	counts, err := h.eraseMetaUser(r.Context(), uid)
	if err != nil {
		h.logger.Error("meta deauthorize cleanup failed", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "deauthorize_failed", "cleanup failed, Meta will retry", nil)
		return
	}
	h.recordMetaEvent(r.Context(), eventMetaDeauthorize, uid, map[string]any{"deleted": counts})
	w.WriteHeader(http.StatusOK)
}

// MetaDeletionStatus handles GET /api/v1/public/data-deletion/{code}: the
// status of a deletion request, for the page Meta links the person to.
// @Summary Status of a data deletion request
// @Tags Integrations
// @Produce json
// @Param code path string true "Confirmation code"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/data-deletion/{code} [get]
func (h *Handler) MetaDeletionStatus(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(chi.URLParam(r, "code"))
	if len(code) < 8 || len(code) > 64 {
		httpapi.Error(w, http.StatusNotFound, "not_found", "no deletion request with this code", nil)
		return
	}
	row, err := h.db.AuditLog.Query().
		Where(auditlog.ScopeEQ("platform"), auditlog.EventEQ(eventMetaDeletion),
			func(s *entsql.Selector) {
				s.Where(sqljson.ValueEQ(auditlog.FieldPayload, code, sqljson.Path("confirmation_code")))
			}).
		Order(ent.Desc(auditlog.FieldOccurredAt)).
		First(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "no deletion request with this code", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"confirmation_code": code,
		"status":            row.Payload["status"],
		"requested_at":      row.OccurredAt.UTC().Format(time.RFC3339),
		"deleted":           row.Payload["deleted"],
	})
}

// eraseMetaUser deletes what the person with this app-scoped id granted: their
// ConnectedAccounts (Page, Instagram and ad account tokens) and each tenant's
// meta_ads/page connection they made, unlinking the tenant's Page.
func (h *Handler) eraseMetaUser(ctx context.Context, fbUserID string) (map[string]int, error) {
	counts := map[string]int{}
	n, err := h.db.ConnectedAccount.Delete().
		Where(connectedaccount.PlatformEQ(PlatformMeta), func(s *entsql.Selector) {
			s.Where(sqljson.ValueEQ(connectedaccount.FieldMetadata, fbUserID, sqljson.Path(MetaFBUserKey)))
		}).
		Exec(ctx)
	if err != nil {
		return nil, err
	}
	counts["connected_accounts"] = n

	marks, err := h.db.ProviderSetting.Query().
		Where(providersetting.ProviderTypeEQ("meta_ads"), providersetting.ProviderNameEQ("page"),
			providersetting.KeyEQ(MetaFBUserKey), providersetting.ValueEQ(fbUserID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, m := range marks {
		n, err := h.db.ProviderSetting.Delete().
			Where(providersetting.TenantIDEQ(m.TenantID), providersetting.ProviderTypeEQ("meta_ads"), providersetting.ProviderNameEQ("page")).
			Exec(ctx)
		if err != nil {
			return nil, err
		}
		counts["page_settings"] += n
		if tid, perr := uuid.Parse(m.TenantID); perr == nil {
			if err := h.db.Tenant.Update().Where(tenant.IDEQ(tid)).ClearMetaPageID().Exec(ctx); err != nil {
				return nil, err
			}
			counts["tenants_unlinked"]++
		}
	}
	return counts, nil
}

// recordMetaEvent keeps a platform audit row; the person is stored only as a
// hash of their app-scoped id, enough to match repeated requests.
func (h *Handler) recordMetaEvent(ctx context.Context, event, fbUserID string, payload map[string]any) {
	sum := sha256.Sum256([]byte(fbUserID))
	payload["fb_user_hash"] = hex.EncodeToString(sum[:])
	if _, err := h.db.AuditLog.Create().
		SetScope("platform").SetEvent(event).SetActor("meta").SetSubjectType("meta_user").
		SetPayload(payload).Save(context.WithoutCancel(ctx)); err != nil {
		h.logger.Warn("meta callback not audited", zap.String("event", event), zap.Error(err))
	}
}

func confirmationCode() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToUpper(hex.EncodeToString(b))
}
