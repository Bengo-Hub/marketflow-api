package tenant

import "testing"

func TestPickHQ(t *testing.T) {
	list := []authOutlet{
		{Address: "Mall branch, Kisumu", Status: "active"},
		{Address: "Pioneer House, Oginga Street, Kisumu CBD", IsHQ: true, Status: "active", Metadata: map[string]any{"city": "Kisumu"}},
	}
	if a, c := pickHQ(list); a != "Pioneer House, Oginga Street, Kisumu CBD" || c != "Kisumu" {
		t.Fatalf("got %q %q", a, c)
	}
	if a, _ := pickHQ([]authOutlet{{Address: "Old", Status: "archived"}, {Address: "Branch", Status: "active"}}); a != "Branch" {
		t.Fatalf("no HQ falls back to the first active outlet, got %q", a)
	}
	if a, _ := pickHQ(nil); a != "" {
		t.Fatal("no outlets, no address")
	}
}
