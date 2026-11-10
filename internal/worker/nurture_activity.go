package worker

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// logNurtureActivity records a sent nurture message on the lead's timeline (Sprint 23 L29:
// every automated message is an Activity). Best effort: the send already happened.
func logNurtureActivity(ctx context.Context, db *ent.Client, tenantID, leadID uuid.UUID, channel, subject, body, ref string, meta map[string]any) error {
	if len(body) > 4000 {
		body = body[:4000]
	}
	b := db.Activity.Create().
		SetTenantID(tenantID).
		SetEntityType("lead").
		SetEntityID(leadID).
		SetActivityType(channel).
		SetDirection("outbound").
		SetSource("agent").
		SetPerformedAt(time.Now().UTC()).
		SetMetadata(meta)
	if subject != "" {
		b.SetSubject(subject)
	}
	if body != "" {
		b.SetBody(body)
	}
	if ref != "" {
		b.SetExternalRef(ref)
	}
	return b.Exec(ctx)
}
