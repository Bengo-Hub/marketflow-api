package integrations

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// OAuth connect state (OWASP review M1, 2026-11-18). The signed state (signState) proves who
// started a connect flow and when, but on its own it could be used again within its 10 minutes,
// and a consent URL sent to someone else would connect their account to the sender's tenant. So:
//
//   - Single use: each state is stored in Redis for 10 minutes and taken (GETDEL) at the
//     callback; a second callback with it is refused.
//   - Bound to the browser: the start sets an HttpOnly, Secure, SameSite=Lax cookie with a random
//     nonce on the API host, and the callback needs the same nonce. The start is the signed-in
//     person's own call, so only their browser holds the cookie; a forwarded consent URL arrives
//     without it. The portal must make that call with credentials (withCredentials) so the
//     browser keeps the cookie; MF_SECURITY_OAUTH_BROWSER_BINDING=false turns the cookie check off
//     while it does not yet.
//   - The stored record also carries the user who started the flow, which must match the signed
//     state's user.

const (
	// stateTTL is how long a connect flow may take, the same as the signed state's own window.
	stateTTL = 10 * time.Minute
	// stateCookiePrefix names each flow's nonce cookie, with a short hash of its state, so two
	// flows started in two tabs do not overwrite each other. __Host- keeps it on the API host.
	stateCookiePrefix = "__Host-mf_oauth_"
)

var (
	errStateUsed        = errors.New("connect link was already used; start the connection again")
	errStateBrowser     = errors.New("connect link was started in another browser; start the connection again from this one")
	errStateUnavailable = errors.New("connect could not be checked just now; start the connection again")
)

// stateStore keeps each issued state until its callback takes it.
type stateStore interface {
	// Put stores value under key for ttl.
	Put(ctx context.Context, key, value string, ttl time.Duration) error
	// Take returns the value and deletes it; ok is false when there is none.
	Take(ctx context.Context, key string) (value string, ok bool, err error)
}

// redisStateStore is the stateStore in the shared Redis, under "<namespace>:oauth-state:".
type redisStateStore struct {
	rdb    redis.UniversalClient
	prefix string
}

func (s redisStateStore) Put(ctx context.Context, key, value string, ttl time.Duration) error {
	return s.rdb.Set(ctx, s.prefix+key, value, ttl).Err()
}

func (s redisStateStore) Take(ctx context.Context, key string) (string, bool, error) {
	v, err := s.rdb.GetDel(ctx, s.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// WithStateStore makes connect states single use (Redis) and, when bindBrowser is set, bound to
// the browser that started the flow. Without it (tests) the signed state works as before.
func (h *Handler) WithStateStore(rdb redis.UniversalClient, namespace string, bindBrowser bool) *Handler {
	if rdb != nil {
		if namespace == "" {
			namespace = "marketflow"
		}
		h.states = redisStateStore{rdb: rdb, prefix: namespace + ":oauth-state:"}
	}
	h.bindBrowser = bindBrowser
	return h
}

// stateRecord is what is stored for an issued state.
type stateRecord struct {
	UserID    string `json:"user_id"`
	NonceHash string `json:"nonce_hash,omitempty"`
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// stateCookieName is the nonce cookie of one state.
func stateCookieName(state string) string {
	return stateCookiePrefix + sha256Hex(state)[:16]
}

// issueState signs a state for the caller's tenant and user, stores it for single use and sets
// the browser's nonce cookie. A state that could not be stored is an error: the flow would only
// fail at the callback.
func (h *Handler) issueState(w http.ResponseWriter, r *http.Request, tenantID string) (string, error) {
	userID := requestUserID(r)
	state := h.signState(tenantID, userID)
	if h.states == nil {
		return state, nil
	}
	rec := stateRecord{UserID: userID}
	var nonce string
	if h.bindBrowser {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		nonce = hex.EncodeToString(b)
		rec.NonceHash = sha256Hex(nonce)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return "", err
	}
	if err := h.states.Put(r.Context(), sha256Hex(state), string(raw), stateTTL); err != nil {
		return "", err
	}
	if h.bindBrowser {
		http.SetCookie(w, &http.Cookie{Name: stateCookieName(state), Value: nonce, Path: "/",
			MaxAge: int(stateTTL.Seconds()), HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	}
	return state, nil
}

// redeemState checks a callback's state: the signature and age (checkState), then takes the
// stored record (single use), then the browser's nonce. It returns the tenant even on most
// failures, so the browser goes back to the right integrations page. The nonce cookie is cleared
// either way.
func (h *Handler) redeemState(w http.ResponseWriter, r *http.Request, state string) (string, string, error) {
	tenantID, userID, err := h.checkState(state)
	if err != nil || h.states == nil {
		return tenantID, userID, err
	}
	name := stateCookieName(state)
	defer http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})

	raw, ok, err := h.states.Take(r.Context(), sha256Hex(state))
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("connect state store unavailable", zap.Error(err))
		}
		return tenantID, userID, errStateUnavailable
	}
	if !ok {
		return tenantID, userID, errStateUsed
	}
	var rec stateRecord
	if json.Unmarshal([]byte(raw), &rec) != nil || rec.UserID != userID {
		return tenantID, userID, errStateInvalid
	}
	if h.bindBrowser {
		c, err := r.Cookie(name)
		if err != nil || c.Value == "" || rec.NonceHash == "" ||
			subtle.ConstantTimeCompare([]byte(sha256Hex(c.Value)), []byte(rec.NonceHash)) != 1 {
			return tenantID, userID, errStateBrowser
		}
	}
	return tenantID, userID, nil
}

func writeStateUnavailable(w http.ResponseWriter) {
	httpapi.Error(w, http.StatusServiceUnavailable, "state_unavailable", "could not start the connection just now; try again", nil)
}

// issueStateOrFail issues a state for an authorize handler, answering 503 when it cannot be stored.
func (h *Handler) issueStateOrFail(w http.ResponseWriter, r *http.Request, tenantID string) (string, bool) {
	state, err := h.issueState(w, r, tenantID)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("connect state not stored", zap.String("tenant", tenantID), zap.Error(err))
		}
		writeStateUnavailable(w)
		return "", false
	}
	return state, true
}
