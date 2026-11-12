package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// graphCall is one request the fake Graph server saw.
type graphCall struct {
	method, path string
	form         url.Values
}

// fakeGraph answers like the Graph API: {"success":true}, or Meta's error body when fail is set.
func fakeGraph(t *testing.T, fail string) (*Graph, *[]graphCall) {
	t.Helper()
	var calls []graphCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(b))
		for k, v := range r.URL.Query() {
			form[k] = v
		}
		calls = append(calls, graphCall{method: r.Method, path: r.URL.Path, form: form})
		if fail != "" {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": fail, "code": 100}})
			return
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	t.Cleanup(srv.Close)
	return NewGraph(srv.URL + "/v26.0"), &calls
}

func TestGraphHideFacebookComment(t *testing.T) {
	g, calls := fakeGraph(t, "")
	if err := g.HideComment(context.Background(), "facebook", "123_456", "page-token", true); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/v26.0/123_456" || c.form.Get("is_hidden") != "true" || c.form.Get("access_token") != "page-token" {
		t.Fatalf("call %+v", c)
	}
	if c.form.Has("hide") {
		t.Fatal("Facebook uses is_hidden, not hide")
	}
}

func TestGraphUnhideFacebookComment(t *testing.T) {
	g, calls := fakeGraph(t, "")
	if err := g.HideComment(context.Background(), "facebook", "123_456", "page-token", false); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.form.Get("is_hidden") != "false" {
		t.Fatalf("call %+v", c)
	}
}

func TestGraphHideInstagramComment(t *testing.T) {
	g, calls := fakeGraph(t, "")
	if err := g.HideComment(context.Background(), "instagram", "1789", "page-token", true); err != nil {
		t.Fatal(err)
	}
	c := (*calls)[0]
	if c.method != http.MethodPost || c.path != "/v26.0/1789" || c.form.Get("hide") != "true" || c.form.Has("is_hidden") {
		t.Fatalf("call %+v", c)
	}
}

func TestGraphDeleteFacebookComment(t *testing.T) {
	g, calls := fakeGraph(t, "")
	if err := g.DeleteComment(context.Background(), "facebook", "123_456", "page-token"); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.method != http.MethodDelete || c.path != "/v26.0/123_456" || c.form.Get("access_token") != "page-token" {
		t.Fatalf("call %+v", c)
	}
}

func TestGraphDeleteInstagramComment(t *testing.T) {
	g, calls := fakeGraph(t, "")
	if err := g.DeleteComment(context.Background(), "instagram", "1789", "page-token"); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[0]; c.method != http.MethodDelete || c.path != "/v26.0/1789" {
		t.Fatalf("call %+v", c)
	}
}

// Meta's own message comes back so the owner sees why (an expired token, a missing permission).
func TestGraphModerationReturnsMetaError(t *testing.T) {
	g, _ := fakeGraph(t, "(#200) Permission error")
	err := g.HideComment(context.Background(), "facebook", "1", "t", true)
	var ge *GraphError
	if !errors.As(err, &ge) || ge.Message != "(#200) Permission error" || ge.Status != http.StatusBadRequest {
		t.Fatalf("err %v", err)
	}
}

// Only Facebook Page and Instagram comments can be hidden or deleted: not DMs, not WhatsApp.
func TestModerationTarget(t *testing.T) {
	ref := func(s string) *string { return &s }
	cases := []struct {
		name string
		a    *ent.Activity
		want error
	}{
		{"facebook comment", &ent.Activity{ActivityType: "comment", ExternalRef: ref("1_2"), Metadata: map[string]any{"platform": "facebook"}}, nil},
		{"instagram comment", &ent.Activity{ActivityType: "comment", ExternalRef: ref("17"), Metadata: map[string]any{"platform": "instagram"}}, nil},
		{"facebook dm", &ent.Activity{ActivityType: "dm", ExternalRef: ref("m"), Metadata: map[string]any{"platform": "facebook"}}, ErrNotComment},
		{"instagram dm", &ent.Activity{ActivityType: "dm", ExternalRef: ref("m"), Metadata: map[string]any{"platform": "instagram"}}, ErrNotComment},
		{"whatsapp", &ent.Activity{ActivityType: "comment", ExternalRef: ref("w"), Metadata: map[string]any{"platform": "whatsapp"}}, ErrNotComment},
		{"no comment id", &ent.Activity{ActivityType: "comment", Metadata: map[string]any{"platform": "facebook"}}, ErrNotComment},
	}
	for _, c := range cases {
		_, _, err := moderationTarget(c.a)
		if !errors.Is(err, c.want) && !(err == nil && c.want == nil) {
			t.Errorf("%s: err %v, want %v", c.name, err, c.want)
		}
	}
}

