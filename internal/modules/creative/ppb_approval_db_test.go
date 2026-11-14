package creative

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// pharmacyTenant adds a tenant row with the business type and removes it afterwards (the creative
// rows go with newTenant's cleanup).
func pharmacyTenant(t *testing.T, db *ent.Client, businessType string) uuid.UUID {
	t.Helper()
	tid := newTenant(t, db)
	ctx := context.Background()
	if _, err := db.Tenant.Create().SetID(tid).SetName("PH " + tid.String()).SetSlug("ph-" + tid.String()).
		SetBusinessType(businessType).SetMetadata(map[string]any{}).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	return tid
}

func adBrief(t *testing.T, s *Service, tid uuid.UUID, offering map[string]any) *ent.CreativeBrief {
	t.Helper()
	b, err := s.CreateBrief(context.Background(), tid, nil, BriefInput{Title: ptr("Weekend ad"), Offering: offering})
	if err != nil {
		t.Fatalf("create brief: %v", err)
	}
	return b
}

// readyPoster makes a poster draft and marks it ready for review, as the renderer would.
func readyPoster(t *testing.T, s *Service, tid, briefID uuid.UUID) *ent.CreativeVersion {
	t.Helper()
	ctx := context.Background()
	v, err := s.CreateVersion(ctx, tid, briefID, VersionInput{Prompt: "a shelf", Subline: "For pain and fever"})
	if err != nil {
		t.Fatalf("create version: %v", err)
	}
	return s.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusReady).SaveX(ctx)
}

// A pharmacy's medicine ad stays a draft until the PPB approval reference is entered (owner,
// 2026-11-14); a lotion and another business type's medicine ad are approved as before.
func TestPPBApprovalHoldsPharmacyMedicineAds(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := pharmacyTenant(t, db, BusinessTypePharmacy)
	panadol := map[string]any{"source": "inventory", "source_id": "p-1", "name": "Panadol Extra 500mg"}

	b := adBrief(t, s, tid, panadol)
	v := readyPoster(t, s, tid, b.ID)
	if strings.Contains(str(v.Copy["subline"]), "PPB") {
		t.Fatal("no reference yet, so none on the poster")
	}
	_, err := s.Approve(ctx, tid, v.ID, nil)
	var held *PPBApprovalRequiredError
	if !errors.Is(err, ErrPPBApprovalRequired) || !errors.As(err, &held) || held.OfferingKey != "inventory:p-1" || held.OfferingName != "Panadol Extra 500mg" {
		t.Fatalf("approval must be held with the product named, got %v", err)
	}
	if got := db.CreativeVersion.GetX(ctx, v.ID); got.Status != StatusReady {
		t.Fatalf("the draft must stay a draft, got %s", got.Status)
	}
	st, err := s.PPBStatusForBrief(ctx, tid, b.ID)
	if err != nil || !st.Held() {
		t.Fatalf("status must show the hold: %+v %v", st, err)
	}

	// The owner enters the reference; the ad can be approved and carries it.
	if _, err := s.SetPPBApproval(ctx, tid, "inventory:p-1", "Panadol Extra 500mg", " PPB/ADV/2026/0001 ", ptr(uuid.New())); err != nil {
		t.Fatal(err)
	}
	approved, err := s.Approve(ctx, tid, v.ID, nil)
	if err != nil {
		t.Fatalf("approve with a reference: %v", err)
	}
	stamp, _ := approved.Metadata[MetaPPBApproval].(map[string]any)
	if approved.Status != StatusApproved || str(stamp["reference"]) != "PPB/ADV/2026/0001" || str(stamp["caption_line"]) != "PPB approval ref. PPB/ADV/2026/0001" {
		t.Fatalf("approval must record the reference: %s %v", approved.Status, approved.Metadata)
	}
	// A new poster for the product shows the reference on its subline.
	next, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "a shelf", Subline: "For pain and fever"})
	if err != nil {
		t.Fatal(err)
	}
	if got := str(next.Copy["subline"]); got != "For pain and fever. PPB approval ref. PPB/ADV/2026/0001" {
		t.Fatalf("poster subline: %q", got)
	}

	// A plain availability post for a medicine is not an advert: approved without a reference
	// (owner, 2026-11-14). It uses a second product, which has no reference.
	coartem := adBrief(t, s, tid, map[string]any{"source": "inventory", "source_id": "c-1", "name": "Coartem 20/120 tablets"})
	stock, err := s.CreateVersion(ctx, tid, coartem.ID, VersionInput{Prompt: "a shelf", Headline: "Coartem in stock", Subline: "Open till 9pm"})
	if err != nil {
		t.Fatal(err)
	}
	db.CreativeVersion.UpdateOneID(stock.ID).SetStatus(StatusReady).ExecX(ctx)
	if _, err := s.Approve(ctx, tid, stock.ID, nil); err != nil {
		t.Fatalf("an availability post must not be held: %v", err)
	}
	promo := readyPoster(t, s, tid, coartem.ID) // "For pain and fever" promotes use
	if _, err := s.Approve(ctx, tid, promo.ID, nil); !errors.Is(err, ErrPPBApprovalRequired) {
		t.Fatalf("a promotional poster of the same product must be held, got %v", err)
	}

	// A supplement is not held either.
	omega := adBrief(t, s, tid, map[string]any{"name": "Seven Seas Omega 3 capsules"})
	if _, err := s.Approve(ctx, tid, readyPoster(t, s, tid, omega.ID).ID, nil); err != nil {
		t.Fatalf("a supplement ad must not be held: %v", err)
	}

	// A lotion is not a medicine: approved without a reference.
	lotion := adBrief(t, s, tid, map[string]any{"name": "Nivea body lotion 400ml"})
	if _, err := s.Approve(ctx, tid, readyPoster(t, s, tid, lotion.ID).ID, nil); err != nil {
		t.Fatalf("a lotion ad must not be held: %v", err)
	}

	// Another business type's medicine ad is never held.
	other := pharmacyTenant(t, db, "generic")
	ob := adBrief(t, s, other, panadol)
	if _, err := s.Approve(ctx, other, readyPoster(t, s, other, ob.ID).ID, nil); err != nil {
		t.Fatalf("a generic tenant must not be held: %v", err)
	}
}

