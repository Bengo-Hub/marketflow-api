package creative

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
)

// tenantRow adds a tenant row with metadata (the creative tables need none, but the brand block,
// brand images and actor names come from it) and removes it afterwards.
func tenantRow(t *testing.T, db *ent.Client, meta map[string]any) uuid.UUID {
	t.Helper()
	tid := newTenant(t, db)
	ctx := context.Background()
	if _, err := db.Tenant.Create().SetID(tid).SetName("CR " + tid.String()).SetSlug("cr-" + tid.String()).
		SetBusinessType("generic").SetMetadata(meta).Save(ctx); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	return tid
}

// A brief gets its brand block (accent, logo, palette) from the tenant's profile, which the
// poster, motion and video ad renders read; one sent with the brief is kept.
func TestBriefBrandFromProfile(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := tenantRow(t, db, map[string]any{"brand_primary_color": "#1f7a4d", "logo_url": "https://cdn.example.com/logo.png"})
	b, err := s.CreateBrief(ctx, tid, nil, BriefInput{Title: ptr("Shea butter")})
	if err != nil {
		t.Fatal(err)
	}
	brand, _ := b.Metadata["brand"].(map[string]any)
	if brand["accent"] != "#1f7a4d" || brand["logo_url"] != "https://cdn.example.com/logo.png" || len(strs(brand["palette"])) != 1 {
		t.Fatalf("brand = %v", b.Metadata)
	}
	own := map[string]any{"brand": map[string]any{"accent": "#000000"}}
	b, _ = s.CreateBrief(ctx, tid, nil, BriefInput{Title: ptr("Own"), Metadata: own})
	if brand, _ := b.Metadata["brand"].(map[string]any); brand["accent"] != "#000000" {
		t.Fatalf("a sent brand block is kept: %v", b.Metadata)
	}
	odd := tenantRow(t, db, map[string]any{"brand_primary_color": "red", "logo_url": "/media/logo.png"})
	b, _ = s.CreateBrief(ctx, odd, nil, BriefInput{Title: ptr("Odd")})
	if _, ok := b.Metadata["brand"]; ok {
		t.Fatalf("no usable colour or link, no brand block: %v", b.Metadata)
	}
}

const soundScriptReply = `{"script":{"format":"product in motion","hooks":["Dry skin?"],"mood":"warm, light acoustic",
 "scenes":[
 {"id":"s1","duration_s":5,"shot":"product_clip","asset":1,"visual":"jar","voiceover":"Dry skin in the morning? Try this.","caption":"Dry skin?","sfx":"soft whoosh"},
 {"id":"s2","duration_s":5,"shot":"b_roll","asset":3,"visual":"the shop","voiceover":"Made in our Kisumu shop.","caption":"Kisumu"},
 {"id":"s3","duration_s":5,"shot":"end_card","visual":"","voiceover":"Order today.","caption":"Order today"}],"cta":"Order"},
 "findings":[],"credits_charged":1,"cost_usd":0.002}`

