package endpoints

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestClassifyOffersOnlyMarketingReads(t *testing.T) {
	cases := []struct {
		method, path string
		tags         []string
		ok           bool
		why          string
	}{
		{"GET", "/api/v1/{tenant}/catalog/items", []string{"Catalog"}, true, ""},
		{"GET", "/v1/{tenantID}/pos/online-orders/pickup", []string{"Online Orders"}, true, ""},
		{"GET", "/api/v1/promotions", []string{"Promotions"}, true, ""},
		{"POST", "/api/v1/{tenant}/catalog/items", []string{"Catalog"}, false, "not a read"},
		{"DELETE", "/api/v1/promotions/{id}", []string{"Promotions"}, false, "not a read"},
		{"GET", "/api/v1/pay/{tenant}/gateways", []string{"Public Payments"}, false, "sensitive"},
		{"GET", "/api/v1/external/etims/tokens/balance", []string{"External eTIMS API"}, false, "sensitive"},
		{"GET", "/admin/tenants/{tenant_id}/products", []string{"Products"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/riders/me/notifications", []string{"Rider"}, false, "sensitive"},
		{"GET", "/addresses/default", []string{"Addresses"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/catalog/audit-logs", []string{"Catalog"}, false, "sensitive"},
		{"GET", "/healthz", []string{"Health"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/delivery-policy/suggest-fee", []string{"Delivery"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/fleet/vehicles", []string{"Fleet"}, false, "not marketing data"},
		{"GET", "/api/v1/{tenant}/payroll/runs", nil, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/products", nil, true, ""},
		// found in the first dry run over the live specs
		{"GET", "/api/v1/orders/guest/{orderId}", []string{"Orders"}, false, "sensitive"},
		{"GET", "/api/v1/orders/{orderId}/track", []string{"Orders"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/orders/{orderId}/delivery/task", []string{"Delivery"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/analytics/dashboards/{module}/embed", []string{"Analytics"}, false, "sensitive"},
		{"GET", "/api/v1/products/{product_code}/entitlement", []string{"Products"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/inventory/import-template", []string{"inventory"}, false, "sensitive"},
		{"GET", "/api/v1/{tenant}/promotions/top-sellers", []string{"Promotions"}, true, ""},
	}
	for _, c := range cases {
		ok, why := Classify(c.method, c.path, c.tags)
		if ok != c.ok || (!ok && !strings.HasPrefix(why, c.why)) {
			t.Errorf("%s %s %v = %v %q, want %v %q", c.method, c.path, c.tags, ok, why, c.ok, c.why)
		}
	}
}

func TestParseSpecJoinsTheBasePath(t *testing.T) {
	swagger2 := []byte(`{"swagger":"2.0","basePath":"/api/v1","paths":{
		"/{tenant}/catalog":{"get":{"tags":["Catalog"],"summary":"List catalog"},"post":{"tags":["Catalog"]}},
		"/api/v1/pay/{tenant}/gateways":{"get":{"tags":["Public Payments"]}}}}`)
	ops, err := ParseSpec(swagger2)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, o := range ops {
		got[o.Method+" "+o.Path] = o.Summary
	}
	if got["GET /api/v1/{tenant}/catalog"] != "List catalog" {
		t.Fatalf("base path not joined: %v", got)
	}
	if _, ok := got["POST /api/v1/{tenant}/catalog"]; !ok {
		t.Fatalf("post missing: %v", got)
	}
	if _, ok := got["GET /api/v1/pay/{tenant}/gateways"]; !ok {
		t.Fatalf("a path that already carries the base was prefixed twice: %v", got)
	}

	oas3 := []byte(`{"openapi":"3.0.0","servers":[{"url":"https://posapi.example.com/api"}],"paths":{"/v1/{tenantID}/pos/orders":{"get":{"tags":["POS"]}}}}`)
	ops, err = ParseSpec(oas3)
	if err != nil || len(ops) != 1 || ops[0].Path != "/api/v1/{tenantID}/pos/orders" {
		t.Fatalf("oas3 = %+v, %v", ops, err)
	}
	if _, err := ParseSpec([]byte("<html>")); err == nil {
		t.Fatal("a non-JSON spec must fail")
	}
}

func TestMergeKeepsDecisionsAndDropsGoneOrSensitiveRoutes(t *testing.T) {
	week1 := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	week2 := week1.AddDate(0, 0, 7)
	existing := []Endpoint{
		{Method: "GET", Path: "/api/v1/{tenant}/catalog", Status: StatusApproved, FirstSeen: week1, LastSeen: week1},
		{Method: "GET", Path: "/api/v1/{tenant}/old", Tags: []string{"Catalog"}, Status: StatusRejected, FirstSeen: week1, LastSeen: week1},
	}
	found := []Op{
		{Method: "GET", Path: "/api/v1/{tenant}/catalog", Tags: []string{"Catalog"}, Summary: "List catalog"},
		{Method: "GET", Path: "/api/v1/{tenant}/promotions", Tags: []string{"Promotions"}},
		{Method: "POST", Path: "/api/v1/{tenant}/promotions", Tags: []string{"Promotions"}},
		{Method: "GET", Path: "/api/v1/pay/{tenant}/gateways", Tags: []string{"Payments"}},
	}
	out, added := Merge(existing, found, week2)
	if added != 1 || len(out) != 2 {
		t.Fatalf("added %d, out %+v", added, out)
	}
	if out[0].Path != "/api/v1/{tenant}/catalog" || out[0].Status != StatusApproved || !out[0].FirstSeen.Equal(week1) || !out[0].LastSeen.Equal(week2) || out[0].Summary != "List catalog" {
		t.Fatalf("approved route lost its decision: %+v", out[0])
	}
	if out[1].Path != "/api/v1/{tenant}/promotions" || out[1].Status != StatusCandidate {
		t.Fatalf("new route should be a candidate: %+v", out[1])
	}
}

func TestFillTenantLeavesOtherParams(t *testing.T) {
	id := uuid.MustParse("6a4f0ad8-2c1e-4b8e-9d55-1a2b3c4d5e6f")
	got, params := FillTenant("/api/v1/{tenant}/items/{itemId}/x/{tenantID}/{tenant_id}/{orgSlug}", id, "urban-loft")
	want := "/api/v1/urban-loft/items/{itemId}/x/" + id.String() + "/" + id.String() + "/urban-loft"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if len(params) != 1 || params[0] != "itemId" {
		t.Fatalf("params = %v", params)
	}
}

func TestToolNameIsStableAndReadable(t *testing.T) {
	if got := ToolName("/api/v1/{tenant}/catalog/items"); got != "catalog_items" {
		t.Fatalf("got %q", got)
	}
	if got := ToolName("/v1/{tenantID}/pos/online-orders/pickup"); got != "pos_online_orders_pickup" {
		t.Fatalf("got %q", got)
	}
	if got := ToolName("/api/v1/internal/products/{id}"); got != "internal_products_by_id" {
		t.Fatalf("got %q", got)
	}
}
