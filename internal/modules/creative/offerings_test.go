package creative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// The picker calls inventory-api's S2S items list the way ordering-backend does
// and keeps only sellable items with absolute photo links.
func TestOfferingsSearch(t *testing.T) {
	tid := uuid.New()
	var gotPath, gotSearch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpware.ValidServiceKey(r, "k") || r.Header.Get("X-Tenant-ID") != tid.String() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotPath, gotSearch = r.URL.Path, r.URL.Query().Get("search")
		yes := true
		price := 850.0
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": uuid.NewString(), "name": "Shea butter 250g", "type": "GOODS", "is_active": true, "selling_price": price,
				"image_url": "https://cdn.example.com/shea.jpg",
				"images":    []map[string]any{{"url": "https://cdn.example.com/shea.jpg"}, {"url": "https://cdn.example.com/shea-2.jpg"}, {"url": "/media/relative.jpg"}}},
			{"id": uuid.NewString(), "name": "Shea nuts (raw)", "type": "INGREDIENT", "is_active": true},
			{"id": uuid.NewString(), "name": "Shea sample", "type": "GOODS", "is_active": true, "not_for_sale": yes},
			{"id": uuid.NewString(), "name": "Shea massage", "type": "SERVICE", "is_active": true},
		}, "total": 4})
	}))
	t.Cleanup(srv.Close)

	o := NewOfferings(srv.URL, "k", zap.NewNop())
	got, err := o.Search(context.Background(), tid, "shea", 10)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/"+tid.String()+"/inventory/items" || gotSearch != "shea" {
		t.Fatalf("called %s ?search=%s", gotPath, gotSearch)
	}
	if len(got) != 2 || got[0].Name != "Shea butter 250g" || got[1].Type != "SERVICE" {
		t.Fatalf("offerings: %+v", got)
	}
	if got[0].Price == nil || *got[0].Price != 850 || len(got[0].PhotoURLs) != 2 || got[0].Source != "inventory" {
		t.Fatalf("first offering: %+v", got[0])
	}

	// Not configured, or inventory down: the UI falls back to free text.
	if _, err := NewOfferings("", "", zap.NewNop()).Search(context.Background(), tid, "x", 5); !errors.Is(err, ErrOfferingsOff) {
		t.Fatalf("unconfigured: %v", err)
	}
	if _, err := NewOfferings(srv.URL, "wrong", zap.NewNop()).Search(context.Background(), tid, "x", 5); !errors.Is(err, ErrOfferingsOff) {
		t.Fatalf("refused: %v", err)
	}
}

// Running pos-api promotions come first, described from their rule; ended ones
// are dropped; pos alone still answers when inventory is down.
func TestOfferingsSearchWithPromotions(t *testing.T) {
	tid := uuid.New()
	var gotPath, gotQ string
	pos := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !httpware.ValidServiceKey(r, "k") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		gotPath, gotQ = r.URL.Path, r.URL.Query().Get("q")
		ends := time.Now().Add(48 * time.Hour)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": uuid.NewString(), "name": "Shea weekend", "description": "On all lotions", "promo_code": "SHEA20",
				"status": "active", "end_at": ends,
				"metadata": map[string]any{"banner": map[string]any{"banner_image_url": "https://cdn.example.com/banner.jpg"}},
				"rule":     map[string]any{"discount_type": "percentage", "discount_value": 20}},
			{"id": uuid.NewString(), "name": "Shea last month", "status": "active", "end_at": time.Now().Add(-time.Hour),
				"rule": map[string]any{"discount_type": "percentage", "discount_value": 10}},
			{"id": uuid.NewString(), "name": "Shea duo", "status": "active",
				"rule": map[string]any{"discount_type": "fixed_price", "discount_value": 1500}},
		}, "total": 3})
	}))
	t.Cleanup(pos.Close)
	inv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{
			{"id": uuid.NewString(), "name": "Shea butter 250g", "type": "GOODS", "is_active": true},
		}})
	}))
	t.Cleanup(inv.Close)

	got, err := NewOfferings(inv.URL, "k", zap.NewNop()).WithPromotions(pos.URL).Search(context.Background(), tid, "shea", 10)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/s2s/"+tid.String()+"/discounts" || gotQ != "shea" {
		t.Fatalf("pos called %s ?q=%s", gotPath, gotQ)
	}
	if len(got) != 3 || got[0].Source != SourcePOSPromotion || got[2].Source != "inventory" {
		t.Fatalf("offerings: %+v", got)
	}
	p := got[0]
	if p.Type != TypePromotion || p.PromoCode != "SHEA20" || p.EndsAt == nil || p.Description != "20% off. On all lotions" ||
		len(p.PhotoURLs) != 1 || p.PhotoURLs[0] != "https://cdn.example.com/banner.jpg" {
		t.Fatalf("promotion: %+v", p)
	}
	if got[1].Name != "Shea duo" || got[1].Price == nil || *got[1].Price != 1500 {
		t.Fatalf("fixed price promotion: %+v", got[1])
	}

	// An extra source (the website for the platform tenant) sits between the two,
	// filtered by the query; nil for a tenant means it has nothing for it.
	platform := uuid.New()
	extra := func(_ context.Context, id uuid.UUID) ([]Offering, error) {
		if id != platform {
			return nil, nil
		}
		return []Offering{
			{Source: "website", Type: "COURSE", Name: "Shea soap making", Description: strings.Repeat("x", 400)},
			{Source: "website", Type: "PRODUCT", Name: "Retail POS"},
		}, nil
	}
	withExtra := NewOfferings(inv.URL, "k", zap.NewNop()).WithPromotions(pos.URL).WithSource(extra)
	mixed, err := withExtra.Search(context.Background(), platform, "shea", 10)
	if err != nil || len(mixed) != 4 || mixed[2].Type != "COURSE" || mixed[3].Source != "inventory" || len([]rune(mixed[2].Description)) > 301 {
		t.Fatalf("with the website: %v %+v", err, mixed)
	}
	if other, _ := withExtra.Search(context.Background(), tid, "shea", 10); len(other) != 3 {
		t.Fatalf("another tenant must not see the website offerings: %+v", other)
	}

	// Inventory not configured: promotions alone.
	only, err := NewOfferings("", "k", zap.NewNop()).WithPromotions(pos.URL).Search(context.Background(), tid, "", 10)
	if err != nil || len(only) != 2 {
		t.Fatalf("promotions only: %v %+v", err, only)
	}
}
