package marketingos

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The setup switches off only what cannot work, never touches the owner's own choice, says
// First once, and switches its own change back when the fact arrives.
func TestSetupTeamOnDB(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db)
	s := NewService(db, nil, zap.NewNop()).WithWebPresence(func(context.Context, uuid.UUID) (bool, bool) { return false, true })

	// The owner switched the research analyst off by hand before any setup.
	if _, err := s.UpsertTeamMember(ctx, tid, MemberOverride{Key: "research_analyst", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	first, err := s.SetupTeam(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if !first.First || fitOf(first.Members, "seo_specialist").Enabled || !fitOf(first.Members, "seo_specialist").Changed {
		t.Fatalf("first setup switches SEO off: %+v", first)
	}
	if r := fitOf(first.Members, "research_analyst"); !r.OwnerSet || r.Enabled {
		t.Fatalf("the owner's choice is kept and reported: %+v", r)
	}
	o := overridesByKey(t, s, tid)
	if o["seo_specialist"].SetBy != SetByTeamSetup || !o["seo_specialist"].Disabled || o["research_analyst"].SetBy != "" {
		t.Fatalf("only the setup's own change is marked: %+v", o)
	}

	// The business gets a website: the review switches SEO back on and is no longer First.
	s.WithWebPresence(func(context.Context, uuid.UUID) (bool, bool) { return true, true })
	again, err := s.SetupTeam(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	if again.First || !fitOf(again.Members, "seo_specialist").Enabled || !fitOf(again.Members, "seo_specialist").Changed {
		t.Fatalf("review restores SEO: %+v", again)
	}
	if overridesByKey(t, s, tid)["research_analyst"].Disabled != true {
		t.Fatal("a review never undoes the owner's choice")
	}

	// A review with nothing new changes nothing.
	still, _ := s.SetupTeam(ctx, tid)
	for _, m := range still.Members {
		if m.Changed {
			t.Fatalf("nothing changed, nothing reported as changed: %+v", m)
		}
	}
}

func overridesByKey(t *testing.T, s *Service, tid uuid.UUID) map[string]MemberOverride {
	t.Helper()
	list, err := s.TeamOverrides(context.Background(), tid)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]MemberOverride{}
	for _, o := range list {
		out[o.Key] = o
	}
	return out
}

func fitOf(fits []MemberFit, key string) MemberFit {
	for _, f := range fits {
		if f.Key == key {
			return f
		}
	}
	return MemberFit{}
}

// A new business with nothing connected keeps the full team except the members that cannot work
// at all, each with a reason the owner can act on; the manager and the reviewer are always on.
func TestFitTeamNewBusiness(t *testing.T) {
	fits := fitTeam(TeamFacts{WebsiteKnown: true})
	if len(fits) != len(teamKeys) {
		t.Fatalf("every default member is reported: %d", len(fits))
	}
	for _, k := range []string{"seo_specialist", "sales_rep"} {
		if f := fitOf(fits, k); f.Enabled || f.Reason == "" {
			t.Fatalf("%s is off with a reason: %+v", k, f)
		}
	}
	for _, k := range []string{"manager", "compliance_reviewer", "strategist", "content_creator", "ads_specialist", "leads_manager"} {
		if !fitOf(fits, k).Enabled {
			t.Fatalf("%s stays on", k)
		}
	}
	if len(fitOf(fits, "ads_specialist").Limits) == 0 || len(fitOf(fits, "strategist").Limits) == 0 {
		t.Fatal("what a member cannot do yet is said as a limit")
	}
}

// An unknown website never switches the SEO specialist off, and a business with what each member
// needs has the whole team on with no limits on the ad specialist.
func TestFitTeamComplete(t *testing.T) {
	if !fitOf(fitTeam(TeamFacts{WebsiteKnown: false}), "seo_specialist").Enabled {
		t.Fatal("a website that could not be read leaves SEO alone")
	}
	all := TeamFacts{Website: true, WebsiteKnown: true, WhatsApp: true, SocialPages: true, AdAccount: true, PaidAdsPlan: true,
		LeadCapture: true, Offerings: true, BrandContext: true, Competitors: true}
	for _, f := range fitTeam(all) {
		if !f.Enabled || len(f.Limits) > 0 {
			t.Fatalf("%s should be on with no limits: %+v", f.Key, f)
		}
	}
	if f := fitOf(fitTeam(TeamFacts{AdAccount: true}), "ads_specialist"); len(f.Limits) != 1 || f.Limits[0] == "" {
		t.Fatalf("an ad account without the plan feature names the plan: %+v", f)
	}
}
