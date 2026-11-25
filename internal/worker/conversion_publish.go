package worker

import (
	"context"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/platform/adconversions"
)

// adAttributed reports whether a lead came from an ad.
func adAttributed(l *ent.Lead) bool { return adconversions.Attributed(l) }

// conversionOutboxID is the outbox id of an order's conversion.
func conversionOutboxID(tenantID uuid.UUID, orderRef string) uuid.UUID {
	return adconversions.OutboxID(tenantID, orderRef)
}

// enqueueAdConversion queues a sale (Purchase) when the contact's originating lead came from an ad.
// Contact details travel only inside the platform (they are hashed before any outbound call).
func enqueueAdConversion(ctx context.Context, db *ent.Client, tenantID, contactID uuid.UUID, orderRef string, value float64, currency, source string) (bool, error) {
	if db == nil || orderRef == "" {
		return false, nil
	}
	c, err := db.Contact.Query().Where(contact.ID(contactID), contact.TenantID(tenantID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if c.LeadID == nil {
		return false, nil
	}
	l, err := db.Lead.Query().Where(lead.ID(*c.LeadID), lead.TenantID(tenantID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	sale := adconversions.Conversion{Name: adconversions.Purchase, Ref: orderRef, Value: value, Currency: currency, Source: source}
	if c.Email != nil {
		sale.Email = *c.Email
	}
	if c.Phone != nil {
		sale.Phone = *c.Phone
	}
	return adconversions.Enqueue(ctx, db, l, sale)
}

// queueConversion is the best-effort call the purchase consumers make after logging the
// activity: a failure is logged, never returned, so the activity is not logged twice on retry.
func queueConversion(ctx context.Context, db *ent.Client, logger *zap.Logger, tenantID, contactID uuid.UUID, orderRef string, value float64, currency, source string) {
	if _, err := enqueueAdConversion(ctx, db, tenantID, contactID, orderRef, value, currency, source); err != nil {
		logger.Warn("ad conversion not queued", zap.String("order", orderRef), zap.Error(err))
	}
}
