package integrations

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Sprint 16: a connected card shows when an account needs reconnecting, from the token health
// job's expiry and from the errors failed posts recorded.
func TestAccountHealth(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	connected := now.Add(-48 * time.Hour)
	meta := map[string]any{metaConnectedAt: connected.Format(time.RFC3339)}

	cases := []struct {
		name       string
		acc        ent.ConnectedAccount
		failure    time.Time
		want       string
		wantExpiry bool
	}{
		{"no expiry, no errors", ent.ConnectedAccount{Status: "active", Metadata: meta}, time.Time{}, healthOK, false},
		{"expires in 3 days", ent.ConnectedAccount{Status: "active", ExpiresAt: at(72 * time.Hour), Metadata: meta}, time.Time{}, healthExpiring, true},
		{"expires in 20 days", ent.ConnectedAccount{Status: "active", ExpiresAt: at(20 * 24 * time.Hour), Metadata: meta}, time.Time{}, healthOK, false},
		{"expiry passed", ent.ConnectedAccount{Status: "active", ExpiresAt: at(-time.Hour), Metadata: meta}, time.Time{}, healthExpired, false},
		{"a recheck date is not an expiry", ent.ConnectedAccount{Status: "active", ExpiresAt: at(time.Hour),
			Metadata: map[string]any{metaExpiryBasis: expiryBasisRecheck}}, time.Time{}, healthOK, false},
		{"marked expired by the job", ent.ConnectedAccount{Status: "expired", Metadata: meta}, time.Time{}, healthExpired, false},
		{"post failed on the token after connecting", ent.ConnectedAccount{Status: "active", Metadata: meta}, now.Add(-time.Hour), healthExpired, false},
		{"post failed before the reconnect", ent.ConnectedAccount{Status: "active", Metadata: meta}, connected.Add(-time.Hour), healthOK, false},
		{"no connect stamp falls back to created_at", ent.ConnectedAccount{Status: "active", CreatedAt: now.Add(-24 * time.Hour)}, now.Add(-time.Hour), healthExpired, false},
	}
	for _, c := range cases {
		got, exp, problem := accountHealth(&c.acc, c.failure, now)
		if got != c.want {
			t.Errorf("%s: health %q, want %q", c.name, got, c.want)
		}
		if (exp != nil) != c.wantExpiry {
			t.Errorf("%s: expires_at %v, want present=%t", c.name, exp, c.wantExpiry)
		}
		if (got != healthOK) != (problem != "") {
			t.Errorf("%s: problem %q must be set exactly when the account needs attention", c.name, problem)
		}
	}
}

func TestAuthFailuresFrom(t *testing.T) {
	now := time.Now().UTC()
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	tokenErr := "facebook start: 400 OAuthException: Error validating access token: Session has expired"
	rows := []*ent.PublishJob{ // newest first, as the query orders them
		{ConnectedAccountID: a, Status: "published", UpdatedAt: now},
		{ConnectedAccountID: a, Status: "failed", Error: tokenErr, UpdatedAt: now.Add(-time.Hour)},
		{ConnectedAccountID: b, Status: "pending", Error: "facebook upload: status 503", UpdatedAt: now},
		{ConnectedAccountID: b, Status: "failed", Error: tokenErr, UpdatedAt: now.Add(-2 * time.Hour)},
		{ConnectedAccountID: c, Status: "failed", Error: "instagram could not process the video (error)", UpdatedAt: now},
	}
	got := authFailuresFrom(rows)
	if _, ok := got[a]; ok {
		t.Error("a later published post clears an older token error")
	}
	if ts, ok := got[b]; !ok || !ts.Equal(now.Add(-2*time.Hour)) {
		t.Errorf("a newer network error does not hide an unresolved token error: %v", got[b])
	}
	if _, ok := got[c]; ok {
		t.Error("a processing failure is not a reconnect")
	}
}
