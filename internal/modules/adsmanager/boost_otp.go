package adsmanager

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/platform/reqid"
)

// A boost whose total reaches the tenant's threshold (os-settings boost_otp_threshold_kes) needs a
// six-digit code sent on WhatsApp to the verified number of the person launching it (audit M1).
// The code is bound to the boost it was asked for: the video, the total, the days and the ad
// account that pays.
const (
	boostOTPTTL      = 10 * time.Minute
	boostOTPMaxTries = 3
	// boostOTPResend is the shortest gap between two codes for the same person and tenant.
	boostOTPResend = 30 * time.Second
)

var errOTPInvalid = errors.New("the confirmation code is wrong or has expired")

// otpRecord is one pending code. Hash is sha256(code + fingerprint); the code itself is never kept.
type otpRecord struct {
	ID    string
	Hash  string
	Tries int
}

// OTPStore keeps at most one pending boost code per key.
type OTPStore interface {
	Put(ctx context.Context, key string, rec otpRecord, ttl time.Duration) error
	// Get returns nil, nil when there is no code.
	Get(ctx context.Context, key string) (*otpRecord, error)
	// Fail counts a wrong try and returns the tries so far.
	Fail(ctx context.Context, key string) (int, error)
	// Take removes the code and reports whether this call removed it (single use).
	Take(ctx context.Context, key string) (bool, error)
	// Allow reports whether a new code may be sent now, and starts the wait when it may.
	Allow(ctx context.Context, key string, wait time.Duration) (bool, error)
}

// BoostCodes sends and checks boost confirmation codes.
type BoostCodes struct {
	Store OTPStore
	// Send is creative.PlatformWhatsApp.SendOTP.
	Send func(ctx context.Context, to, code, key string) error
	// Phone is the verified WhatsApp number (E.164 digits) of the SSO user in the tenant, or ""
	// when there is none.
	Phone func(ctx context.Context, tenantID, userID uuid.UUID) (string, error)
}

func otpKey(tid, user uuid.UUID) string { return "mf:boost-otp:" + tid.String() + ":" + user.String() }

// boostFingerprint binds a code to one boost: the video, the total, the days and the paying account.
func boostFingerprint(in QuoteInput, assetID string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		strings.ToLower(strings.TrimSpace(assetID)), strconv.FormatInt(in.Budget, 10),
		strconv.Itoa(in.Days), strings.TrimSpace(in.AdAccountConnID),
	}, "|")))
	return hex.EncodeToString(sum[:])
}

func codeHash(code, fingerprint string) string {
	sum := sha256.Sum256([]byte(code + fingerprint))
	return hex.EncodeToString(sum[:])
}

func sixDigits() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// maskPhone shows the country code, the first digit after it and the last three: +2547****901.
func maskPhone(phone string) string {
	d := strings.TrimPrefix(strings.TrimSpace(phone), "+")
	if len(d) < 8 {
		return "+****"
	}
	return "+" + d[:4] + "****" + d[len(d)-3:]
}

// budgetKES is the boost total in KES. Ad budgets have no exchange rate in MarketFlow yet, so
// only a KES account converts; ok is false for every other currency.
func budgetKES(budget int64, currency string) (int64, bool) {
	if strings.EqualFold(strings.TrimSpace(currency), "KES") {
		return budget, true
	}
	return 0, false
}

// otpNeeded: threshold 0 turns the code off; a total that cannot be read in KES always needs it.
func otpNeeded(thresholdKES, budget int64, currency string) bool {
	if thresholdKES <= 0 {
		return false
	}
	kes, ok := budgetKES(budget, currency)
	return !ok || kes >= thresholdKES
}

// issue makes a code for the fingerprint, keeps its hash and sends it to phone.
func (c *BoostCodes) issue(ctx context.Context, tid, user uuid.UUID, fingerprint, phone string) (string, error) {
	code, err := sixDigits()
	if err != nil {
		return "", err
	}
	id := uuid.NewString()
	key := otpKey(tid, user)
	if err := c.Store.Put(ctx, key, otpRecord{ID: id, Hash: codeHash(code, fingerprint)}, boostOTPTTL); err != nil {
		return "", err
	}
	if err := c.Send(ctx, phone, code, "boost-otp-"+id); err != nil {
		_, _ = c.Store.Take(ctx, key)
		return "", err
	}
	return id, nil
}