// Brand images join a video ad's photos after the offering's (signed only when a job is sent),
// the actor naming choice goes to the script writer, and the script's sound cues and mood
// reach the render.
func TestVideoAdBrandImagesActorNamesAndSound(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	img := func(owner uuid.UUID, n string) map[string]any {
		return map[string]any{"id": n, "key": brandimages.Prefix(owner) + "2026/11/" + n + ".jpg"}
	}
	tid := tenantRow(t, db, map[string]any{})
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{
		brandimages.MetadataKey: []any{img(tid, "a"), img(uuid.New(), "foreign"), img(tid, "b")},
		marketingos.MetadataKey: map[string]any{"positioning": "Shea butter", "actor_name_style": "luo",
			"actor_names": []any{"Nyar Kabuoch", "Achieng Yar Migori"}},
	}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	b := newBrief(t, s, tid)
	v, err := s.CreateVideoAd(ctx, tid, b.ID, VideoAdInput{})
	if err != nil {
		t.Fatal(err)
	}
	if keys := strs(v.Metadata[metaBrandImageKeys]); len(keys) != 2 || adPhotoCount(v.Metadata) != 3 {
		t.Fatalf("brand keys = %v (another tenant's key must be dropped)", v.Metadata[metaBrandImageKeys])
	}

	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	ai := &fakeJobs{jobs: map[string]string{}, scriptBody: soundScriptReply}
	srv := ai.server(t, "svc-key")
	r := NewRenderer(db, srv.URL, "svc-key", zap.NewNop()).WithBrandImages(brandimages.NewLinker(store, time.Hour))
	r.Tick(ctx)
	if len(ai.scripts) != 1 {
		t.Fatalf("script calls: %d", len(ai.scripts))
	}
	sc := ai.scripts[0]
	names, _ := sc["actor_names"].([]any)
	if sc["photos"] != float64(3) || sc["brand_photos"] != float64(2) || sc["actor_name_style"] != "luo" ||
		len(names) != 2 || names[0] != "Nyar Kabuoch" {
		t.Fatalf("script call: %v", sc)
	}
	got, _ := s.GetVersion(ctx, tid, v.ID)
	if got.Status != StatusScriptReady {
		t.Fatalf("after script: %s %q", got.Status, got.Error)
	}

	// The owner's edit may use a brand photo (asset 3) and keeps the cue and the mood; a cue or
	// mood over the limit is refused.
	edit := &VideoScript{Mood: " warm, light acoustic ", Scenes: []ScriptScene{
		{Shot: "product_clip", Asset: 3, Voiceover: "Made in Kisumu.", SFX: " soft whoosh "},
		{Shot: "end_card", Voiceover: "Order on WhatsApp."}}}
	if _, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: edit}); err != nil {
		t.Fatalf("brand photo asset: %v", err)
	}
	for name, bad := range map[string]*VideoScript{
		"long cue":     {Scenes: []ScriptScene{{Shot: "b_roll", Asset: 1, Voiceover: "Hi", SFX: strings.Repeat("x", maxSceneSFX+1)}, {Shot: "end_card", Voiceover: "Bye"}}},
		"long mood":    {Mood: strings.Repeat("m", maxScriptMood+1), Scenes: []ScriptScene{{Shot: "b_roll", Asset: 1, Voiceover: "Hi"}, {Shot: "end_card", Voiceover: "Bye"}}},
		"photo 4 of 3": {Scenes: []ScriptScene{{Shot: "b_roll", Asset: 4, Voiceover: "Hi"}, {Shot: "end_card", Voiceover: "Bye"}}},
	} {
		if _, err := s.UpdateScript(ctx, tid, v.ID, ScriptInput{Script: bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}

	if _, err := s.RenderVideoAd(ctx, tid, v.ID); err != nil {
		t.Fatal(err)
	}
	r.Tick(ctx)
	if len(ai.started) != 1 {
		t.Fatalf("ad starts: %v", ai.paths)
	}
	ad := ai.started[0]
	photos := strs(ad["photo_urls"])
	if len(photos) != 3 || photos[0] != "https://example.com/shea.jpg" ||
		!strings.Contains(photos[1], brandimages.Prefix(tid)+"2026/11/a.jpg") || !strings.Contains(photos[2], "/b.jpg") {
		t.Fatalf("ad photos: %v", photos)
	}
	scenes, _ := ad["scenes"].([]any)
	first, _ := scenes[0].(map[string]any)
	if ad["mood"] != "warm, light acoustic" || first["sfx"] != "soft whoosh" {
		t.Fatalf("mood %v, first scene %v", ad["mood"], first)
	}
}

// A poster tops the offering's photos up with brand images, up to the poster cap.
func TestPosterAddsBrandImages(t *testing.T) {
	db := openDB(t)
	s := NewService(db, zap.NewNop())
	ctx := context.Background()
	tid := tenantRow(t, db, map[string]any{})
	list := []any{}
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		list = append(list, map[string]any{"id": n, "key": brandimages.Prefix(tid) + "2026/11/" + n + ".jpg"})
	}
	if _, err := db.Tenant.UpdateOneID(tid).SetMetadata(map[string]any{brandimages.MetadataKey: list}).Save(ctx); err != nil {
		t.Fatal(err)
	}
	b := newBrief(t, s, tid)
	if _, err := s.CreateVersion(ctx, tid, b.ID, VersionInput{Prompt: "jar on linen"}); err != nil {
		t.Fatal(err)
	}
	store, err := objectstore.New("marketflow", objectstore.Config{LocalRoot: t.TempDir(), LocalURLBase: "http://localhost/media"})
	if err != nil {
		t.Fatal(err)
	}
	ai := &fakeAI{}
	srv := ai.server(t, "svc-key")
	NewRenderer(db, srv.URL, "svc-key", zap.NewNop()).WithBrandImages(brandimages.NewLinker(store, time.Hour)).Tick(ctx)
	if len(ai.got) != 1 {
		t.Fatalf("ai calls: %d", len(ai.got))
	}
	photos := strs(ai.got[0]["photo_urls"])
	if len(photos) != maxPhotos || photos[0] != "https://example.com/shea.jpg" || !strings.Contains(photos[1], "/a.jpg") {
		t.Fatalf("poster photos: %v", photos)
	}
}
