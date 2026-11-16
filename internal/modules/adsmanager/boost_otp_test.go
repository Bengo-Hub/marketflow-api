package adsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// memOTPStore is an in-memory OTPStore for tests.
type memOTPStore struct {
	mu      sync.Mutex
	recs    map[string]*otpRecord
	waiting map[string]bool
}

func newMemOTPStore() *memOTPStore {
	return &memOTPStore{recs: map[string]*otpRecord{}, waiting: map[string]bool{}}
}

func (m *memOTPStore) Put(_ context.Context, key string, rec otpRecord, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs[key] = &rec
	return nil
}
func (m *memOTPStore) Get(_ context.Context, key string) (*otpRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.recs[key]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}
func (m *memOTPStore) Fail(_ context.Context, key string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.recs[key]
	if !ok {
		return 0, errors.New("gone")
	}
	r.Tries++
	return r.Tries, nil
}
func (m *memOTPStore) Take(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.recs[key]
	delete(m.recs, key)
	return ok, nil
}
func (m *memOTPStore) Allow(_ context.Context, key string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.waiting[key] {
		return false, nil
	}
	m.waiting[key] = true
	return true, nil
}

func TestOTPNeeded(t *testing.T) {
	cases := []struct {
		name      string
		threshold int64
		budget    int64
		currency  string
		want      bool
	}{
		{"off", 0, 1_000_000, "KES", false},
		{"below", 10000, 9999, "KES", false},
		{"at the threshold", 10000, 10000, "KES", true},
		{"above", 10000, 50000, "kes", true},
		{"another currency always asks", 10000, 5, "USD", true},
		{"another currency, code off", 0, 5000, "USD", false},
	}
	for _, c := range cases {
		if got := otpNeeded(c.threshold, c.budget, c.currency); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestMaskPhone(t *testing.T) {
	cases := map[string]string{
		"254712345901":  "+2547****901",
		"+254712345901": "+2547****901",
		"123":           "+****",
	}
	for in, want := range cases {
		if got := maskPhone(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestBoostFingerprint(t *testing.T) {
	base := QuoteInput{AdAccountConnID: "acc", Budget: 20000, Days: 7}
	fp := boostFingerprint(base, "ASSET")
	if fp != boostFingerprint(base, "asset") {
		t.Fatal("asset id case must not matter")
	}
	changes := []struct {
		name  string
		in    QuoteInput
		asset string
	}{
		{"budget", QuoteInput{AdAccountConnID: "acc", Budget: 20001, Days: 7}, "asset"},
		{"days", QuoteInput{AdAccountConnID: "acc", Budget: 20000, Days: 8}, "asset"},
		{"account", QuoteInput{AdAccountConnID: "other", Budget: 20000, Days: 7}, "asset"},
		{"video", base, "another"},
	}
	for _, c := range changes {
		if boostFingerprint(c.in, c.asset) == fp {
			t.Errorf("%s change kept the fingerprint", c.name)
		}
	}
}

// codesUnderTest sends into sent and resolves every user to phone.
func codesUnderTest(phone string) (*BoostCodes, *[]string) {
	var sent []string
	return &BoostCodes{
		Store: newMemOTPStore(),
		Send: func(_ context.Context, to, code, _ string) error {
			sent = append(sent, to+":"+code)
			return nil
		},
		Phone: func(context.Context, uuid.UUID, uuid.UUID) (string, error) { return phone, nil },
	}, &sent
}

func lastCode(t *testing.T, sent []string) string {
	t.Helper()
	m := regexp.MustCompile(`:(\d{6})$`).FindStringSubmatch(sent[len(sent)-1])
	if m == nil {
		t.Fatalf("no six-digit code in %q", sent[len(sent)-1])
	}
	return m[1]
}

func TestBoostCodesVerify(t *testing.T) {
	ctx := context.Background()
	tid, user := uuid.New(), uuid.New()
	fp := boostFingerprint(QuoteInput{AdAccountConnID: "a", Budget: 20000, Days: 3}, "v")
	other := boostFingerprint(QuoteInput{AdAccountConnID: "a", Budget: 90000, Days: 3}, "v")

	cases := []struct {
		name  string
		run   func(c *BoostCodes, id, code string) []error
		wants []bool // true: that call succeeds
	}{
		{"the right code once", func(c *BoostCodes, id, code string) []error {
			return []error{c.verify(ctx, tid, user, fp, id, code)}
		}, []bool{true}},
		{"single use", func(c *BoostCodes, id, code string) []error {
			return []error{c.verify(ctx, tid, user, fp, id, code), c.verify(ctx, tid, user, fp, id, code)}
		}, []bool{true, false}},
		{"another boost", func(c *BoostCodes, id, code string) []error {
			return []error{c.verify(ctx, tid, user, other, id, code)}
		}, []bool{false}},
		{"another otp id", func(c *BoostCodes, _ string, code string) []error {
			return []error{c.verify(ctx, tid, user, fp, uuid.NewString(), code)}
		}, []bool{false}},
		{"another user", func(c *BoostCodes, id, code string) []error {
			return []error{c.verify(ctx, tid, uuid.New(), fp, id, code)}
		}, []bool{false}},
		{"three wrong tries end it", func(c *BoostCodes, id, code string) []error {
			return []error{
				c.verify(ctx, tid, user, fp, id, "000000x"),
				c.verify(ctx, tid, user, fp, id, "000000y"),
				c.verify(ctx, tid, user, fp, id, "000000z"),
				c.verify(ctx, tid, user, fp, id, code),
			}
		}, []bool{false, false, false, false}},
		{"two wrong tries then right", func(c *BoostCodes, id, code string) []error {
			return []error{
				c.verify(ctx, tid, user, fp, id, "000000x"),
				c.verify(ctx, tid, user, fp, id, "000000y"),
				c.verify(ctx, tid, user, fp, id, code),
			}
		}, []bool{false, false, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codes, sent := codesUnderTest("254712345901")
			id, err := codes.issue(ctx, tid, user, fp, "254712345901")
			if err != nil {
				t.Fatal(err)
			}
			errs := c.run(codes, id, lastCode(t, *sent))
			for i, err := range errs {
				if (err == nil) != c.wants[i] {
					t.Fatalf("call %d: err %v", i, err)
				}
			}
		})
	}
}

func TestBoostCodesSendFailureLeavesNoCode(t *testing.T) {
	codes, _ := codesUnderTest("254712345901")
	codes.Send = func(context.Context, string, string, string) error { return errors.New("down") }
	tid, user := uuid.New(), uuid.New()
	if _, err := codes.issue(context.Background(), tid, user, "fp", "254712345901"); err == nil {
		t.Fatal("send failure must fail the issue")
	}
	if rec, _ := codes.Store.Get(context.Background(), otpKey(tid, user)); rec != nil {
		t.Fatal("a code that was not sent must not stay usable")
	}
}

func signedIn(r *http.Request, tid, user uuid.UUID) *http.Request {
	c := &authclient.Claims{TenantID: tid.String()}
	c.Subject = user.String()
	r.Header.Set("X-Tenant-ID", tid.String())
	return r.WithContext(authclient.ContextWithClaims(r.Context(), c))
}

func TestBoostOTPHandler(t *testing.T) {
	tid, user := uuid.New(), uuid.New()
	body := func(v map[string]any) *bytes.Reader {
		b, _ := json.Marshal(v)
		return bytes.NewReader(b)
	}
	good := map[string]any{"asset_id": uuid.NewString(), "ad_account_connected_id": uuid.NewString(), "budget": 20000, "days": 5}
	cases := []struct {
		name  string
		phone string
		in    map[string]any
		want  int
		code  string
	}{
		{"sends a code", "254712345901", good, 200, ""},
		{"no verified phone", "", good, 409, "no_verified_phone"},
		{"missing budget", "254712345901", map[string]any{"asset_id": uuid.NewString(), "ad_account_connected_id": "a", "days": 5}, 400, "invalid_request"},
		{"bad days", "254712345901", map[string]any{"asset_id": uuid.NewString(), "ad_account_connected_id": "a", "budget": 1, "days": 0}, 400, "invalid_request"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			codes, sent := codesUnderTest(c.phone)
			h := &Handler{Codes: codes, Logger: zap.NewNop()}
			r := signedIn(httptest.NewRequest(http.MethodPost, "/api/v1/ads/boosts/otp", body(c.in)), tid, user)
			w := httptest.NewRecorder()
			h.BoostOTP(w, r)
			if w.Code != c.want {
				t.Fatalf("status %d (%s)", w.Code, w.Body.String())
			}
			var out map[string]any
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			if c.code != "" && out["code"] != c.code && out["error"] != c.code {
				t.Fatalf("body %v", out)
			}
			if c.want == 200 {
				if out["channel"] != "whatsapp" || out["to_masked"] != "+2547****901" || out["otp_id"] == "" || out["expires_at"] == "" {
					t.Fatalf("body %v", out)
				}
				if len(*sent) != 1 {
					t.Fatalf("sent %v", *sent)
				}
			}
		})
	}

	// A second request straight away waits.
	codes, _ := codesUnderTest("254712345901")
	h := &Handler{Codes: codes, Logger: zap.NewNop()}
	for i, want := range []int{200, 429} {
		w := httptest.NewRecorder()
		h.BoostOTP(w, signedIn(httptest.NewRequest(http.MethodPost, "/api/v1/ads/boosts/otp", body(good)), tid, user))
		if w.Code != want {
			t.Fatalf("request %d: %d", i, w.Code)
		}
	}
}

func TestCheckBoostCode(t *testing.T) {
	tid, user := uuid.New(), uuid.New()
	asset := uuid.NewString()
	in := BoostInput{QuoteInput: QuoteInput{AdAccountConnID: "acc", Budget: 20000, Days: 5}}
	big := Quote{Currency: "KES", Budget: 20000}
	small := Quote{Currency: "KES", Budget: 500}

	codes, sent := codesUnderTest("254712345901")
	h := &Handler{Codes: codes, Logger: zap.NewNop()}
	id, err := codes.issue(context.Background(), tid, user, boostFingerprint(in.QuoteInput, asset), "254712345901")
	if err != nil {
		t.Fatal(err)
	}
	code := lastCode(t, *sent)

	cases := []struct {
		name      string
		threshold int64
		q         Quote
		otpID     string
		otpCode   string
		ok        bool
		status    int
	}{
		{"below the threshold", 10000, small, "", "", true, 200},
		{"switched off", 0, big, "", "", true, 200},
		{"needs a code", 10000, big, "", "", false, 428},
		{"wrong code", 10000, big, id, "999999", false, 422},
		{"right code", 10000, big, id, code, true, 200},
		{"code already used", 10000, big, id, code, false, 422},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := in
			body.OTPID, body.OTPCode = c.otpID, c.otpCode
			w := httptest.NewRecorder()
			r := signedIn(httptest.NewRequest(http.MethodPost, "/api/v1/ads/boosts", nil), tid, user)
			got := h.checkBoostCode(w, r, tid, c.threshold, body, asset, c.q)
			if got != c.ok {
				t.Fatalf("ok %v", got)
			}
			if !got && w.Code != c.status {
				t.Fatalf("status %d (%s)", w.Code, w.Body.String())
			}
			if c.status == 428 {
				var out map[string]any
				_ = json.Unmarshal(w.Body.Bytes(), &out)
				if out["error"] != "otp_required" || out["threshold_kes"] != float64(10000) {
					t.Fatalf("body %v", out)
				}
			}
		})
	}
}
