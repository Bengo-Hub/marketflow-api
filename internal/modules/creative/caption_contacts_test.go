package creative

import (
	"reflect"
	"testing"
)

func TestContactLinesAreTheEndCardsContacts(t *testing.T) {
	got := contactLines([]contact{
		{Kind: "whatsapp", Value: "+254 700 000 000"}, {Kind: "email", Value: "hello@example.com"},
		{Kind: "web", Value: " example.com "}, {Kind: "phone", Value: ""}, {Kind: "other", Value: "Shop 4"},
	})
	want := []string{"WhatsApp: +254 700 000 000", "Email: hello@example.com", "Website: example.com", "Shop 4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lines %q", got)
	}
	if got := contactLines(nil); got == nil || len(got) != 0 {
		t.Fatalf("no contacts must be an empty list, got %v", got)
	}
}
