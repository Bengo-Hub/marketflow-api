package contacts

import (
	"context"
	"testing"

	"github.com/google/uuid"

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
