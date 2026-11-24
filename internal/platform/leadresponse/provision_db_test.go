package leadresponse

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
)

const testLeadResponseDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openTestDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testLeadResponseDSN)
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

func seedTenant(t *testing.T, db *ent.Client, meta map[string]any) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tid := uuid.New()
	b := db.Tenant.Create().SetID(tid).SetName("LR " + tid.String()).SetSlug("lr-" + tid.String())
	if meta != nil {
		b.SetMetadata(meta)
	}
	if _, err := b.Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	return tid
}

// A tenant gets one copy of the default follow-up, made once even when several replicas race;
// a tenant that opted out, or already has its own lead.created sequence, gets none.
func TestProvisionFollowUpOncePerTenant(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	if tpl, err := DefaultTemplate(ctx, db); err != nil {
		t.Fatal(err)
	} else if tpl == nil {
		home := seedTenant(t, db, nil)
		if _, err := db.NurtureSequence.Create().SetTenantID(home).SetScope("platform").SetName("Instant follow-up").
			SetTriggerEvent(TriggerLeadCreated).SetActive(true).
			SetSteps([]map[string]any{{"delay_hours": 0, "channel": "email", "subject": "Hi", "body": "Hello"}}).
			SetMetadata(map[string]any{TemplateFlag: true, "platform_template": true}).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}

	tid := seedTenant(t, db, nil)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := ProvisionFollowUp(ctx, db, tid); err != nil {
				t.Errorf("provision: %v", err)
			}
		}()
	}
	wg.Wait()
	copies, err := db.NurtureSequence.Query().Where(nurturesequence.TenantID(tid), nurturesequence.ScopeEQ("tenant")).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) != 1 || !IsDefaultCopy(copies[0]) || !copies[0].Active || copies[0].TriggerEvent != TriggerLeadCreated {
		t.Fatalf("want one active default copy, got %d", len(copies))
	}
	if tn, _ := db.Tenant.Get(ctx, tid); tn.Metadata[ProvisionedKey] == nil {
		t.Fatal("the tenant must be marked provisioned")
	}
	// Deleting the copy does not bring a new one.
	_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(tid)).Exec(ctx)
	if seq, err := ProvisionFollowUp(ctx, db, tid); err != nil || seq != nil {
		t.Fatalf("a deleted copy must not come back: %v %v", seq, err)
	}

	optedOut := seedTenant(t, db, map[string]any{TenantKey: map[string]any{"follow_up": false}})
	if seq, err := ProvisionFollowUp(ctx, db, optedOut); err != nil || seq != nil {
		t.Fatalf("opted-out tenant got a copy: %v %v", seq, err)
	}
	own := seedTenant(t, db, nil)
	if _, err := db.NurtureSequence.Create().SetTenantID(own).SetName("Mine").SetTriggerEvent(TriggerLeadCreated).
		SetActive(true).SetSteps([]map[string]any{}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	if seq, err := ProvisionFollowUp(ctx, db, own); err != nil || seq != nil {
		t.Fatalf("a tenant with its own sequence got a copy: %v %v", seq, err)
	}
}

func TestMarkContactedOnlyMovesNewLeads(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, nil)
	mk := func(status string) uuid.UUID {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").SetStatus(status).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l.ID
	}
	fresh, won := mk("new"), mk("converted")
	at := time.Now().UTC().Truncate(time.Second)
	if ok, err := MarkContacted(ctx, db, tid, fresh, at); err != nil || !ok {
		t.Fatalf("new lead: %v %v", ok, err)
	}
	if ok, _ := MarkContacted(ctx, db, tid, fresh, at.Add(time.Hour)); ok {
		t.Fatal("second call must not change it again")
	}
	if ok, _ := MarkContacted(ctx, db, tid, won, at); ok {
		t.Fatal("a converted lead must stay converted")
	}
	if ok, _ := MarkContacted(ctx, db, uuid.New(), fresh, at); ok {
		t.Fatal("another tenant's id must not match")
	}
	l, _ := db.Lead.Get(ctx, fresh)
	if l.Status != "contacted" || l.ContactedAt == nil || !l.ContactedAt.Equal(at) {
		t.Fatalf("lead %s contacted_at %v", l.Status, l.ContactedAt)
	}
}

func TestEnsureStartedAtIsWrittenOnce(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	first, err := EnsureStartedAt(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	again, err := EnsureStartedAt(ctx, db, time.Now().Add(48*time.Hour))
	if err != nil || !again.Equal(first) {
		t.Fatalf("start moved: %s then %s (%v)", first, again, err)
	}
}
