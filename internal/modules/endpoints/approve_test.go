package endpoints

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecideRecordsTheOwnersChoice(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	list := []Endpoint{{Method: "GET", Path: "/api/v1/{tenant}/promotions/deals", Status: StatusCandidate}}

	got, err := Decide(list, "GET", "/api/v1/{tenant}/promotions/deals", StatusApproved, "owner@codevertex", now)
	if err != nil || got[0].Status != StatusApproved || got[0].DecidedBy != "owner@codevertex" || !got[0].DecidedAt.Equal(now) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if list[0].Status != StatusCandidate {
		t.Fatal("Decide must not change the caller's slice")
	}
	if _, err := Decide(list, "GET", "/nope", StatusApproved, "x", now); err != ErrUnknownEndpoint {
		t.Fatalf("unknown route: %v", err)
	}
	if _, err := Decide(list, "GET", "/api/v1/{tenant}/promotions/deals", "deleted", "x", now); err != ErrBadStatus {
		t.Fatalf("bad status: %v", err)
	}
}

func TestApprovedToolsAreTenantFilledAndOnlyApproved(t *testing.T) {
	id := uuid.MustParse("6a4f0ad8-2c1e-4b8e-9d55-1a2b3c4d5e6f")
	all := map[string][]Endpoint{
		"ordering": {
			{Method: "GET", Path: "/api/v1/{tenant}/promotions/deals", Summary: "Deals", Status: StatusApproved},
			{Method: "GET", Path: "/api/v1/{tenant}/promotions/banners", Status: StatusCandidate},
			{Method: "GET", Path: "/api/v1/{tenant}/catalog/items/{itemId}", Status: StatusApproved},
		},
		"pos":     {{Method: "GET", Path: "/v1/{tenantID}/pos/online-orders/pickup", Status: StatusRejected}},
		"unknown": {{Method: "GET", Path: "/api/v1/{tenant}/items", Status: StatusApproved}},
	}
	bases := map[string]string{"ordering": "http://ordering-backend.ordering.svc.cluster.local:4000", "pos": "http://pos"}
	tools := ApprovedTools(all, bases, id, "urban-loft")
	if len(tools) != 2 {
		t.Fatalf("want the two approved ordering routes, got %+v", tools)
	}
	if tools[0].Resource != "catalog_items_by_itemid" || tools[0].URL != "http://ordering-backend.ordering.svc.cluster.local:4000/api/v1/urban-loft/catalog/items/{itemId}" ||
		len(tools[0].Params) != 1 || tools[0].Params[0] != "itemId" {
		t.Fatalf("tool 0 = %+v", tools[0])
	}
	if tools[1].Resource != "promotions_deals" || tools[1].Service != "ordering" || tools[1].Summary != "Deals" || len(tools[1].Params) != 0 {
		t.Fatalf("tool 1 = %+v", tools[1])
	}
}