// verify consumes the code when it matches the fingerprint. Three wrong tries end it.
func (c *BoostCodes) verify(ctx context.Context, tid, user uuid.UUID, fingerprint, otpID, code string) error {
	key := otpKey(tid, user)
	rec, err := c.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	if rec == nil || subtle.ConstantTimeCompare([]byte(rec.ID), []byte(strings.TrimSpace(otpID))) != 1 {
		return errOTPInvalid
	}
	if rec.Tries >= boostOTPMaxTries {
		_, _ = c.Store.Take(ctx, key)
		return errOTPInvalid
	}
	want := codeHash(strings.TrimSpace(code), fingerprint)
	if subtle.ConstantTimeCompare([]byte(want), []byte(rec.Hash)) != 1 {
		if tries, err := c.Store.Fail(ctx, key); err != nil || tries >= boostOTPMaxTries {
			_, _ = c.Store.Take(ctx, key)
		}
		return errOTPInvalid
	}
	took, err := c.Store.Take(ctx, key)
	if err != nil {
		return err
	}
	if !took {
		return errOTPInvalid // used by a request that got there first
	}
	return nil
}

// actorOf is the person a request acts for: the service caller's acting user, else the signed-in user.
func actorOf(ctx context.Context) (uuid.UUID, bool) {
	if id, ok := httpapi.ActingUser(ctx); ok {
		return id, true
	}
	c, ok := authclient.ClaimsFromContext(ctx)
	if !ok || c == nil || c.IsService {
		return uuid.Nil, false
	}
	id, err := c.UserID()
	return id, err == nil && id != uuid.Nil
}

// boostOTPThreshold reads the tenant's threshold; a failed read keeps the default.
func (h *Handler) boostOTPThreshold(ctx context.Context, tid uuid.UUID) int64 {
	t, err := h.DB.Tenant.Get(ctx, tid)
	if err != nil {
		h.Logger.Warn("boost: tenant settings unreadable, default confirmation threshold", zap.String("tenant", tid.String()), zap.Error(err))
		return marketingos.DefaultBoostOTPThresholdKES
	}
	return marketingos.BoostOTPThresholdKES(t.Metadata)
}

// boostConfirmed checks the confirmation code of a boost that needs one. It writes the refusal
// and returns false when the boost may not go ahead.
func (h *Handler) boostConfirmed(w http.ResponseWriter, r *http.Request, tid uuid.UUID, in BoostInput, assetID string, q Quote) bool {
	if h.Codes == nil {
		return true
	}
	return h.checkBoostCode(w, r, tid, h.boostOTPThreshold(r.Context(), tid), in, assetID, q)
}

func (h *Handler) checkBoostCode(w http.ResponseWriter, r *http.Request, tid uuid.UUID, threshold int64, in BoostInput, assetID string, q Quote) bool {
	ctx := r.Context()
	if !otpNeeded(threshold, q.Budget, q.Currency) {
		return true
	}
	if strings.TrimSpace(in.OTPID) == "" || strings.TrimSpace(in.OTPCode) == "" {
		httpapi.JSON(w, http.StatusPreconditionRequired, map[string]any{"error": "otp_required", "code": "otp_required", "threshold_kes": threshold})
		return false
	}
	user, ok := actorOf(ctx)
	if !ok {
		httpapi.Error(w, http.StatusForbidden, "acting_user_required", "acting_user_required", nil)
		return false
	}
	switch err := h.Codes.verify(ctx, tid, user, boostFingerprint(in.QuoteInput, assetID), in.OTPID, in.OTPCode); {
	case errors.Is(err, errOTPInvalid):
		httpapi.Error(w, http.StatusUnprocessableEntity, "otp_invalid", err.Error(), nil)
		return false
	case err != nil:
		h.Logger.Warn("boost: confirmation code unreadable", zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "otp_unavailable", "the confirmation code could not be checked; try again shortly", nil)
		return false
	}
	return true
}

