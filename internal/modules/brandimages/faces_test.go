package brandimages

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// A face is used only with the opt-in, a complete record, an adult, a date, and the tenant's own
// picture; ending it, its expiry, its picture going or the switch stops its use.
func TestFaces(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid, other := seedTenant(t, db), seedTenant(t, db)
	t.Cleanup(func() { _, _ = db.ServiceConfig.Delete().Where(serviceconfig.TenantID(tid)).Exec(ctx) })
	pics := []any{
		map[string]any{"id": "team", "key": Prefix(tid) + "2026/11/team.jpg", "width": 1200, "height": 1200},
		map[string]any{"id": "owner", "key": Prefix(tid) + "2026/11/owner.jpg", "width": 1200, "height": 1200},
		map[string]any{"id": "shot", "key": Prefix(tid) + "2026/11/shot.jpg", "kind": "screenshot"},
		map[string]any{"id": "foreign", "key": Prefix(other) + "2026/11/x.jpg"},
	}
	if err := metastore.SetKeys(ctx, db, tid, map[string]any{MetadataKey: pics}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	purged := map[string]int{}
	s := NewService(db, nil, DefaultConfig(), zap.NewNop()).WithFaceEnded(func(_ context.Context, tenantID uuid.UUID, id string) error {
		if tenantID != tid {
			t.Errorf("purge for another tenant %s", tenantID)
		}
		purged[id]++
		return nil
	})
	s.nowFn = func() time.Time { return now }
	good := FaceInput{ImageID: "owner", Name: "Jane, shop owner", Until: "2027-06-30", Under18: no(), Agreed: true, By: "admin@example.test"}

	if _, err := s.AddFace(ctx, tid, good); !errors.Is(err, ErrFacesOff) {
		t.Fatalf("recorded without the opt-in: %v", err)
	}
	if err := s.SetFacesOn(ctx, tid, true, "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	bad := map[string]struct {
		edit func(*FaceInput)
		want error
	}{
		"under 18":        {func(f *FaceInput) { f.Under18 = yes() }, ErrUnderage},
		"age not said":    {func(f *FaceInput) { f.Under18 = nil }, ErrFaceInvalid},
		"not agreed":      {func(f *FaceInput) { f.Agreed = false }, ErrFaceInvalid},
		"no name":         {func(f *FaceInput) { f.Name = " " }, ErrFaceInvalid},
		"no date":         {func(f *FaceInput) { f.Until = "" }, ErrFaceInvalid},
		"past date":       {func(f *FaceInput) { f.Until = "2026-11-01" }, ErrFaceInvalid},
		"too long":        {func(f *FaceInput) { f.Until = "2030-01-01" }, ErrFaceInvalid},
		"nobody recorded": {func(f *FaceInput) { f.By = "system" }, ErrFaceInvalid},
		"another use":     {func(f *FaceInput) { f.Use = "poster" }, ErrFaceInvalid},
		"a screenshot":    {func(f *FaceInput) { f.ImageID = "shot" }, ErrNotFound},
		"another tenant":  {func(f *FaceInput) { f.ImageID = "foreign" }, ErrNotFound},
	}
	for name, c := range bad {
		in := good
		c.edit(&in)
		if _, err := s.AddFace(ctx, tid, in); !errors.Is(err, c.want) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	face, err := s.AddFace(ctx, tid, good)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddFace(ctx, tid, good); !errors.Is(err, ErrFaceExists) {
		t.Fatalf("twice: %v", err)
	}
	live := func(at time.Time) []LiveFace {
		tn, err := db.Tenant.Get(ctx, tid)
		if err != nil {
			t.Fatal(err)
		}
		return LiveFaces(tn.Metadata, tid, FaceRulesFor(ctx, db, tid), at)
	}
	got := live(now)
	if len(got) != 1 || got[0].RecordedBy != "admin@example.test" || got[0].Statement != FaceStatementVersion || got[0].Key != Prefix(tid)+"2026/11/owner.jpg" {
		t.Fatalf("live %+v", got)
	}
	// It holds through its last day only, and never for another tenant.
	if len(live(time.Date(2027, 6, 30, 23, 0, 0, 0, time.UTC))) != 1 || len(live(time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC))) != 0 {
		t.Fatal("expiry")
	}
	tn, _ := db.Tenant.Get(ctx, tid)
	if len(LiveFaces(tn.Metadata, other, DefaultFaceRules(), now)) != 0 {
		t.Fatal("used for another tenant")
	}
	// The platform switch, and an unreadable setting, stop every face.
	row := db.ServiceConfig.Create().SetTenantID(tid).SetConfigKey(BrandFacesKey).SetConfigValue(`{"enabled":false}`).SaveX(ctx)
	if len(live(now)) != 0 {
		t.Fatal("used while the platform switch is off")
	}
	db.ServiceConfig.UpdateOneID(row.ID).SetConfigValue("{oops").ExecX(ctx)
	if len(live(now)) != 0 {
		t.Fatal("used under an unreadable setting")
	}
	db.ServiceConfig.DeleteOneID(row.ID).ExecX(ctx)

	// Ending it stops use at once and purges; the record stays.
	if err := s.EndFace(ctx, tid, face.ID, FaceRemoved, "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	tn, _ = db.Tenant.Get(ctx, tid)
	rec := ReadFaces(tn.Metadata)
	if len(live(now)) != 0 || purged[face.ID] != 1 || len(rec) != 1 || rec[0].EndedBy != "admin@example.test" || !FaceImages(tn.Metadata)["owner"] {
		t.Fatalf("ended %+v purged %v", rec, purged)
	}
	if err := s.EndFace(ctx, other, face.ID, FaceRemoved, "x"); !errors.Is(err, ErrFaceNotFound) {
		t.Fatalf("another tenant ended it: %v", err)
	}
	// An expired consent and one whose picture went are ended by the sweep; ended ones purge again.
	in := good
	in.ImageID, in.Until = "team", "2026-11-25"
	second, err := s.AddFace(ctx, tid, in)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	tn, _ = db.Tenant.Get(ctx, tid)
	if n, err := s.sweepTenantFaces(ctx, tn); err != nil || n != 1 || purged[second.ID] != 1 || purged[face.ID] != 2 {
		t.Fatalf("sweep %d %v %v", n, err, purged)
	}
	in = good
	third, err := s.AddFace(ctx, tid, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, tid, "owner"); err != nil {
		t.Fatal(err)
	}
	if len(live(now)) != 0 || purged[third.ID] != 1 {
		t.Fatalf("picture deleted: %v", purged)
	}
	// Turning the feature off ends what is live.
	if err := s.SetFacesOn(ctx, tid, false, "admin@example.test"); err != nil {
		t.Fatal(err)
	}
	tn, _ = db.Tenant.Get(ctx, tid)
	for _, f := range ReadFaces(tn.Metadata) {
		if f.EndedAt == "" {
			t.Fatalf("still open %+v", f)
		}
	}
	if FacesOn(tn.Metadata) || tn.Metadata["keep"] != "me" {
		t.Fatalf("metadata %v", tn.Metadata)
	}
}
