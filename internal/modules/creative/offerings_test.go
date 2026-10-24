package creative

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
