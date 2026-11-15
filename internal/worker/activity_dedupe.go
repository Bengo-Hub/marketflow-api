package worker

import (
	"context"
	"encoding/json"

	"entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqljson"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
)

// activityKey identifies the Activity one consumed event writes on a CRM timeline. The event id
// always counts; when externalRef is set, an entry with the same activity type, source and
// external_ref on the same record counts too, so a producer that re-publishes the same order or
// payment under a fresh envelope still lands once.
type activityKey struct {
	tenantID     uuid.UUID
	entity       string
	entityID     uuid.UUID
	eventID      string
	activityType string
	source       string
	externalRef  string
}

// eventIDOf returns the shared-events envelope id of a message. A message without one (a bare
// payload) gets a stable id hashed from its bytes, so a redelivery of the same bytes matches.
func eventIDOf(data []byte) string {
	var env struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(data, &env) == nil {
		if id, err := uuid.Parse(env.ID); err == nil && id != uuid.Nil {
			return id.String()
		}
	}
	return uuid.NewSHA1(uuid.NameSpaceOID, data).String()
}

// eventMetadata is the Activity metadata that records which event wrote it.
func eventMetadata(eventID, subject string) map[string]any {
	return map[string]any{activityEventIDKey: eventID, "event_subject": subject}
}

// logActivityOnce writes in through the activities service unless the record's timeline already
// holds it, keyed as activityKey describes (eventID plus in's type, source and external_ref). It
// records eventID in the Activity's metadata and reports whether a new row was written.
func logActivityOnce(ctx context.Context, db *ent.Client, svc *activities.Service, eventID, subject string, in activities.CreateInput) (bool, error) {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	done, err := activityLogged(ctx, db, activityKey{
		tenantID: in.TenantID, entity: in.EntityType, entityID: in.EntityID, eventID: eventID,
		activityType: in.ActivityType, source: str(in.Source), externalRef: str(in.ExternalRef),
	})
	if err != nil || done {
		return false, err
	}
	meta := eventMetadata(eventID, subject)
	for k, v := range in.Metadata {
		meta[k] = v
	}
	in.Metadata = meta
	if _, err := svc.Log(ctx, in); err != nil {
		return false, err
	}
	return true, nil
}

// activityLogged reports whether the record's timeline already holds the Activity for k. The
// tenant and entity index keeps the lookup to one record's timeline.
func activityLogged(ctx context.Context, db *ent.Client, k activityKey) (bool, error) {
	byEvent := predicate.Activity(func(s *sql.Selector) {
		s.Where(sqljson.ValueEQ(activity.FieldMetadata, k.eventID, sqljson.Path(activityEventIDKey)))
	})
	match := byEvent
	if k.externalRef != "" {
		match = activity.Or(byEvent, activity.And(
			activity.ActivityType(k.activityType),
			activity.Source(k.source),
			activity.ExternalRef(k.externalRef),
		))
	}
	return db.Activity.Query().
		Where(
			activity.TenantID(k.tenantID),
			activity.EntityType(k.entity),
			activity.EntityID(k.entityID),
			match,
		).
		Exist(ctx)
}
