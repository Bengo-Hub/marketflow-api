package deals

import (
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// DealStageChangedTopic is the subject a deal's stage move is published on,
// inside the worker stream's crm.> binding.
const DealStageChangedTopic = "crm.deal.stage_changed"

// dealStageChangedEvent describes one move. A deal can move many times, so the
// event's own id is the move id that consumers deduplicate on.
func dealStageChangedEvent(d *ent.Deal, from uuid.UUID) *eventslib.Event {
	ev := eventslib.NewEvent("deal.stage_changed", "crm", d.ID, d.TenantID, map[string]any{
		"deal_id":       d.ID.String(),
		"pipeline_id":   d.PipelineID.String(),
		"from_stage_id": from.String(),
		"to_stage_id":   d.StageID.String(),
	})
	ev.Payload["move_id"] = ev.ID.String()
	return ev
}
