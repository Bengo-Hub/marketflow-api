package flp

import (
	"context"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
)

// BusinessType is the tenant business type of a Forever distributor.
const BusinessType = "flp"

// MetaBusinessType is the metadata key that marks a platform template (sequence, campaign or
// knowledge entry) as Forever content. Knowledge entries from the FLP seed sets carry a
// source_kind starting "flp_" instead.
const MetaBusinessType = "business_type"

// TenantIsFLP reports whether the tenant is a Forever distributor. Every other tenant, the
// Codevertex platform tenant included, gets the generic defaults and never sees Forever
// templates or knowledge. A lookup error counts as not FLP.
func TenantIsFLP(ctx context.Context, client *ent.Client, tenantID uuid.UUID) bool {
	if client == nil || tenantID == uuid.Nil {
		return false
	}
	ok, err := client.Tenant.Query().
		Where(enttenant.ID(tenantID), enttenant.BusinessTypeEqualFold(BusinessType)).
		Exist(ctx)
	return err == nil && ok
}

// NotPlatformFLP is an ent predicate (usable on any table with scope and metadata columns:
// knowledge entries, nurture sequences, campaigns) that leaves out Forever platform rows.
func NotPlatformFLP() func(*entsql.Selector) {
	return func(sel *entsql.Selector) {
		sel.Where(entsql.ExprP("NOT " + PlatformFLPRowSQL(sel.C("scope"), sel.C("metadata"))))
	}
}

// PlatformFLPRowSQL is a SQL condition that is true for a Forever platform row: scope platform
// and marked as Forever content in metadata. scopeCol and metaCol are the quoted column names.
func PlatformFLPRowSQL(scopeCol, metaCol string) string {
	return "(" + scopeCol + " = 'platform' AND (left(COALESCE(" + metaCol + "->>'source_kind', ''), 4) = 'flp_' OR COALESCE(" +
		metaCol + "->>'" + MetaBusinessType + "', '') = '" + BusinessType + "'))"
}
