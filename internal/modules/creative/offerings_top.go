package creative

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	serviceclient "github.com/Bengo-Hub/shared-service-client"
	"github.com/google/uuid"
	"github.com/sony/gobreaker"
	"go.uber.org/zap"
)

// TopOfferings lists a tenant's best-selling sellable items: POS sales over the last
// salesWindow ranked by revenue, matched to the inventory catalogue for type, price and
// photos. When POS has no sales (a service business, a new shop) it falls back to the
// catalogue itself. pos-api and inventory-api stay the owners; callers keep snapshots.
type TopOfferings struct {
	offerings *Offerings
	pos       *serviceclient.Client
	key       string
	logger    *zap.Logger
}

const salesWindow = 90 * 24 * time.Hour

// NewTopOfferings builds the lister on top of the picker client; an empty POS URL leaves
// only the catalogue fallback.
func NewTopOfferings(offerings *Offerings, posURL, serviceKey string, logger *zap.Logger) *TopOfferings {
	t := &TopOfferings{offerings: offerings, key: serviceKey, logger: logger.Named("creative.top_offerings")}
	if posURL == "" || serviceKey == "" {
		return t
	}
	cfg := serviceclient.DefaultConfig(strings.TrimSuffix(posURL, "/"), "pos-api", logger)
	cfg.Timeout = 2500 * time.Millisecond
	cfg.MaxElapsedTime = offeringsBudget
	cfg.MaxInterval = 500 * time.Millisecond
	cfg.ReadyToTrip = func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 3 }
	t.pos = serviceclient.New(cfg)
	return t
}

// TopOffering is an Offering with the POS figures that ranked it (zero for the fallback).
type TopOffering struct {
	Offering
	QuantitySold float64 `json:"quantity_sold,omitempty"`
	Revenue      float64 `json:"revenue,omitempty"`
}

type posSkuSales struct {
	SKU          string  `json:"sku"`
	Name         string  `json:"name"`
	QuantitySold float64 `json:"quantity_sold"`
	Revenue      float64 `json:"revenue"`
}

// Top returns up to limit (max 20) best-selling sellable items, within the picker's budget.
// ErrOfferingsOff means neither source answered; callers fall back to free text.
func (t *TopOfferings) Top(ctx context.Context, tenantID uuid.UUID, limit int) ([]TopOffering, error) {
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	ctx, cancel := context.WithTimeout(ctx, offeringsBudget)
	defer cancel()

	sales := t.bestSellers(ctx, tenantID, limit*2) // some SKUs are ingredients or no longer sold
	if len(sales) > 0 {
		if out := t.matchCatalogue(ctx, tenantID, sales, limit); len(out) > 0 {
			return out, nil
		}
	}
	if t.offerings == nil {
		return nil, ErrOfferingsOff
	}
	items, err := t.offerings.items(ctx, tenantID, "", limit) // catalogue only: promotions are not best sellers
	if err != nil {
		return nil, err
	}
	out := make([]TopOffering, 0, len(items))
	for _, it := range items {
		out = append(out, TopOffering{Offering: it})
	}
	return out, nil
}

func (t *TopOfferings) bestSellers(ctx context.Context, tenantID uuid.UUID, n int) []posSkuSales {
	if t == nil || t.pos == nil {
		return nil
	}
	now := time.Now().UTC()
	q := url.Values{}
	q.Set("from", now.Add(-salesWindow).Format("2006-01-02"))
	q.Set("to", now.Format("2006-01-02"))
	q.Set("order", "revenue")
	q.Set("limit", fmt.Sprint(n))
	resp, err := t.pos.Get(ctx, "/api/v1/s2s/"+tenantID.String()+"/pos/sales/by-sku?"+q.Encode(),
		map[string]string{"X-API-Key": t.key, "X-Tenant-ID": tenantID.String()})
	if err != nil || !resp.IsSuccess() {
		t.logger.Debug("pos best sellers unavailable", zap.Error(err))
		return nil
	}
	var body struct {
		Data []posSkuSales `json:"data"`
	}
	if err := resp.DecodeJSON(&body); err != nil {
		return nil
	}
	return body.Data
}

// matchCatalogue keeps the sellable catalogue items among the best sellers, in sales order.
func (t *TopOfferings) matchCatalogue(ctx context.Context, tenantID uuid.UUID, sales []posSkuSales, limit int) []TopOffering {
	o := t.offerings
	if o == nil || o.inventory == nil {
		return nil
	}
	skus := make([]string, 0, len(sales))
	for _, s := range sales {
		skus = append(skus, s.SKU)
	}
	q := url.Values{}
	q.Set("status", "active")
	q.Set("skus", strings.Join(skus, ","))
	q.Set("limit", fmt.Sprint(len(skus)))
	q.Set("fields", "id,sku,name,description,type,is_active,not_for_sale,selling_price,image_url,images")
	resp, err := o.inventory.Get(ctx, "/v1/"+tenantID.String()+"/inventory/items?"+q.Encode(),
		map[string]string{"X-API-Key": o.key, "X-Tenant-ID": tenantID.String()})
	if err != nil || !resp.IsSuccess() {
		t.logger.Debug("inventory lookup for best sellers failed", zap.Error(err))
		return nil
	}
	var page struct {
		Data []inventoryItem `json:"data"`
	}
	if err := resp.DecodeJSON(&page); err != nil {
		return nil
	}
	bySKU := make(map[string]inventoryItem, len(page.Data))
	for _, it := range page.Data {
		bySKU[it.SKU] = it
	}
	out := make([]TopOffering, 0, limit)
	for _, s := range sales {
		it, ok := bySKU[s.SKU]
		if !ok || !it.IsActive || !sellableTypes[it.Type] || (it.NotForSale != nil && *it.NotForSale) {
			continue
		}
		out = append(out, TopOffering{Offering: itemOffering(it), QuantitySold: s.QuantitySold, Revenue: s.Revenue})
		if len(out) == limit {
			break
		}
	}
	return out
}

// itemOffering maps a catalogue item to an Offering, keeping only absolute photo links
// (marketflow-ai downloads them for posters). Search builds the same shape inline.
func itemOffering(it inventoryItem) Offering {
	off := Offering{Source: "inventory", SourceID: it.ID.String(), Type: it.Type, Name: it.Name, SKU: it.SKU,
		Description: excerpt(it.Description, 300), Price: it.SellingPrice}
	for _, u := range append([]string{it.ImageURL}, imageURLs(it)...) {
		if (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) && !contains(off.PhotoURLs, u) && len(off.PhotoURLs) < maxPhotos {
			off.PhotoURLs = append(off.PhotoURLs, u)
		}
	}
	return off
}
