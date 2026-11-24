package testimonials

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
)

const testPostgresDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

func testDB(t *testing.T) (*ent.Client, uuid.UUID) {
	t.Helper()
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", testPostgresDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unreachable")
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, sqlDB)))
	ctx := context.Background()
	tid := uuid.New()
	if _, err := client.Tenant.Create().SetID(tid).SetName("Testimonials Test").SetSlug("tt-" + tid.String()[:8]).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Testimonial.Delete().Where(testimonial.TenantID(tid)).Exec(ctx)
		_ = client.Tenant.DeleteOneID(tid).Exec(ctx)
		_ = client.Close()
	})
	return client, tid
}

func events(t *testing.T, c *ent.Client, tid uuid.UUID) map[string]int {
	rows, err := c.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).All(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, r := range rows {
		out[r.EventType]++
	}
	return out
}

// A form testimonial is stored pending and unpublished with its submitted event; the same words
// from the same person again, or a website quote of the same words, are the same testimonial.
func TestSubmitDedupesAndWaitsForReview(t *testing.T) {
	c, tid := testDB(t)
	ctx := context.Background()
	first, err := Submit(ctx, c, SubmitInput{TenantID: tid, Source: SourceForm, AuthorName: "Jane Wambui", Body: "Great food, fast delivery!", Rating: 5,
		Metadata: map[string]any{"consent": true}})
	if err != nil || first.Status != StatusPending || first.IsPublished || first.Source != SourceForm {
		t.Fatalf("first: %+v %v", first, err)
	}
	if _, err := Submit(ctx, c, SubmitInput{TenantID: tid, Source: SourceForm, AuthorName: "jane wambui", Body: "great food fast delivery", Rating: 4}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same person, same words: %v", err)
	}
	if dup, err := Submit(ctx, c, SubmitInput{TenantID: tid, Source: SourceWebsite, AuthorName: "J. Wambui", Body: "Great food, fast delivery!"}); !errors.Is(err, ErrDuplicate) || dup.ID != first.ID {
		t.Fatalf("website copy of the same words: %v", err)
	}
	if n, _ := c.Testimonial.Query().Where(testimonial.TenantID(tid)).Count(ctx); n != 1 {
		t.Fatalf("testimonials = %d, want 1", n)
	}
	if ev := events(t, c, tid); ev[TestimonialSubmittedEvent] != 1 || ev[TestimonialPublishedEvent] != 0 {
		t.Fatalf("events %v", ev)
	}
}

// Approving publishes it once (published event for the testimonial ad); rejecting hides it; the
// public list shows only approved, published rows.
func TestReviewPublishesOnce(t *testing.T) {
	c, tid := testDB(t)
	ctx := context.Background()
	s, err := Submit(ctx, c, SubmitInput{TenantID: tid, Source: SourceForm, AuthorName: "Peter", Body: "Soft chapati every single time.", Rating: 5})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(c, zap.NewNop())
	if rows, _ := h.PublicListForTenant(ctx, tid, 10); len(rows) != 0 {
		t.Fatal("a pending testimonial is never public")
	}
	for i := 0; i < 2; i++ {
		got, err := Review(ctx, c, tid, s.ID, true, true, "admin@example.com")
		if err != nil || got.Status != StatusApproved || !got.IsPublished || got.Metadata["reviewed_by"] != "admin@example.com" {
			t.Fatalf("approve %d: %+v %v", i, got, err)
		}
	}
	if ev := events(t, c, tid); ev[TestimonialPublishedEvent] != 1 {
		t.Fatalf("published events %v, want 1", ev)
	}
	rows, _ := h.PublicListForTenant(ctx, tid, 10)
	if len(rows) != 1 || rows[0]["body"] != "Soft chapati every single time." {
		t.Fatalf("public list: %v", rows)
	}
	if _, ok := rows[0]["submitter_email"]; ok {
		t.Fatal("contact details are never public")
	}
	if got, err := Review(ctx, c, tid, s.ID, false, false, ""); err != nil || got.IsPublished || got.Status != StatusRejected {
		t.Fatalf("reject: %+v %v", got, err)
	}
	if rows, _ := h.PublicListForTenant(ctx, tid, 10); len(rows) != 0 {
		t.Fatal("a rejected testimonial is hidden")
	}
	if _, err := Review(ctx, c, uuid.New(), s.ID, true, true, ""); !ent.IsNotFound(err) {
		t.Fatalf("another tenant cannot review it: %v", err)
	}
}

