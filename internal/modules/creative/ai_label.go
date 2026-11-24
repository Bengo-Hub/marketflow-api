package creative

import (
	"context"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
	"github.com/google/uuid"
)

// AILabelKey is the Tenant.metadata key of the owner's AI label choice (owner 2026-11-14): "on"
// (the default) or "off". On, posts carry each platform's AI flag, and videos may show the on-screen
// note (AILabelTextKey, off by default). Off, both are left out; the owner is
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

// AILabelTextKey is the Tenant.metadata key of the on-screen "AI-generated" note on presenter ads,
// "on" or "off". Unset follows the platform setting of the same name under "creative."
// (AILabelTextConfigKey), which is off (owner, 2026-11-25). It is separate from AILabelKey: the
// flag each platform asks for on a post stays on unless the business turns that off.
const (
	AILabelTextKey       = "ai_label_text"
	AILabelTextConfigKey = "creative.ai_label_text"
)

// TenantAILabelText reports whether the tenant's videos show the on-screen note. A tenant that
// turned the AI label off shows none.
func TenantAILabelText(ctx context.Context, db *ent.Client, tenantID uuid.UUID) bool {
	if db == nil {
		return false
	}
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil || !AILabelOn(t) {
		return false
	}
	if s, _ := t.Metadata[AILabelTextKey].(string); strings.TrimSpace(s) != "" {
		return strings.EqualFold(strings.TrimSpace(s), "on")
	}
	v := svcconfig.Value(ctx, db, uuid.Nil, AILabelTextConfigKey)
	return strings.EqualFold(v, "on") || strings.EqualFold(v, "true")
}
