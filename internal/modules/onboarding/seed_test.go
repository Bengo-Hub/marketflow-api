package onboarding

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
)

func filledFields(r SeedReport) map[string]SeedItem {
	out := map[string]SeedItem{}
	for _, it := range r.Filled {
		out[it.Area+"/"+it.Field] = it
	}
	return out
}

// The plan fills only empty fields, names a source for each, and skips what no source covers.
func TestPlanSeedFillsOnlyEmptyFields(t *testing.T) {
	ten := &ent.Tenant{ID: uuid.New(), Name: "Urban Loft", Slug: "urban-loft", BusinessType: tenant.BusinessTypeGeneric,
		Metadata: map[string]any{"bio": "Serviced apartments in Kilimani.", "contact_email": "owner@urbanloft.test"}}
	current := marketingos.BrandContext{Voice: "Calm", Offers: []string{"Studio flat"}}
	in := seedInput{
		tenant:  ten,
		current: current,
		draft: Draft{
			Current:   current,
			Suggested: marketingos.BrandContext{Voice: "Calm", Offers: []string{"Studio flat"}, Proof: []string{"“Spotless” (Amina)"}, BannedClaims: []string{"No false claims"}},
			Profile:   ProfileDefaults{LogoURL: "https://accounts.codevertexafrica.com/images/logo/urban-loft.png", Website: "https://urbanloft.test", WhatsApp: "+254700000001"},
		},
		facts: &marketingos.BusinessFacts{Name: "Urban Loft", Summary: "Urban Loft. Serviced apartments in Kilimani with weekly and monthly stays",
			Address: "Argwings Kodhek Rd, Nairobi", Phone: "+254700000001", Website: "https://urbanloft.test"},
		titles:      []string{"Our opening hours and check-in"},
		titlesKnown: true,
		teamKnown:   true,
	}
	p := planSeed(in)
	got := filledFields(p.report)

	if p.brand.Positioning != "Serviced apartments in Kilimani." || got["brand_context/positioning"].Source != "profile bio" {
		t.Fatalf("positioning: %q %+v", p.brand.Positioning, got["brand_context/positioning"])
	}
	if len(p.brand.Offers) != 0 || p.brand.Voice != "" {
		t.Fatalf("the owner's offers and voice must not be in the plan: %+v", p.brand)
	}
	if _, ok := got["brand_context/proof"]; !ok {
		t.Fatal("proof is empty and has a source; it should be filled")
	}
	if _, ok := p.profile["contact_email"]; ok {
		t.Fatal("contact_email is set; it must not be replaced")
	}
	if p.profile["logo_url"] != nil || p.profile["socials.website"] != "https://urbanloft.test" {
		t.Fatalf("profile plan: %+v", p.profile)
	}
	if _, ok := p.profile["whatsapp_number"]; ok {
		t.Fatal("an account phone is never guessed to be WhatsApp")
	}
	if !p.runTeamSetup {
		t.Fatal("a tenant without a team setup gets the first one")
	}
	var questions []string
	for _, f := range p.faqs {
		questions = append(questions, f.question)
		if f.url != "https://urbanloft.test" || f.source == "" {
			t.Errorf("faq %q needs its source and link: %+v", f.question, f)
		}
	}
	want := []string{"What does Urban Loft do?", "Where is Urban Loft located?", "How do I contact Urban Loft?"}
	if !reflect.DeepEqual(questions, want) {
		t.Fatalf("faqs = %v, want %v (hours are already covered)", questions, want)
	}
	skipped := map[string]bool{}
	for _, s := range p.report.Skipped {
		skipped[s.Area+"/"+s.Field] = true
	}
	for _, k := range []string{"brand_context/offers", "brand_context/audience", "profile/contact_email", "profile/whatsapp_number", "knowledge/faq.hours"} {
		if !skipped[k] {
			t.Errorf("%s should be reported as skipped: %+v", k, p.report.Skipped)
		}
	}
}