// Website import stores new quotes pending and skips ones it already has.
func TestImportFromWebsiteDedupes(t *testing.T) {
	c, tid := testDB(t)
	ctx := context.Background()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(sitePage))
	}))
	defer srv.Close()
	if _, err := Submit(ctx, c, SubmitInput{TenantID: tid, Source: SourceForm, AuthorName: "Jane Wambui",
		Body: "Catering for our office party was spotless, everyone asked who cooked.", Rating: 5}); err != nil {
		t.Fatal(err)
	}
	res, err := ImportFromWebsite(ctx, c, srv.Client(), tid, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The page and its /testimonials link serve the same 4 quotes: 3 new, the rest already held.
	if res.Stored != 3 || res.Found != 8 || res.Already != 5 {
		t.Fatalf("import result %+v", res)
	}
	again, err := ImportFromWebsite(ctx, c, srv.Client(), tid, srv.URL)
	if err != nil || again.Stored != 0 {
		t.Fatalf("second import %+v %v", again, err)
	}
	brian, err := c.Testimonial.Query().Where(testimonial.TenantID(tid), testimonial.AuthorName("Brian K")).Only(ctx)
	if err != nil || brian.Status != StatusPending || brian.Source != SourceWebsite || ratingOf(brian) != 0 || brian.Metadata["source_url"] == nil {
		t.Fatalf("website testimonial: %+v %v", brian, err)
	}
}

func postForm(t *testing.T, h *Handler, slug string, fields map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/public/testimonials/"+slug, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rc := chi.NewRouteContext()
	rc.URLParams.Add("slug", slug)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rc))
	rec := httptest.NewRecorder()
	h.PublicSubmit(rec, req)
	return rec
}

// The public form: consent and rating are required, a bot (honeypot or instant fill) is answered
// as if accepted but nothing is stored, and a person's testimonial waits for review.
func TestPublicSubmit(t *testing.T) {
	c, tid := testDB(t)
	ctx := context.Background()
	ten, _ := c.Tenant.Get(ctx, tid)
	h := NewHandler(c, zap.NewNop())
	ok := map[string]string{"name": "Amina", "text": "Lovely cakes for my daughter's birthday.", "rating": "5", "consent": "true",
		"email": "amina@example.com", "started_at": strconv.FormatInt(time.Now().Add(-time.Minute).UnixMilli(), 10)}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range ok {
			m[a] = b
		}
		m[k] = v
		return m
	}
	if rec := postForm(t, h, ten.Slug, with("consent", "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("no consent: %d", rec.Code)
	}
	if rec := postForm(t, h, ten.Slug, with("website", "http://spam")); rec.Code != http.StatusCreated {
		t.Fatalf("honeypot answer: %d", rec.Code)
	}
	if rec := postForm(t, h, ten.Slug, with("started_at", strconv.FormatInt(time.Now().UnixMilli(), 10))); rec.Code != http.StatusCreated {
		t.Fatalf("instant fill answer: %d", rec.Code)
	}
	if n, _ := c.Testimonial.Query().Where(testimonial.TenantID(tid)).Count(ctx); n != 0 {
		t.Fatalf("bots stored %d", n)
	}
	if rec := postForm(t, h, ten.Slug, ok); rec.Code != http.StatusCreated {
		t.Fatalf("person: %d %s", rec.Code, rec.Body.String())
	}
	got, err := c.Testimonial.Query().Where(testimonial.TenantID(tid)).Only(ctx)
	if err != nil || got.Status != StatusPending || got.Metadata["consent"] != true || got.Metadata["submitter_email"] != "amina@example.com" {
		t.Fatalf("stored: %+v %v", got, err)
	}
	if rec := postForm(t, h, "no-such-business", ok); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown slug: %d", rec.Code)
	}
}
