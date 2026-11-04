package inbox

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/schema"
	"github.com/Bengo-Hub/pagination"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/migrate"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"
)

const testDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func openDB(t *testing.T) *ent.Client {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	db := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Schema.Create(context.Background(), schema.WithDir(migrate.Dir)); err != nil {
		t.Fatal(err)
	}
	return db
}

func serve(h http.HandlerFunc, method, path, id string, tid uuid.UUID, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("X-Tenant-ID", tid.String())
	if id != "" {
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", id)
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestInboxListReplyHandledPromote(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()
	tid := uuid.New()
	db.Tenant.Create().SetID(tid).SetName("Inbox H " + tid.String()).SetSlug("inbox-h-" + tid.String()).ExecX(ctx)
	t.Cleanup(func() {
		_, _ = db.Activity.Delete().Where(activity.TenantID(tid)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(tid)).Exec(ctx)
	})
	now := time.Now().UTC().Truncate(time.Second)
	mk := func(kind, intent string, at time.Time) *ent.Activity {
		return db.Activity.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(uuid.New()).
			SetActivityType(kind).SetDirection("inbound").SetBody(kind + " " + intent).SetSource("facebook").
			SetExternalRef(uuid.NewString()).SetPerformedAt(at).
			SetMetadata(map[string]any{"platform": "facebook", "intent_status": "tagged", "intent": intent, "sender_name": "Amina"}).SaveX(ctx)
	}
	old := mk("dm", "buyer_price", now.Add(-30*time.Hour)) // outside the DM window
	mk("comment", "praise", now.Add(-2*time.Hour))
	newest := mk("comment", "buyer_shipping", now.Add(-time.Hour))
	db.Activity.Create().SetTenantID(tid).SetEntityType("contact").SetEntityID(uuid.New()).SetActivityType("comment").
		SetDirection("outbound").SetBody("our reply").SetPerformedAt(now).ExecX(ctx)

	h := NewHandler(db, nil, leads.NewService(db, zap.NewNop()), NewGraph("http://unused"), zap.NewNop())

	// Keyset pages of 2, newest first, outbound replies never listed.
	rec := serve(h.List, http.MethodGet, "/?limit=2", "", tid, nil)
	var page pagination.CursorResponse[Item]
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Data) != 2 || !page.HasMore || page.Data[0].ID != newest.ID.String() {
		t.Fatalf("page 1: %s", rec.Body.String())
	}
	rec = serve(h.List, http.MethodGet, "/?limit=2&cursor="+page.NextCursor, "", tid, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Data) != 1 || page.HasMore || page.Data[0].ID != old.ID.String() || page.Data[0].CanReply {
		t.Fatalf("page 2: %s", rec.Body.String())
	}
	// The buyer filter.
	rec = serve(h.List, http.MethodGet, "/?intent=buyer", "", tid, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Data) != 2 {
		t.Fatalf("buyer filter: %s", rec.Body.String())
	}

	// A reply is refused for a member without an approving role, and for a closed DM window.
	h.CanReply = func(context.Context, uuid.UUID) bool { return false }
	if rec := serve(h.Reply, http.MethodPost, "/", newest.ID.String(), tid, map[string]string{"text": "hi"}); rec.Code != http.StatusForbidden {
		t.Fatalf("role gate: %d", rec.Code)
	}
	h.CanReply = nil
	if rec := serve(h.Reply, http.MethodPost, "/", old.ID.String(), tid, map[string]string{"text": "hi"}); rec.Code != http.StatusConflict {
		t.Fatalf("closed window: %d", rec.Code)
	}

	// Handled drops the item from the open list; promote is idempotent.
	serve(h.Handled, http.MethodPost, "/", newest.ID.String(), tid, nil)
	rec = serve(h.List, http.MethodGet, "/?open=true", "", tid, nil)
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if len(page.Data) != 2 {
		t.Fatalf("open list: %s", rec.Body.String())
	}
	r1 := serve(h.Promote, http.MethodPost, "/", newest.ID.String(), tid, nil)
	r2 := serve(h.Promote, http.MethodPost, "/", newest.ID.String(), tid, nil)
	var l1, l2 map[string]string
	_ = json.Unmarshal(r1.Body.Bytes(), &l1)
	_ = json.Unmarshal(r2.Body.Bytes(), &l2)
	if r1.Code != http.StatusCreated || l1["lead_id"] == "" || l1["lead_id"] != l2["lead_id"] {
		t.Fatalf("promote: %d %v %v", r1.Code, l1, l2)
	}
	if n := db.Lead.Query().Where(lead.TenantID(tid)).CountX(ctx); n != 1 {
		t.Fatalf("one lead, got %d", n)
	}
}
