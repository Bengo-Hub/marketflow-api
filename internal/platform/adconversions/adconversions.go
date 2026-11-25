// Package adconversions queues the conversions MarketFlow reports to a tenant's own ad accounts:
// a qualified lead, a booked meeting and a sale of a person who came from an ad (owner, 2026-11-25).
// The outbox publishes conversion.occurred and the worker sends it to Meta and Google.
package adconversions

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// Event names, the ones a tenant maps to conversion actions (googleads.Events).
const (
	Lead     = "Lead"     // marked qualified or converted
	Booking  = "Booking"  // a meeting was booked
	Purchase = "Purchase" // a sale
)

// Event is the conversion.occurred payload.
type Event struct {
	TenantID   string  `json:"tenant_id"`
	EventName  string  `json:"event_name"` // Lead | Booking | Purchase
	Value      float64 `json:"value"`
	Currency   string  `json:"currency"`
	OccurredAt string  `json:"occurred_at"` // RFC3339
	Gclid      string  `json:"gclid,omitempty"`
	// Wbraid and Gbraid are Google's click ids for iPhone clicks that carry no gclid.
	Wbraid string `json:"wbraid,omitempty"`
	Gbraid string `json:"gbraid,omitempty"`
	// ConversionID is the same for every delivery of one conversion, so a platform counts it once.
	ConversionID string `json:"conversion_id,omitempty"`
	Fbclid       string `json:"fbclid,omitempty"`
	// CtwaClid is the click id of a click-to-WhatsApp ad (Sprint 18); such a sale goes to Meta as
	// a business_messaging event on the WhatsApp dataset, not a website event.
	CtwaClid     string `json:"ctwa_clid,omitempty"`
	ClientUserID string `json:"client_user_id,omitempty"` // Lead UUID for dedup
	Email        string `json:"email,omitempty"`          // Raw, hashed before outbound
	Phone        string `json:"phone,omitempty"`          // Raw, hashed before outbound
	Source       string `json:"source,omitempty"`         // lead | meeting | order
}

// adSources are lead sources that came from a paid ad platform.
var adSources = map[string]bool{"meta_ad": true, "tiktok_ad": true, "google_ad": true, "ctwa": true}

// Attributed reports whether a lead came from an ad: an ad source, or a stored click id.
func Attributed(l *ent.Lead) bool {
	if l == nil {
		return false
	}
	if adSources[l.Source] {
		return true
	}
	for _, k := range []string{"fbclid", "gclid", "wbraid", "gbraid", "ctwa_clid", "ttclid"} {
		if v, _ := l.Metadata[k].(string); v != "" {
			return true
		}
	}
	return false
}

// OutboxID is deterministic per tenant and reference, so a repeated call never queues the same
// conversion twice.
func OutboxID(tenantID uuid.UUID, ref string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("marketflow:conversion:"+tenantID.String()+":"+ref))
}

// Conversion is one thing to report for a lead.
type Conversion struct {
	Name     string
	Ref      string // "pos:<order>", "lead:<id>", "meeting:<lead id>"
	Value    float64
	Currency string
	Source   string
	// Email and Phone override the lead's own (a sale reports the buyer's contact).
	Email, Phone string
}

// Enqueue writes the conversion's outbox row when the lead came from an ad. It reports whether a
// row was written; one already queued is not an error.
func Enqueue(ctx context.Context, db *ent.Client, l *ent.Lead, c Conversion) (bool, error) {
	if db == nil || c.Ref == "" || !Attributed(l) {
		return false, nil
	}
	str := func(k string) string { v, _ := l.Metadata[k].(string); return v }
	id := OutboxID(l.TenantID, c.Ref)
	ev := Event{
		TenantID: l.TenantID.String(), EventName: c.Name, Value: c.Value, Currency: c.Currency,
		OccurredAt: time.Now().UTC().Format(time.RFC3339),
		Gclid:      str("gclid"), Wbraid: str("wbraid"), Gbraid: str("gbraid"),
		ConversionID: id.String(), Fbclid: str("fbclid"), CtwaClid: str("ctwa_clid"),
		ClientUserID: l.ID.String(), Email: c.Email, Phone: c.Phone, Source: c.Source,
	}
	if ev.Email == "" && l.Email != nil {
		ev.Email = *l.Email
	}
	if ev.Phone == "" && l.Phone != nil {
		ev.Phone = *l.Phone
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		return false, err
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, err
	}
	event := eventslib.NewEvent("occurred", "conversion", l.ID, l.TenantID, payload)
	event.ID = id
	data, err := event.ToJSON()
	if err != nil {
		return false, err
	}
	if _, err := db.OutboxEvent.Create().
		SetID(event.ID).
		SetTenantID(l.TenantID).
		SetAggregateType(event.AggregateType).
		SetAggregateID(event.AggregateID).
		SetEventType(event.EventType).
		SetPayload(data).
		SetStatus("PENDING").
		Save(ctx); err != nil {
		if ent.IsConstraintError(err) {
			return false, nil
		}
		return false, fmt.Errorf("enqueue conversion: %w", err)
	}
	return true, nil
}

// QualifiedLead reports a lead marked qualified or converted, once in its life.
func QualifiedLead(ctx context.Context, db *ent.Client, l *ent.Lead) (bool, error) {
	if l == nil {
		return false, nil
	}
	return Enqueue(ctx, db, l, Conversion{Name: Lead, Ref: "lead:" + l.ID.String(), Source: "lead"})
}

// BookedMeeting reports a lead's booked meeting, once per lead.
func BookedMeeting(ctx context.Context, db *ent.Client, tenantID, leadID uuid.UUID) (bool, error) {
	l, err := db.Lead.Query().Where(lead.ID(leadID), lead.TenantID(tenantID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return Enqueue(ctx, db, l, Conversion{Name: Booking, Ref: "meeting:" + l.ID.String(), Source: "meeting"})
}
