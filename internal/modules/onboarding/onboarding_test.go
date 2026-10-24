package onboarding

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

func ptr[T any](v T) *T { return &v }

func TestComposeFillsOnlyEmptyFields(t *testing.T) {
	ten := &ent.Tenant{Name: "Mama Njeri", Slug: "mama-njeri", BusinessType: tenant.BusinessTypeGeneric,
		Metadata: map[string]any{"bio": "Fresh groceries delivered in Thika the same day."}}
	current := marketingos.BrandContext{Voice: "Warm and short", Audience: []string{"Families in Thika"}}
	d := compose(composeInput{
		tenant:  ten,
		current: current,
		auth:    &tenant.AuthProfile{ContactEmail: "hi@mamanjeri.co.ke", ContactPhone: "+254700000000", LogoURL: "https://x.co/l.png", PrimaryColor: "#118833"},
		top: []creative.TopOffering{
			{Offering: creative.Offering{Name: "Sukuma bunch", Price: ptr(30.0), PhotoURLs: []string{"https://x.co/s.png"}}, QuantitySold: 400, Revenue: 12000},
			{Offering: creative.Offering{Name: "Delivery box", Price: ptr(1250.0)}},
		},
		testimonials: []*ent.Testimonial{{AuthorName: "Wanjiru", Body: "Always fresh."}},
	})

	s := d.Suggested
	if s.Voice != "Warm and short" || len(s.Audience) != 1 {
		t.Fatalf("owner's fields were changed: %+v", s)
	}
	if s.Positioning != "Fresh groceries delivered in Thika the same day." {
		t.Fatalf("positioning from bio: %q", s.Positioning)
	}
	if len(s.Offers) != 2 || s.Offers[0] != "Sukuma bunch (KES 30)" || s.Offers[1] != "Delivery box (KES 1,250)" {
		t.Fatalf("offers: %v", s.Offers)
	}
	if len(s.Proof) != 1 || !strings.Contains(s.Proof[0], "Always fresh.") || !strings.Contains(s.Proof[0], "Wanjiru") {
		t.Fatalf("proof: %v", s.Proof)
	}
	if len(s.BannedClaims) != len(genericBannedClaims) {
		t.Fatalf("generic tenant got %v", s.BannedClaims)
	}
	if d.Offerings[0].Source != "pos" || d.Offerings[1].Source != "inventory" || d.Offerings[0].ImageURL != "https://x.co/s.png" {
		t.Fatalf("offering suggestions: %+v", d.Offerings)
	}
	if d.Profile.ContactEmail != "hi@mamanjeri.co.ke" || d.Profile.WhatsApp != "+254700000000" || d.Profile.BrandColor != "#118833" {
		t.Fatalf("profile defaults: %+v", d.Profile)
	}
	if err := (&s).Validate(); err != nil {
		t.Fatalf("draft must save as is: %v", err)
	}
}

func TestComposeKeepsProfileValuesAndAddsFLPRules(t *testing.T) {
	ten := &ent.Tenant{Name: "Herine", BusinessType: tenant.BusinessTypeFLP, WhatsappNumber: ptr("254711111111"),
		Metadata: map[string]any{"contact_email": "me@x.co", "logo_url": "https://x.co/me.png"}}
	d := compose(composeInput{tenant: ten, auth: &tenant.AuthProfile{ContactEmail: "other@x.co", ContactPhone: "+254799", LogoURL: "https://x.co/other.png"}})
	if d.Profile.ContactEmail != "" || d.Profile.WhatsApp != "" || d.Profile.LogoURL != "" {
		t.Fatalf("auth values must not replace the profile's own: %+v", d.Profile)
	}
	if len(d.Suggested.BannedClaims) != len(genericBannedClaims)+len(flpBannedClaims) {
		t.Fatalf("FLP rules missing: %v", d.Suggested.BannedClaims)
	}
}

