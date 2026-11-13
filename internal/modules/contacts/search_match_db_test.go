package contacts

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent/contact"
)

// TestSearch_FullNameAcrossColumns: a full name typed in one box finds the contact whose first and
// last names sit in two columns, in either word order, and the list endpoint follows the same rule.
func TestSearch_FullNameAcrossColumns(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background())
	})

	if _, err := client.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLifecycleStage("customer").
		SetFirstName("Test").SetLastName("Customer").SetEmail("buyer@example.co.ke").Save(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Contact.Create().SetTenantID(tid).SetContactType("customer").SetLifecycleStage("customer").
		SetFirstName("Amina").SetLastName("Hassan").Save(ctx); err != nil {
		t.Fatal(err)
	}

	cases := map[string]int{
		"Test Customer":   1,
		"customer test":   1,
		"test":            1,
		"  test   cust  ": 1,
		"buyer@example":   1,
		"Test Hassan":     0,
		"":                2,
	}
	for q, want := range cases {
		_, total, err := svc.Search(ctx, tid, q, 20, 0)
		if err != nil {
			t.Fatalf("Search(%q): %v", q, err)
		}
		if total != want {
			t.Errorf("Search(%q) total = %d, want %d", q, total, want)
		}
		query := q
		_, listTotal, err := svc.List(ctx, ListInput{TenantID: tid, Query: &query, Limit: 20})
		if err != nil {
			t.Fatalf("List(%q): %v", q, err)
		}
		if listTotal != want {
			t.Errorf("List(%q) total = %d, want %d", q, listTotal, want)
		}
	}
}

// TestSearch_ContactTypeFilter: treasury's Customers page asks for customers only, so a lead is
// left out; no filter lists every type (S2-8 review M-2).
func TestSearch_ContactTypeFilter(t *testing.T) {
	svc, client := newContactsTestService(t)
	ctx := context.Background()
	tid := uuid.New()
	seedTenant(t, client, tid)
	t.Cleanup(func() {
		_, _ = client.Contact.Delete().Where(contact.TenantID(tid)).Exec(context.Background())
	})
	for _, typ := range []string{"customer", "lead"} {
		if _, err := client.Contact.Create().SetTenantID(tid).SetContactType(typ).SetLifecycleStage(typ).
			SetFirstName("Asha").SetLastName(typ).Save(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, total, err := svc.Search(ctx, tid, "", 20, 0, "customer"); err != nil || total != 1 {
		t.Errorf("customers only: total %d, err %v; want 1", total, err)
	}
	if _, total, err := svc.Search(ctx, tid, "", 20, 0); err != nil || total != 2 {
		t.Errorf("no filter: total %d, err %v; want 2", total, err)
	}
	if got := contactTypesParam(" customer, ,lead "); len(got) != 2 || got[0] != "customer" || got[1] != "lead" {
		t.Errorf("contactTypesParam = %v", got)
	}
}
