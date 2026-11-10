package contacts

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

// A new contact commits with its crm.contact.created event; a create that
// finds the contact already there emits nothing.
func TestCreateEmitsContactCreatedOnce(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0712345678"), ContactType: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("+254 712 345 678"), ContactType: "customer"}); err != nil {
		t.Fatal(err)
	}
	rows, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.created")).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AggregateID != c.ID || rows[0].AggregateType != "crm" {
		t.Fatalf("contact.created rows = %d", len(rows))
	}
}

// An identity change commits with crm.contact.updated naming only the changed fields; an update
// that touches no identity field emits nothing.
func TestUpdateEmitsContactUpdated(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0712345678"), ContactType: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	updated := func() []*ent.OutboxEvent {
		rows, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.updated")).All(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	if _, err := svc.Update(ctx, tid, c.ID, UpdateInput{Phone: strPtr("0722000111")}); err != nil {
		t.Fatal(err)
	}
	rows := updated()
	if len(rows) != 1 || rows[0].AggregateID != c.ID || rows[0].AggregateType != "crm" {
		t.Fatalf("contact.updated rows = %d", len(rows))
	}
	var ev struct {
		Payload struct {
			ContactID string   `json:"contact_id"`
			Fields    []string `json:"fields"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(rows[0].Payload, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Payload.ContactID != c.ID.String() || len(ev.Payload.Fields) != 1 || ev.Payload.Fields[0] != "phone" {
		t.Fatalf("payload = %s", rows[0].Payload)
	}

	// Notes-only change (metadata) and a re-sent identical phone: no new row.
	if _, err := svc.Update(ctx, tid, c.ID, UpdateInput{Metadata: map[string]any{"note": "vip"}, Phone: strPtr("0722000111")}); err != nil {
		t.Fatal(err)
	}
	if got := len(updated()); got != 1 {
		t.Fatalf("a non-identity update must not emit: rows = %d", got)
	}
}

// Deleting a contact commits with crm.contact.deleted carrying the id only.
func TestDeleteEmitsContactDeleted(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(ctx)
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(ctx)
	})

	c, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0712345679"), ContactType: "customer"})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(ctx, tid, c.ID); err != nil {
		t.Fatal(err)
	}
	// Deleting again matches nothing and emits nothing more.
	if err := svc.Delete(ctx, tid, c.ID); err != nil {
		t.Fatal(err)
	}
	rows, err := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.deleted")).All(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AggregateID != c.ID || rows[0].AggregateType != "crm" {
		t.Fatalf("contact.deleted rows = %d", len(rows))
	}
}
