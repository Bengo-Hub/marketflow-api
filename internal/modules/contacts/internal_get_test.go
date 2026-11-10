package contacts

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

func internalRouter(h *Handler) http.Handler {
	r := chi.NewRouter()
	r.Post("/internal/contacts/batch", h.InternalBatch)
	r.Get("/internal/contacts/{id}", h.InternalGet)
	return r
}

func seedContact(t *testing.T, client *ent.Client, tid uuid.UUID, phone string) *ent.Contact {
	t.Helper()
	c, err := client.Contact.Create().SetTenantID(tid).SetContactType("customer").
		SetPhone(phone).SetPhoneSubscriberDigits(nationalSubscriberDigits(phone)).SetFirstName("Test").Save(context.Background())
	if err != nil {
		t.Fatalf("seed contact: %v", err)
	}
	return c
}

func TestInternalGetByID(t *testing.T) {
	svc, client := newContactsTestService(t)
	own, other := uuid.New(), uuid.New()
	seedTenant(t, client, own)
	seedTenant(t, client, other)
	t.Cleanup(func() {
		_, _ = client.Contact.Delete().Where(contact.TenantIDIn(own, other)).Exec(context.Background())
	})
	c := seedContact(t, client, own, "+254700000101")
	router := internalRouter(NewHandler(svc, zap.NewNop()))

	get := func(tid uuid.UUID) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/internal/contacts/"+c.ID.String()+"?tenant_id="+tid.String(), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get(own)
	if rec.Code != http.StatusOK {
		t.Fatalf("own tenant: got %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["id"] != c.ID.String() || body["phone"] != "+254700000101" || body["first_name"] != "Test" {
		t.Fatalf("unexpected body %v", body)
	}
	if rec := get(other); rec.Code != http.StatusNotFound {
		t.Fatalf("other tenant: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestInternalBatchGet(t *testing.T) {
	svc, client := newContactsTestService(t)
	own, other := uuid.New(), uuid.New()
	seedTenant(t, client, own)
	seedTenant(t, client, other)
	t.Cleanup(func() {
		_, _ = client.Contact.Delete().Where(contact.TenantIDIn(own, other)).Exec(context.Background())
	})
	a := seedContact(t, client, own, "+254700000201")
	b := seedContact(t, client, own, "+254700000202")
	foreign := seedContact(t, client, other, "+254700000203")
	router := internalRouter(NewHandler(svc, zap.NewNop()))

	post := func(ids []uuid.UUID) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]any{"tenant_id": own, "ids": ids})
		req := httptest.NewRequest(http.MethodPost, "/internal/contacts/batch", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := post([]uuid.UUID{a.ID, b.ID, foreign.ID})
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("a foreign id must be omitted: got %d rows", len(body.Data))
	}

	tooMany := make([]uuid.UUID, 201)
	for i := range tooMany {
		tooMany[i] = uuid.New()
	}
	if rec := post(tooMany); rec.Code != http.StatusBadRequest {
		t.Fatalf("201 ids: got %d %s", rec.Code, rec.Body.String())
	}
}
