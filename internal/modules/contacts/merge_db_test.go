package contacts

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
)

// TestMerge_HardDeletesAndCarriesIdentity: the duplicate is gone, the survivor keeps its own
// values, takes the duplicate's phone and imported customer code, adds up order totals, and one
// crm.contact.merged event is queued for treasury and pos.
func TestMerge_HardDeletesAndCarriesIdentity(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.OutboxEvent.Delete().Where(outboxevent.TenantID(tid)).Exec(context.Background())
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background())
	})

	dup, err := client.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLifecycleStage("customer").
		SetPhone("+254728579887").SetPhoneSubscriberDigits("728579887").SetFirstName("Mama").SetTotalOrders(3).
		SetMetadata(map[string]any{"legacy_contact_id": "CO0206"}).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}
	survivor, err := client.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLifecycleStage("lead").
		SetEmail("Mama@Shop.co.ke").SetEmailNormalized("mama@shop.co.ke").SetFirstName("Mama Shiko").SetTotalOrders(2).Save(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Merge(ctx, tid, survivor.ID, []uuid.UUID{dup.ID}); err != nil {
		t.Fatalf("Merge: %v", err)
	}

	if n, _ := client.Contact.Query().Where(contact.ID(dup.ID)).Count(ctx); n != 0 {
		t.Fatal("the duplicate must be hard deleted")
	}
	got, err := client.Contact.Get(ctx, survivor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phone == nil || *got.Phone != "+254728579887" || got.PhoneSubscriberDigits == nil || *got.PhoneSubscriberDigits != "728579887" {
		t.Errorf("phone not carried over: %v / %v", got.Phone, got.PhoneSubscriberDigits)
	}
	if *got.FirstName != "Mama Shiko" {
		t.Errorf("survivor's own name overwritten: %s", *got.FirstName)
	}
	if got.Metadata["legacy_contact_id"] != "CO0206" {
		t.Errorf("customer code not carried over: %v", got.Metadata)
	}
	if got.TotalOrders != 5 || got.LifecycleStage != "customer" {
		t.Errorf("totals/stage = %d/%s, want 5/customer", got.TotalOrders, got.LifecycleStage)
	}
	if n, _ := client.OutboxEvent.Query().Where(outboxevent.TenantID(tid), outboxevent.EventType("contact.merged")).Count(ctx); n != 1 {
		t.Errorf("crm.contact.merged events queued = %d, want 1", n)
	}

	// The vacated phone now dedups to the survivor.
	again, err := svc.Create(ctx, CreateInput{TenantID: tid, Phone: strPtr("0728 579 887"), ContactType: "customer"})
	if err != nil || again.ID != survivor.ID {
		t.Errorf("Create after merge = %v, %v; want the survivor", again, err)
	}
}
