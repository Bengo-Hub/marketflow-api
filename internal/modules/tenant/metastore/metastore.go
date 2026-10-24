// Package metastore writes tenant metadata keys atomically. It is a leaf package (ent and
// uuid only) so every module that owns a metadata key (tenant profile, marketingos brand
// context and switches, onboarding) can use it without import cycles.
package metastore

import (
	"context"
	"encoding/json"
	"fmt"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
)

// SetKeys merges values into a tenant's metadata in one UPDATE (metadata || patch), so
// concurrent writers of different keys never overwrite each other the way a
// read-modify-write of the whole map does. A key set to nil is removed. db may be a
// transaction's client (tx.Client()).
func SetKeys(ctx context.Context, db *ent.Client, tenantID uuid.UUID, values map[string]any) error {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]any, len(values))
	var drop []string
	for k, v := range values {
		if v == nil {
			drop = append(drop, k)
			continue
		}
		set[k] = v
	}
	patch, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("encode metadata: %w", err)
	}
	// ExprFunc with b.Arg so the builder numbers the placeholders ($1, $2...); a "?" inside
	// sql.Expr is not rewritten in an UPDATE's SET clause.
	value := sql.ExprFunc(func(b *sql.Builder) {
		b.WriteString("((COALESCE(" + enttenant.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
		for _, k := range drop {
			b.WriteString(" - ")
			b.Arg(k)
		}
		b.WriteString(")")
	})
	n, err := db.Tenant.Update().
		Where(enttenant.ID(tenantID)).
		Modify(func(u *sql.UpdateBuilder) {
			u.Set(enttenant.FieldMetadata, value)
		}).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("update tenant metadata: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("tenant not found")
	}
	return nil
}
