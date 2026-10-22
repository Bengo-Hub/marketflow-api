package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

func TestAdAttributed(t *testing.T) {
	cases := []struct {
		name string
		l    *ent.Lead
		want bool
	}{
		{"nil", nil, false},
		{"meta ad", &ent.Lead{Source: "meta_ad"}, true},
		{"tiktok ad", &ent.Lead{Source: "tiktok_ad"}, true},
		{"website with fbclid", &ent.Lead{Source: "website", Metadata: map[string]any{"fbclid": "fb.1"}}, true},
		{"whatsapp with ctwa_clid", &ent.Lead{Source: "whatsapp", Metadata: map[string]any{"ctwa_clid": "c1"}}, true},
		{"organic", &ent.Lead{Source: "organic"}, false},
		{"empty click id", &ent.Lead{Source: "website", Metadata: map[string]any{"gclid": ""}}, false},
	}
	for _, tc := range cases {
		if got := adAttributed(tc.l); got != tc.want {
			t.Errorf("%s: adAttributed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEnqueueAdConversionOncePerOrder(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testWorkerPostgresDSN)
	if err != nil {
		t.Skipf("local postgres unavailable: %v", err)
	}
	if err := sqlDB.Ping(); err != nil {
		_ = sqlDB.Close()
		t.Skipf("local postgres unreachable: %v", err)
	}
	defer sqlDB.Close()
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	defer db.Close()
	ctx := context.Background()

	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("Conv " + tid.String()).SetSlug("conv-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})

	adLead, _ := db.Lead.Create().SetTenantID(tid).SetSource("meta_ad").SetLeadType("customer").
		SetMetadata(map[string]any{"fbclid": "fb.1.123"}).Save(ctx)
	email := "buyer@example.com"
	adContact, err := db.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLeadID(adLead.ID).SetEmail(email).Save(ctx)
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	organicLead, _ := db.Lead.Create().SetTenantID(tid).SetSource("organic").SetLeadType("customer").Save(ctx)
	organicContact, _ := db.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLeadID(organicLead.ID).Save(ctx)
	noLeadContact, _ := db.Contact.Create().SetTenantID(tid).SetContactType("customer").Save(ctx)

	wrote, err := enqueueAdConversion(ctx, db, tid, adContact.ID, "pos:order-1", 2500, "KES", "order")
	if err != nil || !wrote {
		t.Fatalf("ad-attributed sale must queue a conversion: wrote=%v err=%v", wrote, err)
	}
	row, err := db.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).Only(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ev, err := eventslib.FromJSON(row.Payload)
	if err != nil || ev.Subject() != "conversion.occurred" {
		t.Fatalf("outbox row does not publish conversion.occurred: %v", err)
	}
	// The ConversionsHandler reads the payload through eventBody.
	var body ConversionEvent
	if err := json.Unmarshal(eventBody(row.Payload), &body); err != nil {
		t.Fatal(err)
	}
	if body.EventName != "Purchase" || body.Value != 2500 || body.Fbclid != "fb.1.123" || body.Email != email || body.ClientUserID != adLead.ID.String() || body.TenantID != tid.String() {
		t.Fatalf("unexpected conversion body %+v", body)
	}

	if wrote, err := enqueueAdConversion(ctx, db, tid, adContact.ID, "pos:order-1", 2500, "KES", "order"); err != nil || wrote {
		t.Fatalf("a redelivered sale must not queue a second conversion: wrote=%v err=%v", wrote, err)
	}
	for name, cid := range map[string]uuid.UUID{"organic lead": organicContact.ID, "no lead": noLeadContact.ID} {
		if wrote, err := enqueueAdConversion(ctx, db, tid, cid, "pos:order-"+name, 100, "KES", "order"); err != nil || wrote {
			t.Fatalf("%s must not queue a conversion: wrote=%v err=%v", name, wrote, err)
		}
	}
	if n, _ := db.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).Count(ctx); n != 1 {
		t.Fatalf("outbox rows = %d, want 1", n)
	}
}
