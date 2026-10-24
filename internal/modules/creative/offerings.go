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

// Offering is something a brief can advertise, picked from the tenant's own
// catalogue. The brief keeps a snapshot (CreativeBrief.offering); inventory-api
// stays the owner of the item.
type Offering struct {
	Source      string   `json:"source"` // inventory
	SourceID    string   `json:"source_id"`
	Type        string   `json:"type"` // GOODS | SERVICE | RECIPE
	Name        string   `json:"name"`
	SKU         string   `json:"sku,omitempty"`
	Description string   `json:"description,omitempty"`
	Price       *float64 `json:"price,omitempty"`
	PhotoURLs   []string `json:"photo_urls,omitempty"`
}

// Offerings searches the tenant's sellable catalogue over S2S. Only items that
// can be sold are offered: active goods, services and menu recipes, not
// ingredients, and never one marked not for sale.
type Offerings struct {
	inventory *serviceclient.Client
	key       string
	logger    *zap.Logger
}

// offeringsBudget keeps the picker responsive: past it the user types free text.
const offeringsBudget = 3 * time.Second

// NewOfferings builds the picker client; an empty URL or key turns it off.
func NewOfferings(inventoryURL, serviceKey string, logger *zap.Logger) *Offerings {
	o := &Offerings{key: serviceKey, logger: logger.Named("creative.offerings")}
	if inventoryURL == "" || serviceKey == "" {
		return o
	}
	cfg := serviceclient.DefaultConfig(strings.TrimSuffix(inventoryURL, "/"), "inventory-api", logger)
	cfg.Timeout = 2500 * time.Millisecond
	cfg.MaxElapsedTime = offeringsBudget
	cfg.MaxInterval = 500 * time.Millisecond
	cfg.ReadyToTrip = func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 3 }
	o.inventory = serviceclient.New(cfg)
	return o
}

// ErrOfferingsOff means the catalogue is not reachable from here (not configured,
// or inventory-api failed): the UI offers free text instead.
var ErrOfferingsOff = fmt.Errorf("the catalogue is not available right now; describe the offering instead")

// inventoryItem is the part of inventory-api's ItemDTO the picker reads.
type inventoryItem struct {
	ID           uuid.UUID `json:"id"`
	SKU          string    `json:"sku"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	Type         string    `json:"type"`
	IsActive     bool      `json:"is_active"`
	NotForSale   *bool     `json:"not_for_sale"`
	SellingPrice *float64  `json:"selling_price"`
	ImageURL     string    `json:"image_url"`
	Images       []struct {
		URL string `json:"url"`
	} `json:"images"`
}

var sellableTypes = map[string]bool{"GOODS": true, "SERVICE": true, "RECIPE": true}

// Search returns up to limit sellable items whose name or SKU matches q.
func (o *Offerings) Search(ctx context.Context, tenantID uuid.UUID, q string, limit int) ([]Offering, error) {
	if o == nil || o.inventory == nil {
		return nil, ErrOfferingsOff
	}
	if limit <= 0 || limit > 20 {
		limit = 20
	}
	ctx, cancel := context.WithTimeout(ctx, offeringsBudget)
	defer cancel()
	query := url.Values{}
	query.Set("status", "active")
	query.Set("limit", fmt.Sprint(limit*2)) // ingredients and not-for-sale items are dropped below
	query.Set("fields", "id,sku,name,description,type,is_active,not_for_sale,selling_price,image_url,images")
	if q = strings.TrimSpace(q); q != "" {
		query.Set("search", q)
	}
	resp, err := o.inventory.Get(ctx, "/v1/"+tenantID.String()+"/inventory/items?"+query.Encode(),
		map[string]string{"X-API-Key": o.key, "X-Tenant-ID": tenantID.String()})
	if err != nil || !resp.IsSuccess() {
		o.logger.Warn("inventory search failed", zap.Error(err))
		return nil, ErrOfferingsOff
	}
	var page struct {
		Data []inventoryItem `json:"data"`
	}
	if err := resp.DecodeJSON(&page); err != nil {
		return nil, ErrOfferingsOff
	}
	out := make([]Offering, 0, limit)
	for _, it := range page.Data {
		if !it.IsActive || !sellableTypes[it.Type] || (it.NotForSale != nil && *it.NotForSale) {
			continue
		}
		out = append(out, itemOffering(it))
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func imageURLs(it inventoryItem) []string {
	out := make([]string, 0, len(it.Images))
	for _, img := range it.Images {
		out = append(out, img.URL)
	}
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func excerpt(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
