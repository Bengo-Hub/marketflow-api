package integrations

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// memStates is an in-memory stateStore; failTake makes Take fail like an unreachable Redis.
type memStates struct {
	mu       sync.Mutex
	m        map[string]string
	failTake bool
}

func (s *memStates) Put(_ context.Context, key, value string, _ time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]string{}
	}
	s.m[key] = value
	return nil
}

func (s *memStates) Take(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failTake {
		return "", false, errors.New("redis down")
	}
	v, ok := s.m[key]
	delete(s.m, key)
	return v, ok, nil
}

func stateHandler(bind bool) (*Handler, *memStates) {
	h := NewHandler(nil, []byte("test-state-key-0123456789abcdef"), nil, zap.NewNop()) // secret-scan:allow
	st := &memStates{}
	h.states, h.bindBrowser = st, bind
	return h, st
}

// start runs issueState as the signed-in user and returns the state and the cookies set.
func start(t *testing.T, h *Handler, tenantID, userID string) (string, []*http.Cookie) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/integrations/google/authorize", nil)
	req = req.WithContext(authclient.ContextWithClaims(req.Context(), &authclient.Claims{TenantID: tenantID}))
	if userID != "" {
		c, _ := authclient.ClaimsFromContext(req.Context())
		c.Subject = userID
	}
	rec := httptest.NewRecorder()
	state, err := h.issueState(rec, req, tenantID)
	if err != nil {
		t.Fatal(err)
	}
	return state, rec.Result().Cookies()
}

// callback runs redeemState with the given cookies.
func callback(h *Handler, state string, cookies []*http.Cookie) (string, string, error, *httptest.ResponseRecorder) {
	req := httptest.NewRequest(http.MethodGet, "/integrations/google/callback?state="+url.QueryEscape(state), nil)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	tid, uid, err := h.redeemState(rec, req, state)
	return tid, uid, err, rec
}

func TestOAuthStateSingleUseAndBrowserBound(t *testing.T) {
	h, _ := stateHandler(true)
	tenantID, userID := uuid.NewString(), uuid.NewString()
	state, cookies := start(t, h, tenantID, userID)

	if len(cookies) != 1 {
		t.Fatalf("one nonce cookie expected, got %d", len(cookies))
	}
	c := cookies[0]
	if !strings.HasPrefix(c.Name, stateCookiePrefix) || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode ||
		c.Path != "/" || c.Domain != "" || c.MaxAge != int(stateTTL.Seconds()) {
		t.Fatalf("cookie attributes: %+v", c)
	}

	// Another browser (no cookie) is refused, and that attempt uses the state up.
	if _, _, err, _ := callback(h, state, nil); !errors.Is(err, errStateBrowser) {
		t.Fatalf("no cookie: %v", err)
	}
	if _, _, err, _ := callback(h, state, cookies); !errors.Is(err, errStateUsed) {
		t.Fatalf("a refused state must not work afterwards: %v", err)
	}

	// The starting browser, first time: the tenant and user come back and the cookie is cleared.
	state, cookies = start(t, h, tenantID, userID)
	tid, uid, err, rec := callback(h, state, cookies)
	if err != nil || tid != tenantID || uid != userID {
		t.Fatalf("first callback: %v %s %s", err, tid, uid)
	}
	cleared := false
	for _, k := range rec.Result().Cookies() {
		if k.Name == cookies[0].Name && k.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Error("the nonce cookie was not cleared")
	}
	// The same state again (a replayed callback URL) is refused.
	if _, _, err, _ := callback(h, state, cookies); !errors.Is(err, errStateUsed) {
		t.Fatalf("replay: %v", err)
	}

	// A wrong nonce (another flow's cookie under this flow's name) is refused.
	state, cookies = start(t, h, tenantID, userID)
	forged := &http.Cookie{Name: cookies[0].Name, Value: strings.Repeat("0", 64)}
	if _, _, err, _ := callback(h, state, []*http.Cookie{forged}); !errors.Is(err, errStateBrowser) {
		t.Fatalf("wrong nonce: %v", err)
	}
}

func TestOAuthStateFailures(t *testing.T) {
	tenantID, userID := uuid.NewString(), uuid.NewString()

	// The store unreachable at the callback: refused, never let through.
	h, st := stateHandler(true)
	state, cookies := start(t, h, tenantID, userID)
	st.failTake = true
	if _, _, err, _ := callback(h, state, cookies); !errors.Is(err, errStateUnavailable) {
		t.Fatalf("store down: %v", err)
	}

	// A signed state that was never stored (issued before this change, or forged with the key) is
	// refused as used.
	h, _ = stateHandler(true)
	if _, _, err, _ := callback(h, h.signState(tenantID, userID), nil); !errors.Is(err, errStateUsed) {
		t.Fatalf("unstored state: %v", err)
	}

	// A bad signature is still refused before the store is touched.
	if _, _, err, _ := callback(h, "x.y.z.w", nil); !errors.Is(err, errStateInvalid) {
		t.Fatalf("bad signature: %v", err)
	}

	// Browser binding off (the portal not yet sending credentials): single use still holds.
	h, _ = stateHandler(false)
	state, cookies = start(t, h, tenantID, userID)
	if len(cookies) != 0 {
		t.Fatalf("no cookie expected with binding off: %v", cookies)
	}
	if _, _, err, _ := callback(h, state, nil); err != nil {
		t.Fatalf("binding off, first use: %v", err)
	}
	if _, _, err, _ := callback(h, state, nil); !errors.Is(err, errStateUsed) {
		t.Fatalf("binding off, replay: %v", err)
	}

	// No store wired (tests elsewhere): the signed state alone, as before.
	h = NewHandler(nil, []byte("test-state-key-0123456789abcdef"), nil, zap.NewNop()) // secret-scan:allow
	state = h.signState(tenantID, userID)
	if tid, _, err, _ := callback(h, state, nil); err != nil || tid != tenantID {
		t.Fatalf("no store: %v", err)
	}
}

func TestStateReasons(t *testing.T) {
	cases := map[error]string{
		errStateExpired:     "state_expired",
		errStateUsed:        "state_used",
		errStateBrowser:     "state_browser",
		errStateUnavailable: "state_unavailable",
		errStateInvalid:     "bad_state",
	}
	for err, want := range cases {
		if got := stateReason(err); got != want {
			t.Errorf("%v: %s, want %s", err, got, want)
		}
	}
}