// The seed copies no Forever default; every one is inherited when read.
func TestPlanSeedCopiesNoFLPDefaults(t *testing.T) {
	ten := &ent.Tenant{ID: uuid.New(), Name: "FBO", Slug: "254000000001", BusinessType: tenant.BusinessTypeFLP,
		Metadata: map[string]any{"bio": "Mine", "tagline": "", "offerings": []any{map[string]any{"name": "Aloe Vera Gel"}}}}
	p := planSeed(seedInput{tenant: ten})
	if _, ok := p.profile["bio"]; ok {
		t.Fatal("bio is the owner's")
	}
	if _, ok := p.profile["tagline"]; ok {
		t.Fatal("a present key, even empty, is the owner's")
	}
	if _, ok := p.profile["story"]; ok {
		t.Fatal("a default is inherited, never copied")
	}
	if len(p.brand.Offers) != 1 || p.brand.Offers[0] != "Aloe Vera Gel" {
		t.Fatalf("Forever offers come from the featured products: %+v", p.brand.Offers)
	}
}

// A storefront's generated line, or a description that only repeats the name, is not taken.
func TestOwnDescription(t *testing.T) {
	if ownDescription("Alpha China Market Marketplace. Order from Alpha China Market for pickup or delivery", "Alpha China Market") {
		t.Fatal("the storefront template was taken")
	}
	if ownDescription("Mama Njeri. Mama Njeri", "Mama Njeri") {
		t.Fatal("a description that is only the name was taken")
	}
	if !ownDescription("Urban Loft Cafe, Busia. Cafe and restaurant in Busia, Kenya, open every day from 7:30 AM", "THE URBAN LOFT CAFE LIMITED") {
		t.Fatal("a real description was refused")
	}
}

// An excluded fill leaves the plan and is reported as skipped; other tenants keep it.
func TestPlanSeedExclude(t *testing.T) {
	ten := &ent.Tenant{ID: uuid.New(), Slug: "mss", BusinessType: tenant.BusinessTypeGeneric, Metadata: map[string]any{"tagline": "Tangible"}}
	in := seedInput{tenant: ten, draft: Draft{Suggested: marketingos.BrandContext{Offers: []string{"Milestone 2"}}},
		facts: &marketingos.BusinessFacts{Name: "MSS", Summary: "MSS", Website: "https://mss.test"}, titlesKnown: true}
	p := planSeed(in)
	p.exclude("mss", []string{"mss:brand_context/offers", "other:brand_context/positioning", "knowledge/faq.about"})
	if len(p.brand.Offers) != 0 || p.brand.Positioning == "" {
		t.Fatalf("brand plan after exclude: %+v", p.brand)
	}
	for _, f := range p.faqs {
		if f.topic == "about" {
			t.Fatal("the about FAQ was excluded for every tenant")
		}
	}
	got := filledFields(p.report)
	if _, ok := got["brand_context/offers"]; ok {
		t.Fatal("excluded offers still reported as filled")
	}
	if strings.Join(p.brandFields, ",") != "positioning" {
		t.Fatalf("brand fields = %v", p.brandFields)
	}
}

