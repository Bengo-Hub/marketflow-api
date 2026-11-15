package publishing

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/google"
)

// A YouTube token renewal refused with invalid_grant fails the post with words the integrations
// card reads as "reconnect"; a renewal that failed because Google was down is retried.
func TestRunnerAccountTokenErrors(t *testing.T) {
	revoked := &google.TokenError{Status: 400, Code: "invalid_grant", Description: "Token has been expired or revoked."}

	ad := &fakeAdapter{platform: YouTube, chunk: 10}
	st := &memStore{}
	r := NewRunner(st, fixedAccounts{err: &AccountTokenError{Platform: YouTube, Err: revoked, Reconnect: googleNeedsReconnect(revoked)}},
		switches{YouTube: true}, ad)
	j := newJob()
	if err := r.Step(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusFailed || !NeedsReconnect(j.Error) || ad.starts != 0 {
		t.Fatalf("a refused grant fails the post and asks for a reconnect: %s %q", j.Status, j.Error)
	}

	down := fmt.Errorf("google token: status 503")
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	r = NewRunner(&memStore{}, fixedAccounts{err: &AccountTokenError{Platform: YouTube, Err: down, Reconnect: googleNeedsReconnect(down)}},
		switches{YouTube: true}, ad)
	r.Now = func() time.Time { return now }
	j = newJob()
	_ = r.Step(context.Background(), j)
	if j.Status != StatusPending || j.Attempts != 1 || !j.ScheduledAt.Equal(now.Add(time.Minute)) || NeedsReconnect(j.Error) {
		t.Fatalf("an outage is retried with backoff: %+v", j)
	}

	if !googleNeedsReconnect(fmt.Errorf("wrap: %w", google.ErrNoRefreshToken)) {
		t.Error("a channel with no refresh token can only be reconnected")
	}
	if googleNeedsReconnect(&google.TokenError{Status: 401, Code: "invalid_client"}) {
		t.Error("a platform client problem is not the business's to fix")
	}
	if !errors.Is(&AccountTokenError{Err: google.ErrNoRefreshToken}, google.ErrNoRefreshToken) {
		t.Error("AccountTokenError unwraps")
	}
}
