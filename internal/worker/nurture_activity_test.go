package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent/activity"
)

// Sprint 23 L29: every automated nurture message is an Activity on the lead's timeline.
func TestLogNurtureActivity(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tenantID, leadID := uuid.New(), uuid.New()
	db.Tenant.Create().SetID(tenantID).SetName("nurture " + tenantID.String()).SetSlug("nurture-" + tenantID.String()).SaveX(ctx)
	err := logNurtureActivity(ctx, db, tenantID, leadID, "whatsapp", "", strings.Repeat("hi ", 2000), "nurture-x-0",
		map[string]any{"sequence_id": "s1", "step_index": 0})
	if err != nil {
		t.Fatal(err)
	}
	a := db.Activity.Query().Where(activity.TenantID(tenantID), activity.EntityID(leadID)).OnlyX(ctx)
	if a.EntityType != "lead" || a.ActivityType != "whatsapp" || a.Direction == nil || *a.Direction != "outbound" {
		t.Fatalf("activity: %+v", a)
	}
	if a.Body == nil || len(*a.Body) != 4000 {
		t.Fatalf("body capped at 4000, got %v", a.Body)
	}
	if a.ExternalRef == nil || *a.ExternalRef != "nurture-x-0" {
		t.Fatalf("external ref: %v", a.ExternalRef)
	}
}