// End to end against Postgres (skipped without it): hide, unhide and delete record the outcome on
// the item; a DM is refused with 409; a Meta error is a 502 carrying Meta's message; the handler's
// own role check refuses a caller without a moderating role.
func TestInboxHideUnhideDelete(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Inbox M " + tid.String()).SetSlug("inbox-m-" + tid.String()).ExecX(ctx)
	key, err := encryption.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keys := encryption.NewKeyProvider(nil, key)
	sealed, err := keys.Encrypt(ctx, "page-token")
	if err != nil {
		t.Fatal(err)
	}
	acc := db.ConnectedAccount.Create().SetTenantID(tid).SetPlatform("meta").SetAccountType("facebook_page").
		SetExternalID("PAGE-" + tid.String()[:8]).SetDisplayName("Shop").SetTokenCiphertext(sealed).SaveX(ctx)
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(tid)).Exec(ctx)
	})
	mk := func(kind, platform string) *ent.Activity {
		return db.Activity.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(uuid.New()).
			SetActivityType(kind).SetDirection("inbound").SetBody("hello").SetSource(platform).
			SetExternalRef("C-" + uuid.NewString()[:8]).SetPerformedAt(time.Now().UTC()).
			SetMetadata(map[string]any{"platform": platform, "connected_account_id": acc.ID.String()}).SaveX(ctx)
	}
	fb, ig, dm := mk("comment", "facebook"), mk("comment", "instagram"), mk("dm", "facebook")

	g, calls := fakeGraph(t, "")
	h := NewHandler(db, keys, nil, g, zap.NewNop())
	item := func(rec *httptest.ResponseRecorder) Item {
		var it Item
		_ = json.Unmarshal(rec.Body.Bytes(), &it)
		return it
	}

	if rec := serve(h.Hide, http.MethodPost, "/", fb.ID.String(), tid, map[string]bool{"hidden": true}); rec.Code != 200 || !item(rec).Hidden {
		t.Fatalf("hide fb: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Hide, http.MethodPost, "/", fb.ID.String(), tid, map[string]bool{"hidden": false}); rec.Code != 200 || item(rec).Hidden {
		t.Fatalf("unhide fb: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Hide, http.MethodPost, "/", ig.ID.String(), tid, map[string]bool{"hidden": true}); rec.Code != 200 || !item(rec).Hidden {
		t.Fatalf("hide ig: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Delete, http.MethodDelete, "/", fb.ID.String(), tid, nil); rec.Code != 200 || item(rec).DeletedAt == "" || item(rec).CanReply {
		t.Fatalf("delete fb: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Delete, http.MethodDelete, "/", ig.ID.String(), tid, nil); rec.Code != 200 {
		t.Fatalf("delete ig: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Delete, http.MethodDelete, "/", fb.ID.String(), tid, nil); rec.Code != http.StatusConflict {
		t.Fatalf("delete twice: %d", rec.Code)
	}
	want := []string{"POST /v26.0/" + *fb.ExternalRef, "POST /v26.0/" + *fb.ExternalRef, "POST /v26.0/" + *ig.ExternalRef,
		"DELETE /v26.0/" + *fb.ExternalRef, "DELETE /v26.0/" + *ig.ExternalRef}
	if len(*calls) != len(want) {
		t.Fatalf("graph calls %+v", *calls)
	}
	for i, c := range *calls {
		if c.method+" "+c.path != want[i] {
			t.Errorf("call %d: %s %s, want %s", i, c.method, c.path, want[i])
		}
	}
	got := db.Activity.GetX(ctx, fb.ID)
	if got.Metadata["deleted_at"] == nil || got.Metadata["hidden"] != false || got.Metadata["unhidden_at"] == nil {
		t.Fatalf("fb metadata %v", got.Metadata)
	}

	rec := serve(h.Hide, http.MethodPost, "/", dm.ID.String(), tid, map[string]bool{"hidden": true})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "only comments can be hidden or deleted") {
		t.Fatalf("dm hide: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(h.Delete, http.MethodDelete, "/", dm.ID.String(), tid, nil); rec.Code != http.StatusConflict {
		t.Fatalf("dm delete: %d", rec.Code)
	}
	if rec := serve(h.Hide, http.MethodPost, "/", ig.ID.String(), tid, map[string]string{}); rec.Code != http.StatusBadRequest {
		t.Fatalf("no hidden flag: %d", rec.Code)
	}

	other := mk("comment", "facebook")
	bad, _ := fakeGraph(t, "(#10) Permission denied")
	h.graph = bad
	rec = serve(h.Hide, http.MethodPost, "/", other.ID.String(), tid, map[string]bool{"hidden": true})
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "(#10) Permission denied") {
		t.Fatalf("meta error: %d %s", rec.Code, rec.Body.String())
	}

	h.CanModerate = func(context.Context, uuid.UUID) bool { return false }
	if rec := serve(h.Delete, http.MethodDelete, "/", other.ID.String(), tid, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("role check: %d", rec.Code)
	}
}
