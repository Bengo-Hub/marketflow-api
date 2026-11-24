package leads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

// The service-to-service capture (POST /internal/leads, used by codevertex-website since
// 2026-11-24) publishes lead.created like the public path, so the follow-up, the team alert and
// the reminders all apply to it; a repeat of the same person publishes lead.resubmitted instead.
func TestIngestFromServicePublishesLeadCreated(t *testing.T) {
	svc, client := newLeadsTestService(t)
	ctx := context.Background()
	tid := seedLeadsTenant(t, client)
	h := NewHandler(svc, zap.NewNop())
	post := func() int {
		body := `{"service_origin":"codevertex-website","source":"website_contact","email":"esther@example.com","first_name":"Esther",` +
			`"interest":"Enterprise Software Development","metadata":{"message":"We need an ERP."}}`
		r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/leads", strings.NewReader(body))
		r.Header.Set("X-Tenant-ID", tid.String())
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.IngestFromService(w, r)
		return w.Code
	}
	if code := post(); code != http.StatusCreated {
		t.Fatalf("first capture: %d", code)
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("repeat capture: %d", code)
	}
	events, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid)).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var created, resubmitted int
	for _, e := range events {
		switch e.EventType {
		case LeadCreatedEvent:
			created++
		case LeadResubmittedEvent:
			resubmitted++
		}
	}
	if created != 1 || resubmitted != 1 {
		t.Fatalf("lead.created %d, lead.resubmitted %d; want 1 and 1 (events %d)", created, resubmitted, len(events))
	}
}
