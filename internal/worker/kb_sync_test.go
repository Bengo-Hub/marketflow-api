package worker

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestUsesMarketFlow(t *testing.T) {
	if usesMarketFlow(map[string]any{"tagline": "x"}) {
		t.Fatal("a POS-only tenant is not synced")
	}
	if !usesMarketFlow(map[string]any{"marketing_context": map[string]any{}}) || !usesMarketFlow(map[string]any{"team_setup": map[string]any{}}) {
		t.Fatal("a brand context or a team setup means the tenant uses MarketFlow")
	}
}

// item.created says whether the item is for sale, so it may create an entry. item.updated does not
// say, so it only refreshes one that exists. Neither path takes a price from the event.
func TestKBItemEventShape(t *testing.T) {
	tid := uuid.New()
	var created, updated kbItemEvent
	_ = json.Unmarshal([]byte(`{"id":"i1","name":"Lotion","type":"GOODS","is_active":true,"not_for_sale":false,"selling_price":999,"image_url":"https://m/x.jpg"}`), &created)
	_ = json.Unmarshal([]byte(`{"id":"i1","name":"Lotion","type":"GOODS","is_active":true,"image_url":"/local.jpg"}`), &updated)

	c := created.sourceItem(tid)
	if !c.Sellable || c.UpdateOnly || c.Price != nil || c.ImageURL != "https://m/x.jpg" {
		t.Fatalf("created: %+v", c)
	}
	u := updated.sourceItem(tid)
	if !u.Sellable || !u.UpdateOnly || u.ImageURL != "" {
		t.Fatalf("updated: %+v", u)
	}
	for name, ev := range map[string]kbItemEvent{
		"inactive":   {ID: "a", Name: "n", Type: "GOODS"},
		"ingredient": {ID: "a", Name: "n", Type: "INGREDIENT", IsActive: true},
		"controlled": {ID: "a", Name: "n", Type: "GOODS", IsActive: true, Controlled: true},
	} {
		if ev.sourceItem(tid).Sellable {
			t.Fatalf("%s is not offered", name)
		}
	}
}
