package worker

import (
	"encoding/json"
	"strconv"
)

// eventBody returns the JSON object a handler should decode from a NATS message.
//
// Other services (pos, ordering, treasury) and this service's outbox publish the shared-events
// envelope {id, event_type, tenant_id, payload:{...}}, so the business fields sit under
// "payload". Internal work signals (nurture.step.due, cron ticks) are flat JSON. Handlers that
// unmarshalled msg.Data() straight into a flat struct silently read nothing from an envelope:
// pos.sale.finalized, for example, never logged a single sale on a CRM contact.
//
// For an envelope this returns the payload object, adding the envelope's tenant_id when the
// payload has none. Anything else (flat JSON, or invalid JSON for the caller to reject) is
// returned unchanged.
//
// An envelope is recognised by its event_type. A flat message may carry its own "payload"
// field (crm.agent.run_requested is {run_id, agent_id, tenant_id, payload}); treating that
// as an envelope would drop run_id and turn every custom-agent run into a poison message.
func eventBody(data []byte) []byte {
	var env struct {
		EventType string          `json:"event_type"`
		TenantID  string          `json:"tenant_id"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &env); err != nil || env.EventType == "" || len(env.Payload) == 0 || env.Payload[0] != '{' {
		return data
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(env.Payload, &fields); err != nil {
		return data
	}
	if _, ok := fields["tenant_id"]; !ok && env.TenantID != "" {
		fields["tenant_id"] = json.RawMessage(strconv.Quote(env.TenantID))
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return data
	}
	return out
}
