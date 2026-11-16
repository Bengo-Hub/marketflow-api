package custom_fields

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/customfielddef"
	"github.com/bengobox/marketflow-api/internal/ent/customfieldvalue"
)

// A value is only saved against a field definition of the same tenant and record type.
func TestSetValueChecksTheDefinition(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	mine, theirs := uuid.New(), uuid.New()
	for _, tid := range []uuid.UUID{mine, theirs} {
		if _, err := db.Tenant.Create().SetID(tid).SetName("CF " + tid.String()).SetSlug("cf-" + tid.String()).Save(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.CustomFieldValue.Delete().Where(customfieldvalue.TenantID(tid)).Exec(ctx)
			_, _ = db.CustomFieldDef.Delete().Where(customfielddef.TenantID(tid)).Exec(ctx)
			_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
		})
	}
	svc := NewService(db, zap.NewNop())
	own, err := svc.CreateDef(ctx, CreateDefInput{TenantID: mine, EntityType: "lead", FieldKey: "budget", FieldLabel: "Budget", FieldType: "text"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := svc.CreateDef(ctx, CreateDefInput{TenantID: theirs, EntityType: "lead", FieldKey: "budget", FieldLabel: "Budget", FieldType: "text"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name       string
		def        uuid.UUID
		entityType string
		want       error
	}{
		{"own definition", own.ID, "lead", nil},
		{"another tenant's definition", other.ID, "lead", ErrNotFound},
		{"own definition, other record type", own.ID, "deal", ErrNotFound},
		{"unknown definition", uuid.New(), "lead", ErrNotFound},
	}
	for _, c := range cases {
		_, err := svc.SetValue(ctx, SetValueInput{TenantID: mine, EntityType: c.entityType, EntityID: uuid.New(), FieldDefID: c.def, Value: "1"})
		if (c.want == nil && err != nil) || (c.want != nil && !errors.Is(err, c.want)) {
			t.Errorf("%s: err %v", c.name, err)
		}
	}
	if n := db.CustomFieldValue.Query().Where(customfieldvalue.FieldDefID(other.ID)).CountX(ctx); n != 0 {
		t.Fatalf("a value was saved against another tenant's field: %d", n)
	}
}
