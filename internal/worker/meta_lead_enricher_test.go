package worker

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"go.uber.org/zap"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
)

func TestParseMetaLeadFields(t *testing.T) {
	got := parseMetaLeadFields([]metaFieldDatum{
		{Name: "full_name", Values: []string{"  Jane  Wanjiku Doe "}},
		{Name: "EMAIL", Values: []string{"jane@example.com"}},
		{Name: "phone_number", Values: []string{"+254700000001"}},
		{Name: "which_course", Values: []string{"Python"}},
		{Name: "interests", Values: []string{"POS", "ERP"}},
		{Name: "empty_answer", Values: []string{""}},
		{Name: "no_values"},
	})
	if got.FirstName != "Jane" || got.LastName != "Wanjiku Doe" {
		t.Fatalf("full_name split = %q / %q", got.FirstName, got.LastName)
	}
	if got.Email != "jane@example.com" || got.Phone != "+254700000001" {
		t.Fatalf("email/phone = %q / %q", got.Email, got.Phone)
	}
	if got.Answers["which_course"] != "Python" {
		t.Fatalf("custom answer lost: %+v", got.Answers)
	}
	if v, ok := got.Answers["interests"].([]string); !ok || len(v) != 2 {
		t.Fatalf("multi-value answer lost: %+v", got.Answers["interests"])
	}
	if _, ok := got.Answers["empty_answer"]; ok {
		t.Fatal("empty answers must be skipped")
	}

	// Explicit first/last name wins over full_name regardless of order.
	got = parseMetaLeadFields([]metaFieldDatum{
		{Name: "first_name", Values: []string{"Ann"}},
		{Name: "last_name", Values: []string{"Otieno"}},
		{Name: "full_name", Values: []string{"Somebody Else"}},
	})
	if got.FirstName != "Ann" || got.LastName != "Otieno" {
		t.Fatalf("first/last must win over full_name: %q %q", got.FirstName, got.LastName)
	}
}

func TestMetaLeadEnricherEndToEnd(t *testing.T) {
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
	if _, err := db.Tenant.Create().SetID(tid).SetName("Meta " + tid.String()).SetSlug("meta-" + tid.String()).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Cleanup touches only this test's own tenant id.
	t.Cleanup(func() {
		_, _ = db.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
		_, _ = db.ProviderSetting.Delete().Where(providersetting.TenantID(tid.String())).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	if _, err := db.ProviderSetting.Create().SetTenantID(tid.String()).SetProviderType("meta_ads").SetProviderName("page").
		SetKey("page_token").SetValue("PAGE-TOKEN-1").SetIsSecret(true).Save(ctx); err != nil {
		t.Fatal(err)
	}

	status := http.StatusOK
	calls := 0
	graph := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("access_token") != "PAGE-TOKEN-1" || !strings.HasSuffix(r.URL.Path, "/LG-1") {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"field_data":[{"name":"full_name","values":["Jane Doe"]},{"name":"email","values":["jane+` + tid.String()[:8] + `@example.com"]},{"name":"phone_number","values":["+254700000002"]},{"name":"course","values":["Python"]}]}`))
		}
	}))
	defer graph.Close()

	h := &MetaLeadEnricher{
		db: db, tokens: NewMetaTokenResolver(db, nil, zap.NewNop()),
		contacts: contacts.NewService(db, zap.NewNop()), hc: graph.Client(), graphBase: graph.URL, logger: zap.NewNop(),
	}
	mkLead := func() *ent.Lead {
		l, err := db.Lead.Create().SetTenantID(tid).SetSource("meta_ad").SetLeadType("customer").
			SetMetadata(map[string]any{"meta_leadgen_id": "LG-1"}).Save(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	msgFor := func(l *ent.Lead) fakeMsg {
		return fakeMsg{data: envelope(t, tid, l.ID, map[string]any{"lead_id": l.ID.String(), "source": "meta_ad", "lead_type": "customer"})}
	}

	t.Run("5xx is retried", func(t *testing.T) {
		status = http.StatusBadGateway
		l := mkLead()
		if err := h.Handle(ctx, msgFor(l)); err == nil {
			t.Fatal("a Graph 5xx must return an error so JetStream redelivers")
		}
		_ = db.Lead.DeleteOneID(l.ID).Exec(ctx)
	})
	t.Run("4xx is permanent and acked", func(t *testing.T) {
		status = http.StatusBadRequest
		l := mkLead()
		if err := h.Handle(ctx, msgFor(l)); err != nil {
			t.Fatalf("a Graph 4xx must be acked, got %v", err)
		}
		_ = db.Lead.DeleteOneID(l.ID).Exec(ctx)
	})
	t.Run("enriches the lead and creates the contact once", func(t *testing.T) {
		status = http.StatusOK
		l := mkLead()
		if err := h.Handle(ctx, msgFor(l)); err != nil {
			t.Fatal(err)
		}
		got, err := db.Lead.Get(ctx, l.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Email == nil || got.Phone == nil || got.FirstName == nil || *got.FirstName != "Jane" || got.QualificationAnswers["course"] != "Python" {
			t.Fatalf("lead not enriched: %+v", got)
		}
		if _, ok := got.Metadata["meta_enriched_at"]; !ok || got.Metadata["meta_leadgen_id"] != "LG-1" {
			t.Fatalf("metadata not kept or stamped: %+v", got.Metadata)
		}
		n, _ := db.Contact.Query().Where(contact.TenantID(tid)).Count(ctx)
		if n != 1 {
			t.Fatalf("contacts = %d, want 1", n)
		}
		c, _ := db.Contact.Query().Where(contact.TenantID(tid)).Only(ctx)
		if c.SubscribedEmail || c.SubscribedSms {
			t.Fatal("enrichment must never opt the contact in to marketing")
		}

		before := calls
		if err := h.Handle(ctx, msgFor(l)); err != nil {
			t.Fatal(err)
		}
		if calls != before {
			t.Fatal("a redelivery of an enriched lead must not call Graph again")
		}
		if n, _ := db.Contact.Query().Where(contact.TenantID(tid)).Count(ctx); n != 1 {
			t.Fatalf("redelivery created another contact: %d", n)
		}
	})
	t.Run("non-meta leads are ignored", func(t *testing.T) {
		before := calls
		l, _ := db.Lead.Create().SetTenantID(tid).SetSource("website").SetLeadType("customer").Save(ctx)
		if err := h.Handle(ctx, fakeMsg{data: envelope(t, tid, l.ID, map[string]any{"lead_id": l.ID.String(), "source": "website"})}); err != nil {
			t.Fatal(err)
		}
		if calls != before {
			t.Fatal("a website lead must not call Graph")
		}
	})
}
