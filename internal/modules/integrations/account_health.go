package integrations

import (
	"context"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
)

// Account health on the integrations page (Sprint 16). Two sources, both already recorded:
//   - expires_at and status, written by the daily token health job (worker/connected_token_health.go),
//     which also emails the admins before and after expiry;
//   - the error a failed or retrying post recorded on its PublishJob, when the platform said the
//     token no longer works (publishing.NeedsReconnect).
const (
	healthOK       = "ok"
	healthExpiring = "expiring"
	healthExpired  = "expired"

	// healthRenewWindow matches the token health job's renew window, so the card and the
	// email agree on when an account starts needing attention.
	healthRenewWindow = 7 * 24 * time.Hour
	// authFailureLookback bounds how far back a publishing error can flag an account.
	authFailureLookback = 14 * 24 * time.Hour
	authFailureScanCap  = 200

	// metaConnectedAt is set on every connect and reconnect (saveAccounts); an error recorded
	// before it is about the old token and no longer counts.
	metaConnectedAt = "connected_at"
	// metaExpiryBasis says what expires_at holds: "token", "data_access" or "recheck" (a date
	// the job checks again because Meta reported no expiry). Only a recheck date is not shown.
	metaExpiryBasis    = "expiry_basis"
	expiryBasisRecheck = "recheck"
)

// accountHealth says whether an account works, needs reconnecting soon or has stopped working,
// with the renew-by date when it is expiring and a plain reason the card can show.
// lastAuthFailure is the newest publishing error for this account that needs a reconnect (zero
// when none).
func accountHealth(a *ent.ConnectedAccount, lastAuthFailure, now time.Time) (string, *time.Time, string) {
	switch a.Status {
	case "expired", "error":
		return healthExpired, nil, "This connection has stopped working. Connect again to keep posting, reading leads and running ads."
	}
	if !lastAuthFailure.IsZero() && lastAuthFailure.After(connectedAt(a)) {
		return healthExpired, nil, "A recent post failed because the platform no longer accepts this connection. Connect again, then retry the post."
	}
	if a.ExpiresAt == nil || a.Metadata[metaExpiryBasis] == expiryBasisRecheck {
		return healthOK, nil, ""
	}
	exp := a.ExpiresAt.UTC()
	if !exp.After(now) {
		return healthExpired, nil, "This connection has expired. Connect again to keep posting, reading leads and running ads."
	}
	if exp.Before(now.Add(healthRenewWindow)) {
		return healthExpiring, &exp, "This connection expires soon. Connect again before then so posts and ads keep running."
	}
	return healthOK, nil, ""
}

// connectedAt is when the account was last connected: the stamp saveAccounts writes, or the
// row's creation for accounts connected before the stamp existed.
func connectedAt(a *ent.ConnectedAccount) time.Time {
	if v, ok := a.Metadata[metaConnectedAt].(string); ok {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return a.CreatedAt
}

// recentAuthFailures returns, per connected account, the time of the newest post error in the
// lookback window that needs a reconnect, unless a later post to that account was published.
// It reads at most authFailureScanCap rows through the (tenant_id, ...) index; a read error
// never blocks the page, it only hides the prompt.
func (h *Handler) recentAuthFailures(ctx context.Context, tenantID uuid.UUID, now time.Time) map[uuid.UUID]time.Time {
	rows, err := h.db.PublishJob.Query().
		Where(
			publishjob.TenantIDEQ(tenantID),
			publishjob.UpdatedAtGT(now.Add(-authFailureLookback)),
			publishjob.Or(publishjob.ErrorNEQ(""), publishjob.StatusEQ(publishing.StatusPublished)),
		).
		Order(ent.Desc(publishjob.FieldUpdatedAt)).
		Limit(authFailureScanCap).
		Select(publishjob.FieldConnectedAccountID, publishjob.FieldStatus, publishjob.FieldError, publishjob.FieldUpdatedAt).
		All(ctx)
	if err != nil {
		h.logger.Warn("read publishing errors for account health", zap.Error(err))
		return nil
	}
	return authFailuresFrom(rows)
}

// authFailuresFrom reads jobs ordered newest first: the newest job that either published or
// recorded an error decides each account.
func authFailuresFrom(rows []*ent.PublishJob) map[uuid.UUID]time.Time {
	out := map[uuid.UUID]time.Time{}
	decided := map[uuid.UUID]bool{}
	for _, j := range rows {
		if decided[j.ConnectedAccountID] {
			continue
		}
		switch {
		case j.Status == publishing.StatusPublished:
			decided[j.ConnectedAccountID] = true
		case publishing.NeedsReconnect(j.Error):
			decided[j.ConnectedAccountID] = true
			out[j.ConnectedAccountID] = j.UpdatedAt
		}
	}
	return out
}
