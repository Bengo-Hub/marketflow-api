package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/auditlog"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

type fakeDeleter struct {
	platform string
	err      error
	calls    int
}

func (f *fakeDeleter) Platform() string { return f.platform }

func (f *fakeDeleter) Delete(_ context.Context, a Account, id string) error {
	f.calls++
	if a.Token != "tok" || id == "" {
		return errors.New("bad call")
	}
	return f.err
}

// fakeTokens hands back an account with the scopes the test sets; err plays a disconnected one.
type fakeTokens struct {
	scopes []string
	err    error
}

func (f fakeTokens) Account(context.Context, string, string) (Account, error) {
	if f.err != nil {
		return Account{}, f.err
	}
	return Account{Token: "tok", Scopes: f.scopes}, nil
}

var youtubeDeleteScopes = []string{"https://www.googleapis.com/auth/youtube.upload", "https://www.googleapis.com/auth/youtube.force-ssl"}

func deleteRequest(h *Handler, tenant uuid.UUID, id uuid.UUID) (int, map[string]any) {
	r := chi.NewRouter()
	r.Post("/jobs/{id}/delete-from-platform", h.DeleteFromPlatform)
	req := httptest.NewRequest(http.MethodPost, "/jobs/"+id.String()+"/delete-from-platform", nil)
	req.Header.Set("X-Tenant-ID", tenant.String())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var body map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func publishedJob(t *testing.T, db *ent.Client, platform, externalID string) *ent.PublishJob {
	t.Helper()
	row := seedJob(t, db, StatusPublished, time.Now().Add(-time.Hour).Truncate(time.Second))
	return db.PublishJob.UpdateOneID(row.ID).SetPlatform(platform).SetExternalID(externalID).
		SetPermalink("https://www.youtube.com/shorts/" + externalID).SaveX(context.Background())
}

func TestDeleteFromPlatform(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	yt := &fakeDeleter{platform: YouTube}
	allow := true
	h := &Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop(),
		CanDelete: func(context.Context, uuid.UUID) bool { return allow },
		Deleters:  map[string]Deleter{YouTube: yt},
		Tokens:    fakeTokens{scopes: youtubeDeleteScopes}}

	row := publishedJob(t, db, YouTube, "v1")

	// Roles: a member who may post but not delete is refused before anything is called.
	allow = false
	if code, _ := deleteRequest(h, row.TenantID, row.ID); code != http.StatusForbidden || yt.calls != 0 {
		t.Fatalf("forbidden: %d, calls %d", code, yt.calls)
	}
	allow = true

	// Tenant scope: another tenant's id is not found.
	if code, _ := deleteRequest(h, uuid.New(), row.ID); code != http.StatusNotFound {
		t.Fatalf("other tenant: %d", code)
	}

	// The delete: the platform is called once, the job records it, the audit log has it.
	code, body := deleteRequest(h, row.TenantID, row.ID)
	if code != http.StatusOK || body["status"] != StatusDeletedOnPlatform || body["deleted_at"] == nil || yt.calls != 1 {
		t.Fatalf("delete: %d %v calls %d", code, body, yt.calls)
	}
	n := db.AuditLog.Query().Where(auditlog.TenantIDEQ(row.TenantID), auditlog.EventEQ(AuditDeletedOnPlatform)).CountX(ctx)
	if n != 1 {
		t.Fatalf("audit rows %d", n)
	}

	// Repost allowed (owner 2026-11-14): the delete freed the asset:account dedupe key, so the
	// same video can be scheduled to the same account again as a new post.
	freed := db.PublishJob.GetX(ctx, row.ID)
	if freed.DedupeKey != row.DedupeKey+":deleted:"+row.ID.String() {
		t.Fatalf("dedupe key not freed: %q", freed.DedupeKey)
	}
	again, err := db.PublishJob.Create().SetTenantID(row.TenantID).SetAssetID(row.AssetID).
		SetConnectedAccountID(row.ConnectedAccountID).SetPlatform(YouTube).SetScheduledAt(time.Now().Add(time.Hour)).
		SetDedupeKey(row.DedupeKey).Save(ctx)
	if err != nil {
		t.Fatalf("a repost after the delete must create a new job: %v", err)
	}
	if again.ID == row.ID {
		t.Fatal("the repost is a new job")
	}

	// Idempotent: a second press answers the same job and calls nothing.
	code, body = deleteRequest(h, row.TenantID, row.ID)
	if code != http.StatusOK || body["status"] != StatusDeletedOnPlatform || yt.calls != 1 {
		t.Fatalf("repeat: %d %v calls %d", code, body, yt.calls)
	}
	if n := db.AuditLog.Query().Where(auditlog.TenantIDEQ(row.TenantID), auditlog.EventEQ(AuditDeletedOnPlatform)).CountX(ctx); n != 1 {
		t.Fatalf("a repeat writes no second audit row: %d", n)
	}

	// Only a published post.
	pending := seedJob(t, db, StatusPending, time.Now().Add(time.Hour))
	if code, _ := deleteRequest(h, pending.TenantID, pending.ID); code != http.StatusConflict {
		t.Fatalf("pending: %d", code)
	}
}

