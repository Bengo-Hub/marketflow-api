package custom_fields

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customfielddef"
	"github.com/bengobox/marketflow-api/internal/ent/customfieldvalue"
)

func TestValidateCreate(t *testing.T) {
	ok := CreateDefInput{EntityType: "lead", FieldKey: "company_size", FieldLabel: " Company size ", FieldType: "select", Options: []string{" Small", "Large", "Small", ""}}
	if err := validateCreate(&ok); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if ok.FieldLabel != "Company size" || len(ok.Options) != 2 {
		t.Fatalf("not normalised: %+v", ok)
	}
	text := CreateDefInput{EntityType: "contact", FieldKey: "notes_x", FieldLabel: "Notes", FieldType: "text", Options: []string{"ignored"}}
	if err := validateCreate(&text); err != nil || text.Options != nil {
		t.Fatalf("text field keeps no options: %v %+v", err, text)
	}
	for name, in := range map[string]CreateDefInput{
		"entity":       {EntityType: "invoice", FieldKey: "x_y", FieldLabel: "X", FieldType: "text"},
		"type":         {EntityType: "lead", FieldKey: "x_y", FieldLabel: "X", FieldType: "json"},
		"key caps":     {EntityType: "lead", FieldKey: "Company", FieldLabel: "X", FieldType: "text"},
		"key space":    {EntityType: "lead", FieldKey: "company size", FieldLabel: "X", FieldType: "text"},
		"no label":     {EntityType: "lead", FieldKey: "x_y", FieldLabel: "  ", FieldType: "text"},
		"choice empty": {EntityType: "lead", FieldKey: "x_y", FieldLabel: "X", FieldType: "multiselect", Options: []string{" "}},
	} {
		in := in
		if err := validateCreate(&in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable")
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// Deleting a field that has saved values used to fail on the values' foreign key.
func TestDeleteDefRemovesValuesAndDuplicateKeyIsRefused(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("CF " + tid.String()).SetSlug("cf-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.CustomFieldValue.Delete().Where(customfieldvalue.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomFieldDef.Delete().Where(customfielddef.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	svc := NewService(db, zap.NewNop())
	def, err := svc.CreateDef(ctx, CreateDefInput{TenantID: tid, EntityType: "lead", FieldKey: "budget", FieldLabel: "Budget", FieldType: "number"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateDef(ctx, CreateDefInput{TenantID: tid, EntityType: "lead", FieldKey: "budget", FieldLabel: "Again", FieldType: "text"}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate key: want ErrDuplicate, got %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := db.CustomFieldValue.Create().SetTenantID(tid).SetEntityType("lead").SetEntityID(uuid.New()).SetFieldDefID(def.ID).SetValue("100").Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := svc.DeleteDef(ctx, tid, def.ID)
	if err != nil || removed != 3 {
		t.Fatalf("delete: removed=%d err=%v", removed, err)
	}
	if n, _ := db.CustomFieldDef.Query().Where(customfielddef.ID(def.ID)).Count(ctx); n != 0 {
		t.Fatal("definition still there")
	}
	if _, err := svc.DeleteDef(ctx, tid, def.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: want ErrNotFound, got %v", err)
	}
	// Another tenant's id is never deleted through this tenant.
	if _, err := svc.DeleteDef(ctx, uuid.New(), uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: want ErrNotFound, got %v", err)
	}
}