// The run writes only empty fields, a dry run writes nothing, and a second run changes nothing.
func TestSeedDefaultsFillOnlyAndIdempotent(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	meta := map[string]any{
		"bio":                   "Fresh groceries delivered in Thika the same day.",
		"contact_email":         "owner@mamanjeri.test",
		marketingos.MetadataKey: map[string]any{"voice": "Warm and short", "offers": []any{"Owner's own offer"}},
	}
	if _, err := db.Tenant.Create().SetID(tid).SetName("Seed " + tid.String()).SetSlug("seed-" + tid.String()).
		SetBusinessType("generic").SetMetadata(meta).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(tid)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	mos := marketingos.NewService(db, nil, zap.NewNop())
	kb := knowledge.NewService(db, zap.NewNop())
	svc := NewService(db, mos, nil, nil, nil, "codevertex", zap.NewNop()).WithSeed(SeedDeps{
		SaveBrand: mos.SaveBrandContext,
		Facts: func(context.Context, uuid.UUID) (marketingos.BusinessFacts, error) {
			return marketingos.BusinessFacts{Name: "Mama Njeri", Summary: "Mama Njeri. Fresh groceries delivered to homes and offices in Thika every day",
				Address: "Kenyatta Hwy, Thika", Email: "owner@mamanjeri.test", Website: "https://mamanjeri.test"}, nil
		},
		KBTitles: kb.TenantTitles, DraftFAQ: kb.DraftFAQ,
		TeamSetupDone: mos.TeamSetupDone, SetupTeam: mos.SetupTeamIfNew,
	})

	// Dry run: a full report, nothing written.
	dry, err := svc.SeedDefaults(ctx, tid, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Filled) == 0 {
		t.Fatalf("the dry run should report fills: %+v", dry)
	}
	after, _ := db.Tenant.Get(ctx, tid)
	if !reflect.DeepEqual(after.Metadata, normalise(t, meta)) {
		t.Fatalf("the dry run wrote metadata: %+v", after.Metadata)
	}
	if n, _ := db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).Count(ctx); n != 0 {
		t.Fatalf("the dry run wrote %d entries", n)
	}

	// Real run.
	if _, err := svc.SeedDefaults(ctx, tid, false); err != nil {
		t.Fatal(err)
	}
	bc, _ := mos.GetBrandContext(ctx, tid)
	if bc.Voice != "Warm and short" || len(bc.Offers) != 1 || bc.Offers[0] != "Owner's own offer" {
		t.Fatalf("the owner's fields changed: %+v", bc)
	}
	if bc.Positioning != "Fresh groceries delivered in Thika the same day." || len(bc.BannedClaims) != 0 || len(bc.SharedBannedClaims) == 0 {
		t.Fatalf("empty fields not filled: %+v", bc)
	}
	ten, _ := db.Tenant.Get(ctx, tid)
	if ten.Metadata["contact_email"] != "owner@mamanjeri.test" {
		t.Fatalf("contact_email changed: %v", ten.Metadata["contact_email"])
	}
	entries, err := db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).All(ctx)
	if err != nil || len(entries) != 3 {
		t.Fatalf("drafted entries = %d (%v), want about, location and contact", len(entries), err)
	}
	for _, e := range entries {
		if e.Metadata["suggested"] != true || e.Metadata["suggested_from"] != knowledge.SuggestedFromSeed ||
			e.Metadata["source_url"] != "https://mamanjeri.test" || e.Metadata["drafted_at"] == nil {
			t.Errorf("entry %q is not a labelled pending draft: %+v", e.Title, e.Metadata)
		}
		if !strings.Contains(e.Title, "Mama Njeri") {
			t.Errorf("title %q", e.Title)
		}
	}

	// Second run: nothing left to fill, nothing changes.
	again, err := svc.SeedDefaults(ctx, tid, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Filled) != 0 {
		t.Fatalf("a second run filled %+v", again.Filled)
	}
	ten2, _ := db.Tenant.Get(ctx, tid)
	if !reflect.DeepEqual(ten2.Metadata[marketingos.MetadataKey], ten.Metadata[marketingos.MetadataKey]) {
		t.Fatal("the brand context changed on the second run")
	}
	if n, _ := db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.TenantID(tid)).Count(ctx); n != 3 {
		t.Fatalf("the second run changed the entries: %d", n)
	}
}

// normalise gives a metadata map the shape it has after a JSON round trip through the database.
func normalise(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	out := map[string]any{}
	for k, v := range m {
		if inner, ok := v.(map[string]any); ok {
			cp := map[string]any{}
			for ik, iv := range inner {
				cp[ik] = iv
			}
			out[k] = cp
			continue
		}
		out[k] = v
	}
	return out
}