// A held video ad is not rendered (nothing is spent), and once the reference is entered the
// render puts it on the end card.
func TestPPBApprovalHoldsVideoAdRender(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := pharmacyTenant(t, db, BusinessTypePharmacy)
	b := adBrief(t, s, tid, map[string]any{"name": "Ozempic 1mg pen"})
	v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(1).SetKind(KindVideoAd).
		SetTier("standard").SetFormat("1080x1920").SetStatus(StatusScriptReady).SetMetadata(map[string]any{}).
		SetCopy(map[string]any{"script": map[string]any{"scenes": []any{}}, "end_card": map[string]any{"headline": "Order on WhatsApp", "subline": "Prescription only"}}).
		SaveX(ctx)

	if _, err := s.RenderVideoAd(ctx, tid, v.ID); !errors.Is(err, ErrPPBApprovalRequired) {
		t.Fatalf("render must be held, got %v", err)
	}
	if got := db.CreativeVersion.GetX(ctx, v.ID); got.Status != StatusScriptReady {
		t.Fatalf("a held render must not be queued, got %s", got.Status)
	}

	key, name := OfferingKey(b.Offering, b.Title)
	if _, err := s.SetPPBApproval(ctx, tid, key, name, "PPB/ADV/77", nil); err != nil {
		t.Fatal(err)
	}
	queued, err := s.RenderVideoAd(ctx, tid, v.ID)
	if err != nil {
		t.Fatalf("render with a reference: %v", err)
	}
	card, _ := queued.Copy["end_card"].(map[string]any)
	if queued.Status != StatusQueued || str(card["subline"]) != "Prescription only. PPB approval ref. PPB/ADV/77" || str(card["headline"]) != "Order on WhatsApp" {
		t.Fatalf("end card must carry the reference: %s %v", queued.Status, card)
	}
	if stamp, _ := queued.Metadata[MetaPPBApproval].(map[string]any); str(stamp["reference"]) != "PPB/ADV/77" {
		t.Fatalf("version must record the reference: %v", queued.Metadata)
	}
}

// Saving one product's reference keeps the others; an empty reference removes one; another
// tenant's list is untouched; the profile's other metadata stays.
func TestSetPPBApprovalKeepsOtherEntries(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := pharmacyTenant(t, db, BusinessTypePharmacy)
	other := pharmacyTenant(t, db, BusinessTypePharmacy)
	db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{"tagline": "keep me"}).ExecX(ctx)

	for key, ref := range map[string]string{"inventory:1": "PPB/1", "inventory:2": "PPB/2", "name:ors sachets": "PPB/3"} {
		if _, err := s.SetPPBApproval(ctx, tid, key, key, ref, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetPPBApproval(ctx, other, "inventory:1", "x", "PPB/OTHER", nil); err != nil {
		t.Fatal(err)
	}
	// Replace one, remove one.
	if _, err := s.SetPPBApproval(ctx, tid, "inventory:1", "Panadol", "PPB/1B", nil); err != nil {
		t.Fatal(err)
	}
	if a, err := s.SetPPBApproval(ctx, tid, "inventory:2", "", "", nil); err != nil || a != nil {
		t.Fatalf("remove: %v %v", a, err)
	}
	list, pharmacy, err := s.ListPPBApprovals(ctx, tid)
	if err != nil || !pharmacy {
		t.Fatalf("list: %v %v", pharmacy, err)
	}
	got := map[string]string{}
	for _, a := range list {
		got[a.OfferingKey] = a.Reference
	}
	if len(got) != 2 || got["inventory:1"] != "PPB/1B" || got["name:ors sachets"] != "PPB/3" {
		t.Fatalf("unexpected references: %v", got)
	}
	if str(db.Tenant.GetX(ctx, tid).Metadata["tagline"]) != "keep me" {
		t.Fatal("other metadata was lost")
	}
	if ol, _, _ := s.ListPPBApprovals(ctx, other); len(ol) != 1 || ol[0].Reference != "PPB/OTHER" {
		t.Fatalf("another tenant's list changed: %v", ol)
	}
	if _, err := s.SetPPBApproval(ctx, tid, "inventory:9", "x", "<bad>", nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad reference must be refused, got %v", err)
	}
	if _, err := s.SetPPBApproval(ctx, uuid.New(), "inventory:9", "x", "", nil); !ent.IsNotFound(err) {
		t.Fatalf("an unknown tenant is not found, got %v", err)
	}
}
