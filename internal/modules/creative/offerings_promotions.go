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

// A brief can also advertise one of the tenant's running promotions. pos-api is
// the discount source of truth; the picker reads its S2S list
// (GET /api/v1/s2s/{tenant}/discounts, active only) and keeps a snapshot, like
// an inventory item.

// SourcePOSPromotion marks an offering that is a pos-api promotion.
const SourcePOSPromotion = "pos_promotion"

// TypePromotion is a promotion offering's type.
const TypePromotion = "PROMOTION"

// WithPromotions adds the tenant's active pos-api promotions to the picker; an
// empty URL leaves them out.
func (o *Offerings) WithPromotions(posURL string) *Offerings {
	if posURL == "" || o.key == "" {
		return o
	}
	cfg := serviceclient.DefaultConfig(strings.TrimSuffix(posURL, "/"), "pos-api", o.logger)
	cfg.Timeout = 2500 * time.Millisecond
	cfg.MaxElapsedTime = offeringsBudget
	cfg.MaxInterval = 500 * time.Millisecond
	cfg.ReadyToTrip = func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 3 }
	o.pos = serviceclient.New(cfg)
	return o
}

// posPromotion is the part of pos-api's promotionWithRule the picker reads.
type posPromotion struct {
	ID          uuid.UUID      `json:"id"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	PromoCode   string         `json:"promo_code"`
	Status      string         `json:"status"`
	EndAt       *time.Time     `json:"end_at"`
	Metadata    map[string]any `json:"metadata"`
	Rule        *struct {
		DiscountType  string  `json:"discount_type"`
		DiscountValue float64 `json:"discount_value"`
		BuyQuantity   int     `json:"buy_quantity"`
		GetQuantity   int     `json:"get_quantity"`
	} `json:"rule"`
}

// promotions returns up to limit running promotions whose name matches q.
// ok is false when pos-api is not configured or did not answer.
func (o *Offerings) promotions(ctx context.Context, tenantID uuid.UUID, q string, limit int) ([]Offering, bool) {
	if o == nil || o.pos == nil {
		return nil, false
	}
	query := url.Values{}
	query.Set("limit", fmt.Sprint(limit))
	if q = strings.TrimSpace(q); q != "" {
		query.Set("q", q)
	}
	resp, err := o.pos.Get(ctx, "/api/v1/s2s/"+tenantID.String()+"/discounts?"+query.Encode(),
		map[string]string{"X-API-Key": o.key, "X-Tenant-ID": tenantID.String()})
	if err != nil || !resp.IsSuccess() {
		o.logger.Warn("pos promotions unavailable", zap.Error(err))
		return nil, false
	}
	var page struct {
		Data []posPromotion `json:"data"`
	}
	if err := resp.DecodeJSON(&page); err != nil {
		return nil, false
	}
	now := time.Now()
	out := make([]Offering, 0, len(page.Data))
	for _, p := range page.Data {
		if p.Status != "active" || (p.EndAt != nil && p.EndAt.Before(now)) {
			continue
		}
		out = append(out, promotionOffering(p))
	}
	return out, true
}

func promotionOffering(p posPromotion) Offering {
	off := Offering{Source: SourcePOSPromotion, SourceID: p.ID.String(), Type: TypePromotion, Name: p.Name,
		PromoCode: p.PromoCode, EndsAt: p.EndAt}
	parts := []string{}
	if p.Rule != nil {
		switch p.Rule.DiscountType {
		case "percentage":
			parts = append(parts, fmt.Sprintf("%g%% off", p.Rule.DiscountValue))
		case "fixed_amount":
			parts = append(parts, fmt.Sprintf("%g off", p.Rule.DiscountValue))
		case "fixed_price":
			price := p.Rule.DiscountValue
			off.Price = &price
		case "bogo":
			parts = append(parts, fmt.Sprintf("buy %d get %d", max(p.Rule.BuyQuantity, 1), max(p.Rule.GetQuantity, 1)))
		case "free_delivery":
			parts = append(parts, "free delivery")
		}
	}
	if d := strings.TrimSpace(p.Description); d != "" {
		parts = append(parts, d)
	}
	off.Description = excerpt(strings.Join(parts, ". "), 300)
	if banner, ok := p.Metadata["banner"].(map[string]any); ok {
		if img := str(banner["banner_image_url"]); strings.HasPrefix(img, "https://") || strings.HasPrefix(img, "http://") {
			off.PhotoURLs = []string{img}
		}
	}
	return off
}
