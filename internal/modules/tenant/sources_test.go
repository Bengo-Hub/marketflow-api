package tenant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Plan item 75: each source shows its value and last sync, Sync now reads past the cache, and a
// service that is down is shown as such without hiding the others.
func TestSettingSources(t *testing.T) {
	db := openProfileDB(t)
	ctx := context.Background()
	tid := uuid.New()
	if _, err := db.Tenant.Create().SetID(tid).SetName("S " + tid.String()).SetSlug("s-" + tid.String()).Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })

	var freshSeen []bool
	h := NewHandler(db, zap.NewNop())
	h.Sources = []SettingSource{
		{Key: "up", Name: "Up", From: "Books", State: func(_ context.Context, tn *ent.Tenant, fresh bool) (SourceState, error) {
			freshSeen = append(freshSeen, fresh)
			return SourceState{Detail: tn.Slug, LastSync: time.Now()}, nil
		}},
		{Key: "down", Name: "Down", From: "Billing", State: func(context.Context, *ent.Tenant, bool) (SourceState, error) {
			return SourceState{}, errors.New("no answer")
		}},
	}
	r := chi.NewRouter()
	r.Get("/sources", h.ListSources)
	r.Post("/sources/{key}/sync", h.SyncSource)
	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(nil))
		req.Header.Set("X-Tenant-ID", tid.String())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	var list struct{ Data []sourceView }
	w := call(http.MethodGet, "/sources")
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil || len(list.Data) != 2 {
		t.Fatalf("%v %s", err, w.Body)
	}
	if up, down := list.Data[0], list.Data[1]; !up.OK || up.LastSync == nil || up.Detail != "s-"+tid.String() || down.OK || down.LastSync != nil {
		t.Fatalf("%+v", list.Data)
	}
	if w := call(http.MethodPost, "/sources/up/sync"); w.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", w.Code, w.Body)
	}
	if len(freshSeen) != 2 || freshSeen[0] || !freshSeen[1] {
		t.Fatalf("fresh flags %v", freshSeen)
	}
	if w := call(http.MethodPost, "/sources/down/sync"); w.Code != http.StatusBadGateway {
		t.Fatalf("down: %d", w.Code)
	}
	if w := call(http.MethodPost, "/sources/nope/sync"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown: %d", w.Code)
	}
}

// An auth.tenant.updated event brings the status in line and drops the cached account details,
// and never the name a business edited in MarketFlow.
func TestSyncerRefresh(t *testing.T) {
	db := openProfileDB(t)
	ctx := context.Background()
	tid := uuid.New()
	slug := "r-" + tid.String()
	if _, err := db.Tenant.Create().SetID(tid).SetName("My own name").SetSlug(slug).SetStatus("active").Save(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Tenant.DeleteOneID(tid).Exec(ctx) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"` + tid.String() + `","slug":"` + slug + `","name":"Auth name","status":"suspended"}`))
	}))
	defer srv.Close()
	s := &Syncer{client: db, authAPIURL: srv.URL, httpClient: srv.Client(), logger: zap.NewNop()}
	s.profiles.Store(slug, cachedProfile{p: &AuthProfile{Name: "old"}, at: time.Now()})
	if err := s.Refresh(ctx, slug); err != nil {
		t.Fatal(err)
	}
	got, err := db.Tenant.Get(ctx, tid)
	if err != nil || got.Status != "suspended" || got.Name != "My own name" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, ok := s.ProfileReadAt(slug); ok {
		t.Fatal("the cached account details were kept")
	}
}