// BoostOTP handles POST /api/v1/ads/boosts/otp: sends a six-digit confirmation code on WhatsApp to
// the verified number of the person launching the boost. The body is the boost about to be sent
// (asset_id, ad_account_connected_id, budget and days are what the code is bound to). The code
// lasts 10 minutes and allows 3 tries.
// @Summary Send the confirmation code for a large boost
// @Tags Ads
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param request body BoostInput true "The boost to confirm"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 403 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 429 {object} httpapi.ErrorResponse
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /ads/boosts/otp [post]
func (h *Handler) BoostOTP(w http.ResponseWriter, r *http.Request) {
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
	if h.Codes == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "otp_unavailable", "confirmation codes are not available", nil)
		return
	}
	var in BoostInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	assetID, err := uuid.Parse(in.AssetID)
	if err != nil || in.Budget <= 0 || in.Days < MinDays || in.Days > MaxDays || strings.TrimSpace(in.AdAccountConnID) == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "asset_id, ad_account_connected_id, budget and days are required", nil)
		return
	}
	user, ok := actorOf(ctx)
	if !ok {
		httpapi.Error(w, http.StatusForbidden, "acting_user_required", "acting_user_required", nil)
		return
	}
	phone, err := h.Codes.Phone(ctx, tid, user)
	if err != nil {
		h.Logger.Warn("boost otp: verified phone unreadable", zap.Error(err))
		httpapi.Error(w, http.StatusServiceUnavailable, "phone_unavailable", "your verified number could not be checked; try again shortly", nil)
		return
	}
	if phone == "" {
		httpapi.JSON(w, http.StatusConflict, map[string]any{"error": "no_verified_phone", "code": "no_verified_phone",
			"message": "verify your WhatsApp number and message the MarketFlow assistant once, then try again"})
		return
	}
	key := otpKey(tid, user)
	if allowed, err := h.Codes.Store.Allow(ctx, key, boostOTPResend); err != nil {
		h.fail(w, err)
		return
	} else if !allowed {
		httpapi.Error(w, http.StatusTooManyRequests, "otp_rate_limited", "a code was just sent; wait a moment before asking again", nil)
		return
	}
	id, err := h.Codes.issue(ctx, tid, user, boostFingerprint(in.QuoteInput, assetID.String()), phone)
	if err != nil {
		h.Logger.Warn("boost otp: code not sent", zap.String("tenant", tid.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "otp_not_sent", "the confirmation code could not be sent; try again shortly", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"otp_id": id, "expires_at": time.Now().UTC().Add(boostOTPTTL).Format(time.RFC3339),
		"channel": "whatsapp", "to_masked": maskPhone(phone),
	})
}

// RedisOTPStore keeps boost codes in Redis.
type RedisOTPStore struct{ RDB *redis.Client }

func (s RedisOTPStore) Put(ctx context.Context, key string, rec otpRecord, ttl time.Duration) error {
	_, err := s.RDB.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.Del(ctx, key)
		p.HSet(ctx, key, "id", rec.ID, "hash", rec.Hash, "tries", rec.Tries)
		p.Expire(ctx, key, ttl)
		return nil
	})
	return err
}

func (s RedisOTPStore) Get(ctx context.Context, key string) (*otpRecord, error) {
	m, err := s.RDB.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if m["id"] == "" {
		return nil, nil
	}
	tries, _ := strconv.Atoi(m["tries"])
	return &otpRecord{ID: m["id"], Hash: m["hash"], Tries: tries}, nil
}

func (s RedisOTPStore) Fail(ctx context.Context, key string) (int, error) {
	n, err := s.RDB.HIncrBy(ctx, key, "tries", 1).Result()
	return int(n), err
}

func (s RedisOTPStore) Take(ctx context.Context, key string) (bool, error) {
	n, err := s.RDB.Del(ctx, key).Result()
	return n == 1, err
}

func (s RedisOTPStore) Allow(ctx context.Context, key string, wait time.Duration) (bool, error) {
	return s.RDB.SetNX(ctx, key+":resend", "1", wait).Result()
}

// AssistantPhone finds the user's verified WhatsApp number: the number they last wrote to the
// MarketFlow assistant from for this tenant (the worker keeps it), checked again with auth-api's
// verified phone lookup so a number the user since removed is never used.
func AssistantPhone(rdb *redis.Client, authURL, authKey string) func(ctx context.Context, tid, user uuid.UUID) (string, error) {
	hc := reqid.Client(10 * time.Second)
	base := strings.TrimSuffix(authURL, "/")
	return func(ctx context.Context, tid, user uuid.UUID) (string, error) {
		if rdb == nil {
			return "", nil
		}
		number, err := rdb.Get(ctx, creative.AssistantNumberKey(tid, user.String())).Result()
		if errors.Is(err, redis.Nil) || strings.TrimSpace(number) == "" {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		number = strings.TrimPrefix(strings.TrimSpace(number), "+")
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/s2s/users/by-phone?phone="+url.QueryEscape("+"+number), nil)
		if err != nil {
			return "", err
		}
		req.Header.Set("X-API-Key", authKey)
		resp, err := hc.Do(req)
		if err != nil {
			return "", fmt.Errorf("auth by-phone: %w", err)
		}
		defer resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
			return "", nil
		case resp.StatusCode != http.StatusOK:
			return "", fmt.Errorf("auth by-phone: status %d", resp.StatusCode)
		}
		var u struct {
			UserID        string `json:"user_id"`
			PhoneVerified bool   `json:"phone_verified"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
			return "", fmt.Errorf("auth by-phone: bad answer")
		}
		if !u.PhoneVerified || !strings.EqualFold(u.UserID, user.String()) {
			return "", nil
		}
		return number, nil
	}
}
