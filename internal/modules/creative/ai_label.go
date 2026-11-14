package creative

import (
	"context"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
)

// AILabelKey is the Tenant.metadata key of the owner's AI label choice (owner 2026-11-14): "on"
// (the default) or "off". On, videos with an AI presenter carry an on-screen "AI-generated" note
// and auto-published posts set each platform's AI flag. Off, both are left out; the owner is
// responsible for each platform's rules. The script's honesty rules never change: it never
// claims an AI presenter is a real customer or a real person. It is saved through the profile
// PATCH (tenant.ValidateAILabel).
const AILabelKey = "ai_label"

// AILabelOn reports whether the tenant shows the AI label: anything but "off" is on, so an unset
// or unreadable value keeps the label.
func AILabelOn(t *ent.Tenant) bool {
	if t == nil {
		return true
	}
	s, _ := t.Metadata[AILabelKey].(string)
	return strings.ToLower(strings.TrimSpace(s)) != "off"
}

// TenantAILabel reads the tenant's choice; a tenant that cannot be read keeps the label.
func TenantAILabel(ctx context.Context, db *ent.Client, tenantID uuid.UUID) bool {
	if db == nil {
		return true
	}
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return true
	}
	return AILabelOn(t)
}
