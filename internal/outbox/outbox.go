// Package outbox writes domain events to the transactional outbox. The shared
// events poller publishes each row on "<aggregate_type>.<event_type>", so a
// row saved in the same transaction as the change it describes is published
// exactly when that change commits.
package outbox

import (
	"context"
	"fmt"

	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Enqueue saves ev as a pending outbox row. Pass the transaction's
// OutboxEvent client (tx.OutboxEvent) so the event commits with the change.
func Enqueue(ctx context.Context, c *ent.OutboxEventClient, ev *eventslib.Event) error {
	payload, err := ev.ToJSON()
	if err != nil {
		return fmt.Errorf("outbox: marshal %s.%s: %w", ev.AggregateType, ev.EventType, err)
	}
	if _, err := c.Create().
		SetID(ev.ID).
		SetTenantID(ev.TenantID).
		SetAggregateType(ev.AggregateType).
		SetAggregateID(ev.AggregateID).
		SetEventType(ev.EventType).
		SetPayload(payload).
		SetStatus("PENDING").
		Save(ctx); err != nil {
		return fmt.Errorf("outbox: enqueue %s.%s: %w", ev.AggregateType, ev.EventType, err)
	}
	return nil
}
