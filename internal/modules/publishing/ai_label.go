package publishing

import (
	"context"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/google/uuid"
)

// aiLabelKey is the tenant's AI label choice, the same key as creative.AILabelKey (this package
// does not import creative). Owner 2026-11-14: it covers every MarketFlow post, the ones the
// owner schedules in the Studio and the autopilot's alike.
const aiLabelKey = "ai_label"

// aiLabelOn reports whether the tenant's posts carry each platform's AI flag: anything but "off"
// is on, and a tenant that cannot be read keeps the flag.
func aiLabelOn(ctx context.Context, db *ent.Client, tenantID uuid.UUID) bool {
	if db == nil {
		return true
	}
	t, err := db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return true
	}
	s, _ := t.Metadata[aiLabelKey].(string)
	return strings.ToLower(strings.TrimSpace(s)) != "off"
}
