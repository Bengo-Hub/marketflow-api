package brandimages

import (
	"context"
	"errors"
	"testing"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func TestKindCheck(t *testing.T) {
	for _, c := range []struct {
		u    KindUpdate
		want error
	}{
		{KindUpdate{Kind: "screenshot", NoCustomerData: true, Device: "Laptop"}, nil},
		{KindUpdate{Kind: "photo"}, nil},
		{KindUpdate{Kind: "screenshot"}, ErrCustomerData},
		{KindUpdate{Kind: "face"}, ErrInvalidKind},
		{KindUpdate{Kind: "screenshot", NoCustomerData: true, Device: "fridge"}, ErrInvalidKind},
	} {
		if err := c.u.check(); !errors.Is(err, c.want) && err != c.want {
			t.Fatalf("%+v: %v", c.u, err)
		}
	}
}

// Screenshots have their own limits, are never brand photos, need the no-customer-data statement,
// and belong to their tenant.
func TestScreenshots(t *testing.T) {
	db := openDB(t)
	tid, other := seedTenant(t, db), seedTenant(t, db)
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.Max, cfg.MinShortSide, cfg.MaxScreens, cfg.MinScreenSide = 2, 64, 1, 32
	s := NewService(db, store, cfg, zap.NewNop())
	ctx := context.Background()
	up := func(w, h int) *objectstore.Upload {
		u, err := objectstore.Prepare(jpegOf(t, w, h), MediaType)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	first, err := s.Add(ctx, tid, up(80, 80))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAs(ctx, tid, up(120, 70), KindUpdate{Kind: KindScreenshot}); !errors.Is(err, ErrCustomerData) {
		t.Fatalf("unconfirmed screenshot: %v", err)
	}
	shot, err := s.AddAs(ctx, tid, up(120, 40), KindUpdate{Kind: KindScreenshot, NoCustomerData: true, Device: "laptop", By: "owner@example.test"})
	if err != nil || shot.Kind != KindScreenshot || shot.Device != "laptop" {
		t.Fatalf("screenshot %+v %v", shot, err)
	}
	// The screenshot limit is one; the photo limit is not touched by it.
	if _, err := s.SetKind(ctx, tid, first.ID, KindUpdate{Kind: KindScreenshot, NoCustomerData: true}); !errors.Is(err, ErrFull) {
		t.Fatalf("second screenshot: %v", err)
	}
	if _, err := s.Add(ctx, tid, up(90, 90)); err != nil {
		t.Fatalf("second photo: %v", err)
	}
	if _, err := s.Add(ctx, tid, up(91, 91)); !errors.Is(err, ErrFull) {
		t.Fatalf("third photo: %v", err)
	}
	// A picture too small for a photo stays a screenshot; another tenant cannot touch it.
	if _, err := s.SetKind(ctx, tid, shot.ID, KindUpdate{Kind: "photo"}); !errors.Is(err, ErrTooSmall) {
		t.Fatalf("small photo: %v", err)
	}
	if _, err := s.SetKind(ctx, other, shot.ID, KindUpdate{Kind: KindScreenshot, NoCustomerData: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another tenant: %v", err)
	}
	tn, err := db.Tenant.Get(ctx, tid)
	if err != nil {
		t.Fatal(err)
	}
	shots := OfKind(tn.Metadata, tid, KindScreenshot)
	if len(Keys(tn.Metadata, tid, 0)) != 2 || len(shots) != 1 || shots[0].NoCustomerDataBy != "owner@example.test" || !NeverFrame(shots[0]) {
		t.Fatalf("stored %+v", Read(tn.Metadata))
	}
	if len(OfKind(tn.Metadata, uuid.New(), KindScreenshot)) != 0 {
		t.Fatal("a picture outside the tenant's prefix was returned")
	}
	// Removing the screenshot makes room, and a photo can then become one, in place.
	if err := s.Delete(ctx, tid, shot.ID); err != nil {
		t.Fatal(err)
	}
	if v, err := s.SetKind(ctx, tid, first.ID, KindUpdate{Kind: KindScreenshot, NoCustomerData: true, Device: "pos"}); err != nil || v.Device != "pos" {
		t.Fatalf("marked %+v %v", v, err)
	}
	if v, err := s.SetKind(ctx, tid, first.ID, KindUpdate{Kind: "photo"}); err != nil || v.Kind != KindPhoto || v.Device != "" {
		t.Fatalf("unmarked %+v %v", v, err)
	}
	tn, _ = db.Tenant.Get(ctx, tid)
	if tn.Metadata["keep"] != "me" || Read(tn.Metadata)[0].ID != first.ID {
		t.Fatalf("metadata %v", tn.Metadata)
	}
}
