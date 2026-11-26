package adsetup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
)

func picture(id string, w, h int) Picture {
	return Picture{ID: id, URL: "https://img.example/" + id, Source: "Your brand pictures", Width: w, Height: h,
		load: func(context.Context) (image.Image, error) { return image.NewRGBA(image.Rect(0, 0, w, h)), nil }}
}

// Pictures are offered per shape, shown cut, sent only when ticked, in Google's shapes and in one
// call with the texts.
func TestMaterialPictures(t *testing.T) {
	f := &fake{reads: googleReads(), write: `{}`}
	f.reads["asset_group"] = `{"results":[{"assetGroup":{"id":"44","name":"Main"}}]}`
	f.reads["asset_group_asset"] = `{"results":[{"assetGroup":{"id":"44"},"assetGroupAsset":{"fieldType":"MARKETING_IMAGE"},"asset":{"name":"MarketFlow brand-a landscape"}}]}`
	in := googleIn(f.server(t))
	pics := []Picture{picture("brand-a", 1600, 1200), picture("brand-b", 1600, 1200), picture("small", 400, 400)}
	pics = append(pics, Picture{ID: "gone", URL: "https://img.example/gone", load: func(context.Context) (image.Image, error) { return nil, errors.New("gone") }})
	in.Profile = func(context.Context) Profile { return Profile{Name: "Digitika", Pictures: pics} }
	items := googleChecklist(context.Background(), in)
	it := find(t, items, "google.material:44")
	offered := map[string]*PictureChoice{}
	for _, x := range it.Fix.Fields {
		if x.Picture != nil {
			offered[x.Key] = x.Picture
			if x.Value != "" {
				t.Errorf("%s is ticked already", x.Key)
			}
		}
	}
	for key, want := range map[string]bool{"image|MARKETING_IMAGE|brand-a": false, "image|MARKETING_IMAGE|brand-b": true, "image|MARKETING_IMAGE|small": false,
		"image|SQUARE_MARKETING_IMAGE|small": true, "image|PORTRAIT_MARKETING_IMAGE|small": false, "image|PORTRAIT_MARKETING_IMAGE|brand-a": true} {
		if (offered[key] != nil) != want {
			t.Errorf("%s offered: %v", key, !want)
		}
	}
	run := func(v map[string]string) error {
		return runFix(context.Background(), items, fixRequest{Item: it.Key, Token: it.Fix.Token, Input: v})
	}
	for _, bad := range []map[string]string{
		{"image|MARKETING_IMAGE|brand-a": "yes"}, // there already
		{"image|MARKETING_IMAGE|other": "yes", "headline_1": "Digitika"},
		{"image|SQUARE_MARKETING_IMAGE|gone": "yes"},
		{"image|MARKETING_IMAGE|brand-b": ""},
	} {
		if run(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	if len(f.writes) != 0 {
		t.Fatal(f.writes)
	}
	if err := run(map[string]string{"headline_1": "Digitika", "image|MARKETING_IMAGE|brand-b": "yes", "image|PORTRAIT_MARKETING_IMAGE|brand-a": "yes"}); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 {
		t.Fatal(len(f.writes))
	}
	_, body, _ := strings.Cut(f.writes[0], " ")
	var sent struct {
		MutateOperations []struct {
			AssetOperation struct {
				Create struct {
					Name       string
					Type       string
					ImageAsset struct{ Data string }
				}
			}
			AssetGroupAssetOperation struct{ Create struct{ FieldType string } }
		}
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatal(err)
	}
	sizes, fields := map[string]image.Point{}, ""
	for _, o := range sent.MutateOperations {
		fields += o.AssetGroupAssetOperation.Create.FieldType + " "
		if c := o.AssetOperation.Create; c.Type == "IMAGE" {
			raw, _ := base64.StdEncoding.DecodeString(c.ImageAsset.Data)
			img, err := jpeg.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			sizes[c.Name] = img.Bounds().Size()
		}
	}
	if sizes["MarketFlow brand-b landscape"] != image.Pt(1200, 628) || sizes["MarketFlow brand-a tall"] != image.Pt(960, 1200) || len(sizes) != 2 ||
		!strings.Contains(fields, " MARKETING_IMAGE ") || !strings.Contains(fields, " PORTRAIT_MARKETING_IMAGE ") {
		t.Errorf("%v %s", sizes, fields)
	}
	many := map[string]string{}
	for _, k := range materialKinds {
		if k.Ratio > 0 {
			for i := range 4 {
				many[pictureKeyPrefix+k.Field+"|p"+string(rune('a'+i))] = "yes"
			}
		}
	}
	var four []Picture
	for i := range 4 {
		four = append(four, picture("p"+string(rune('a'+i)), 1600, 1600))
	}
	if _, err := pickedPictures(context.Background(), many, groupWith(nil), four); err == nil {
		t.Error("twelve pictures at once")
	}
	if _, err := pickedPictures(context.Background(), map[string]string{"image|MARKETING_IMAGE|pa": "yes"}, groupWith(map[string]int{"MARKETING_IMAGE": 20}), four); err == nil {
		t.Error("over Google's limit")
	}
}

func groupWith(count map[string]int) google.AssetGroup { return google.AssetGroup{Count: count} }

func openPicturesDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	dsn := os.Getenv("MF_TEST_POSTGRES_DSN")
	if dsn == "" {
		dsn = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil || db.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	c := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Only the tenant's own photos and the clean scenes of its approved ads: no logo, screenshot, poster,
// consent picture, unapproved ad, ad with a consented face, branded twin, writing or other tenant.
func TestPicturesOfTheTenant(t *testing.T) {
	db := openPicturesDB(t)
	ctx := context.Background()
	tid, other := uuid.New(), uuid.New()
	img := func(id string, f func(*brandimages.Image)) brandimages.Image {
		x := brandimages.Image{ID: id, Key: brandimages.Prefix(tid) + id + ".jpg", Width: 1600, Height: 1200}
		if f != nil {
			f(&x)
		}
		return x
	}
	meta := map[string]any{"brand_images": []brandimages.Image{
		img("shop", nil),
		img("logo", func(x *brandimages.Image) { x.Logo = true }),
		img("screen", func(x *brandimages.Image) { x.Kind = brandimages.KindScreenshot }),
		img("flyer", func(x *brandimages.Image) { x.Name = "June poster.jpg" }),
		img("person", nil),
		img("theirs", func(x *brandimages.Image) { x.Key = brandimages.Prefix(other) + "x.jpg" }),
	}, "brand_faces": []brandimages.Face{{ID: "f", ImageID: "person", EndedAt: time.Now().Format(time.RFC3339)}}}
	tn := db.Tenant.Create().SetID(tid).SetName("Pics").SetSlug("pics-" + tid.String()).SetMetadata(meta).SaveX(ctx)
	b := db.CreativeBrief.Create().SetTenantID(tid).SetTitle("Ad").SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantIDEQ(tid)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantIDEQ(tid)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(tid).Exec(ctx)
	})
	no := 0
	version := func(kind, status string, stills []any, face string, assets map[string]string) {
		no++
		sb := map[string]any{"stills": stills, "actor": map[string]any{"face_id": face}}
		v := db.CreativeVersion.Create().SetTenantID(tid).SetBriefID(b.ID).SetVersionNo(no).SetKind(kind).SetFormat("1080x1920").
			SetStatus(status).SetMetadata(map[string]any{"storyboard": sb}).SaveX(ctx)
		for key, k := range assets {
			db.CreativeAsset.Create().SetTenantID(tid).SetVersionID(v.ID).SetKind(k).SetStorageKey(key).SetURL("https://media.example/" + key).SaveX(ctx)
		}
	}
	still := func(key string, more map[string]any) map[string]any {
		m := map[string]any{"scene_id": key, "key": key, "source": "generated"}
		for k, v := range more {
			m[k] = v
		}
		return m
	}
	version("video_ad", "approved", []any{still("clean", nil), still("branded", map[string]any{"plain_key": "plain", "logo": map[string]any{"x": 1}}),
		still("screened", map[string]any{"screen": map[string]any{"id": "s"}}), still("signed", map[string]any{"cue": "OPEN"})}, "",
		map[string]string{"clean": "keyframe", "branded": "keyframe", "plain": "keyframe", "screened": "keyframe", "signed": "keyframe", "old-redraw": "keyframe", "card-preview": "keyframe"})
	version("video_ad", "approved", []any{still("faced", nil)}, "f", map[string]string{"faced": "keyframe"})
	version("video_ad", "ready", []any{still("draft", nil)}, "", map[string]string{"draft": "keyframe"})
	version("poster", "approved", nil, "", map[string]string{"poster": "image", "poster-scene": "scene"})

	h := &Handler{DB: db, Fetch: http.DefaultClient, Links: func(_ uuid.UUID, keys []string) []string {
		if !brandimages.Owned(keys[0], tid) {
			return nil
		}
		return []string{"https://media.example/" + keys[0]}
	}}
	got := map[string]bool{}
	for _, p := range h.pictures(ctx, tn) {
		got[p.URL[strings.LastIndex(p.URL, "/")+1:]] = true
	}
	want := []string{"shop.jpg", "clean", "plain", "poster-scene"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("%s is not offered", w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("offered %v", got)
	}
}
