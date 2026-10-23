package audit

import (
	"context"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Tenant records a tenant-scope event for the Settings > Activity log page. It is best effort:
// a failed write is logged and never fails the change being audited. The actor is the signed-in
// user's email (or subject) from the request context, else "system".
func Tenant(ctx context.Context, db *ent.Client, log *zap.Logger, tenantID uuid.UUID, event, subjectType string, subjectID *uuid.UUID, payload map[string]any) {
	if db == nil || tenantID == uuid.Nil {
		return
	}
	b := db.AuditLog.Create().
		SetScope("tenant").
		SetTenantID(tenantID).
		SetEvent(event).
		SetActor(actorFrom(ctx))
	if subjectType != "" {
		b.SetSubjectType(subjectType)
	}
	if subjectID != nil && *subjectID != uuid.Nil {
		b.SetSubjectID(*subjectID)
	}
	if payload != nil {
		b.SetPayload(payload)
	}
	if _, err := b.Save(context.WithoutCancel(ctx)); err != nil && log != nil {
		log.Warn("audit write failed", zap.String("event", event), zap.Error(err))
	}
}

func actorFrom(ctx context.Context) string {
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims != nil {
		if claims.Email != "" {
			return claims.Email
		}
		if claims.Subject != "" {
			return claims.Subject
		}
	}
	return "system"
}
