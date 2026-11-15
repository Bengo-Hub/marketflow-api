package worker

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

// Timeline events that cannot change a contact's profile never reach the enricher (its enr is nil
// here, so reaching it would panic): other entities, suggestions, and anything an agent wrote.
func TestContactEnricherSkipsActivitiesThatCannotChangeAProfile(t *testing.T) {
	h := &ContactEnricherHandler{subject: activities.ActivityLoggedTopic, logger: zap.NewNop()}
	for _, body := range []string{
		`{"event_type":"activity.logged","tenant_id":"6f1c2d0e-0000-4000-8000-000000000001","payload":{"entity_type":"lead","entity_id":"6f1c2d0e-0000-4000-8000-000000000002","activity_type":"call"}}`,
		`{"event_type":"activity.logged","tenant_id":"6f1c2d0e-0000-4000-8000-000000000001","payload":{"entity_type":"contact","entity_id":"6f1c2d0e-0000-4000-8000-000000000002","activity_type":"suggestion"}}`,
		`{"event_type":"activity.logged","tenant_id":"6f1c2d0e-0000-4000-8000-000000000001","payload":{"entity_type":"contact","entity_id":"6f1c2d0e-0000-4000-8000-000000000002","activity_type":"note","source":"agent"}}`,
	} {
		if err := h.Handle(context.Background(), fakeMsg{data: []byte(body)}); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}
