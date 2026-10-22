package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// adLeadSources are lead sources that came from a paid ad platform.
var adLeadSources = map[string]bool{"meta_ad": true, "tiktok_ad": true, "google_ad": true}

// adAttributed reports whether a lead came from an ad: an ad source, or a stored click id.
func adAttributed(l *ent.Lead) bool {
	if l == nil {
		return false
	}
	if adLeadSources[l.Source] {
		return true
	}
	for _, k := range []string{"fbclid", "gclid", "ctwa_clid", "ttclid"} {
		if v, _ := l.Metadata[k].(string); v != "" {
			return true
		}
	}
	return false
}

// conversionOutboxID is deterministic per tenant and order, so a redelivered sale event can
// never enqueue a second conversion for the same order.
func conversionOutboxID(tenantID uuid.UUID, orderRef string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("marketflow:conversion:"+tenantID.String()+":"+orderRef))
}

// enqueueAdConversion writes a conversion.occurred outbox row (Purchase) when the contact's
// originating lead came from an ad. The API's outbox poller publishes it and the worker's
// ConversionsHandler sends it to Meta CAPI and Google Ads. Returns whether a row was written.
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
	if !adAttributed(l) {
		return false, nil
	}

	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	ev := ConversionEvent{
		TenantID:     tenantID.String(),
		EventName:    "Purchase",
		Value:        value,
		Currency:     currency,
		OccurredAt:   time.Now().UTC().Format(time.RFC3339),
		Gclid:        str(l.Metadata, "gclid"),
		Fbclid:       str(l.Metadata, "fbclid"),
		ClientUserID: l.ID.String(),
		Source:       source,
	}
	if c.Email != nil {
		ev.Email = *c.Email
	}
	if c.Phone != nil {
		ev.Phone = *c.Phone
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, err
	}

	event := eventslib.NewEvent("occurred", "conversion", l.ID, tenantID, payload)
	event.ID = conversionOutboxID(tenantID, orderRef)
	data, err := event.ToJSON()
	if err != nil {
		return false, err
	}
	if _, err := db.OutboxEvent.Create().
		SetID(event.ID).
		SetTenantID(tenantID).
		SetAggregateType(event.AggregateType).
		SetAggregateID(event.AggregateID).
		SetEventType(event.EventType).
		SetPayload(data).
		SetStatus("PENDING").
		Save(ctx); err != nil {
		if ent.IsConstraintError(err) {
			return false, nil // this order's conversion is already queued
		}
		return false, fmt.Errorf("enqueue conversion: %w", err)
	}
	return true, nil
}

// queueConversion is the best-effort call the purchase consumers make after logging the
// activity: a failure is logged, never returned, so the activity is not logged twice on retry.
func queueConversion(ctx context.Context, db *ent.Client, logger *zap.Logger, tenantID, contactID uuid.UUID, orderRef string, value float64, currency, source string) {
	if _, err := enqueueAdConversion(ctx, db, tenantID, contactID, orderRef, value, currency, source); err != nil {
		logger.Warn("ad conversion not queued", zap.String("order", orderRef), zap.Error(err))
	}
}