func TestComposePlatformTenantUsesWebsite(t *testing.T) {
	ten := &ent.Tenant{Name: "CodeVertex Africa", Slug: "codevertex", Metadata: map[string]any{}}
	d := compose(composeInput{tenant: ten, website: true, catalog: Catalog{
		Products: []WebsiteProduct{{Name: "POS System", Description: "Offline-first point of sale", URL: "https://pos.codevertexafrica.com", Highlights: []string{"Works offline", "KRA eTIMS ready"}}},
		Courses:  []WebsiteCourse{{Name: "Data Analytics", Price: 45000, CoverImage: "/images/c.png"}},
	}})
	if !strings.Contains(d.Suggested.Positioning, "POS System") {
		t.Fatalf("positioning: %q", d.Suggested.Positioning)
	}
	if len(d.Offerings) != 2 || d.Offerings[1].PriceLabel != "KES 45,000" || d.Offerings[1].ImageURL != "" {
		t.Fatalf("website offerings: %+v", d.Offerings)
	}
	if len(d.Suggested.Proof) != 1 || !strings.Contains(d.Suggested.Proof[0], "KRA eTIMS ready") {
		t.Fatalf("proof from highlights: %v", d.Suggested.Proof)
	}
}

func TestKES(t *testing.T) {
	for in, want := range map[float64]string{0: "KES 0", 999: "KES 999", 1000: "KES 1,000", 1234567.4: "KES 1,234,567"} {
		if got := kes(in); got != want {
			t.Errorf("kes(%v) = %q, want %q", in, got, want)
		}
	}
}

const testDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testDSN)
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

func TestChecklistAndDismissInDatabase(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("Onb " + tid.String()).SetSlug("onb-" + tid.String()).
		SetMetadata(map[string]any{"tagline": "Fresh daily", "keep": "me"}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Funnel.Delete().Where(funnel.TenantID(tid)).Exec(ctx)
		_, _ = db.Testimonial.Delete().Where(testimonial.TenantIDEQ(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	brand := marketingos.NewService(db, nil, zap.NewNop())
	svc := NewService(db, brand, nil, nil, nil, "codevertex", zap.NewNop())

	c, err := svc.Checklist(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if c.Total != 5 || c.Done != 0 || c.Complete || c.Dismissed {
		t.Fatalf("fresh tenant: %+v", c)
	}

	// Profile, offerings and brand context written by their own paths; a published funnel.
	if err := metastore.SetKeys(ctx, db, tid, map[string]any{
		"bio": "Groceries in Thika", "contact_email": "a@b.co",
		"offerings": []any{map[string]any{"name": "Sukuma"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := brand.SaveBrandContext(ctx, tid, marketingos.BrandContext{Positioning: "Fresh", Audience: []string{"Families"}, Offers: []string{"Sukuma"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Funnel.Create().SetTenantID(tid).SetName("Join").SetSlug("join-" + tid.String()[:8]).SetStatus("published").Save(ctx); err != nil {
		t.Fatal(err)
	}
	c, err = svc.Checklist(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	done := map[string]bool{}
	for _, s := range c.Steps {
		done[s.Key] = s.Done
	}
	if !done["profile"] || !done["offerings"] || !done["brand_context"] || !done["lead_capture"] || done["channels"] || c.Done != 4 {
		t.Fatalf("after setup: %+v", c)
	}

	if err := svc.SetDismissed(ctx, tid, true); err != nil {
		t.Fatal(err)
	}
	if c, _ = svc.Checklist(ctx, tid); !c.Dismissed {
		t.Fatal("dismiss not stored")
	}
	if err := svc.SetDismissed(ctx, tid, false); err != nil {
		t.Fatal(err)
	}
	if c, _ = svc.Checklist(ctx, tid); c.Dismissed {
		t.Fatal("undismiss not stored")
	}
	// The atomic merges kept every other key, including the brand context written in between.
	ten, _ := db.Tenant.Get(ctx, tid)
	if ten.Metadata["keep"] != "me" || ten.Metadata["tagline"] != "Fresh daily" || ten.Metadata[marketingos.MetadataKey] == nil {
		t.Fatalf("metadata lost keys: %v", ten.Metadata)
	}
}