func TestDeleteFromPlatformRefusals(t *testing.T) {
	db := openTestDB(t)
	yt := &fakeDeleter{platform: YouTube}
	h := &Handler{Store: &EntStore{DB: db}, Logger: zap.NewNop(),
		Deleters: map[string]Deleter{YouTube: yt, TikTok: NewTikTokDirect("http://127.0.0.1:1", "", false)},
		Tokens:   fakeTokens{scopes: youtubeDeleteScopes}}

	unsupportedWith := func(t *testing.T, body map[string]any) {
		t.Helper()
		d, _ := body["details"].(map[string]any)
		steps, _ := d["steps"].([]any)
		if body["code"] != "delete_unsupported" || len(steps) == 0 {
			t.Fatalf("want delete_unsupported with the manual steps: %v", body)
		}
	}

	// TikTok has no delete: 409 with TikTok's own steps, the job stays published.
	tt := publishedJob(t, db, TikTok, "7300000000000000000")
	code, body := deleteRequest(h, tt.TenantID, tt.ID)
	if code != http.StatusConflict {
		t.Fatalf("tiktok: %d %v", code, body)
	}
	unsupportedWith(t, body)
	if db.PublishJob.GetX(context.Background(), tt.ID).Status != StatusPublished {
		t.Fatal("a refused delete leaves the job published")
	}

	// youtube.upload alone cannot delete.
	h.Tokens = fakeTokens{scopes: []string{"https://www.googleapis.com/auth/youtube.upload"}}
	ytRow := publishedJob(t, db, YouTube, "v2")
	code, body = deleteRequest(h, ytRow.TenantID, ytRow.ID)
	if code != http.StatusConflict || yt.calls != 0 {
		t.Fatalf("missing scope: %d calls %d", code, yt.calls)
	}
	unsupportedWith(t, body)

	// A disconnected account.
	h.Tokens = fakeTokens{err: errors.New("not found")}
	if code, body = deleteRequest(h, ytRow.TenantID, ytRow.ID); code != http.StatusConflict {
		t.Fatalf("disconnected: %d", code)
	}
	unsupportedWith(t, body)

	// A platform error comes back with the platform's message; the job stays published.
	h.Tokens = fakeTokens{scopes: youtubeDeleteScopes}
	yt.err = errors.New("youtube delete: 403 forbidden: The video that you are trying to delete cannot be deleted.")
	code, body = deleteRequest(h, ytRow.TenantID, ytRow.ID)
	if code != http.StatusBadGateway || body["code"] != "platform_error" || !strings.Contains(body["error"].(string), "cannot be deleted") {
		t.Fatalf("platform error: %d %v", code, body)
	}
	if db.PublishJob.GetX(context.Background(), ytRow.ID).Status != StatusPublished {
		t.Fatal("a failed delete leaves the job published")
	}

	// Already gone on the platform: recorded as deleted.
	yt.err = ErrAlreadyGone
	if code, body = deleteRequest(h, ytRow.TenantID, ytRow.ID); code != http.StatusOK || body["status"] != StatusDeletedOnPlatform {
		t.Fatalf("already gone: %d %v", code, body)
	}
}

// The calendar says which published posts can be deleted, and how to delete the others.
func TestViewsCanDeleteOnPlatform(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := &EntStore{DB: db}
	row := publishedJob(t, db, Facebook, "vid9")
	acc := db.ConnectedAccount.Create().SetTenantID(row.TenantID).SetPlatform("meta").SetAccountType("facebook_page").
		SetExternalID("p_" + row.ID.String()[:8]).SetDisplayName("Shea Co").SetStatus("active").
		SetScopes([]string{"pages_show_list", "pages_read_engagement", "pages_manage_posts"}).SaveX(ctx)
	t.Cleanup(func() { _ = db.ConnectedAccount.DeleteOneID(acc.ID).Exec(ctx) })
	row = db.PublishJob.UpdateOneID(row.ID).SetConnectedAccountID(acc.ID).SaveX(ctx)

	tt := publishedJob(t, db, TikTok, "7300000000000000000")
	pending := seedJob(t, db, StatusPending, time.Now().Add(time.Hour))

	v := s.Views(ctx, row.TenantID, []*ent.PublishJob{row})[0]
	if !v.CanDeleteOnPlatform || len(v.DeleteSteps) != 0 {
		t.Fatalf("facebook with pages_manage_posts: %+v", v)
	}
	v = s.Views(ctx, tt.TenantID, []*ent.PublishJob{tt})[0]
	if v.CanDeleteOnPlatform || len(v.DeleteSteps) == 0 || v.DeleteReason == "" {
		t.Fatalf("tiktok shows the manual steps: %+v", v)
	}
	v = s.Views(ctx, pending.TenantID, []*ent.PublishJob{pending})[0]
	if v.CanDeleteOnPlatform || len(v.DeleteSteps) != 0 {
		t.Fatalf("only a published post carries delete info: %+v", v)
	}
}
