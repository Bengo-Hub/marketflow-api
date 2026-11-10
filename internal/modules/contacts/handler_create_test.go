package contacts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

// postCreate sends body to Handler.Create for the tenant and returns the recorder.
func postCreate(h *Handler, tid uuid.UUID, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/contacts", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Tenant-ID", tid.String())
	rec := httptest.NewRecorder()
	h.Create(rec, req)
	return rec
}

// errorCode reads the "code" field of an httpapi.Error response.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return body.Code
}

// The rejection tests need no database: validation runs before the service is called.
func rejectingHandler() *Handler {
	return NewHandler(NewService(nil, zap.NewNop()), zap.NewNop())
}

func TestCreateRejectsNoIdentifier(t *testing.T) {
	rec := postCreate(rejectingHandler(), uuid.New(), `{"first_name":"A"}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "missing_identifier" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
	// Blank strings count as absent.
	rec = postCreate(rejectingHandler(), uuid.New(), `{"phone":"  ","email":""}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "missing_identifier" {
		t.Fatalf("blank values: got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsBadPhone(t *testing.T) {
	rec := postCreate(rejectingHandler(), uuid.New(), `{"phone":"12"}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_phone" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsBadEmail(t *testing.T) {
	rec := postCreate(rejectingHandler(), uuid.New(), `{"email":"not-an-email"}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_email" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateRejectsUnknownType(t *testing.T) {
	rec := postCreate(rejectingHandler(), uuid.New(), `{"email":"a@example.com","contact_type":"vip"}`)
	if rec.Code != http.StatusBadRequest || errorCode(t, rec) != "invalid_contact_type" {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCreateAcceptsEmailOnly(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	h := NewHandler(svc, zap.NewNop())
	rec := postCreate(h, tid, `{"email":"only@example.com"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d %s", rec.Code, rec.Body.String())
	}
}
