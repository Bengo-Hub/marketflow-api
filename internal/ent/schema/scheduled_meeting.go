package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// ScheduledMeeting holds the schema definition for the ScheduledMeeting entity.
// Represents a virtual meeting booked via the public booking widget, the AI
// agent, or manual scheduling.
type ScheduledMeeting struct {
	ent.Schema
}

// Fields of the ScheduledMeeting.
func (ScheduledMeeting) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}).
			Comment("Owning tenant"),

		field.UUID("lead_id", uuid.UUID{}).
			Optional().
			Nillable(),
		field.UUID("contact_id", uuid.UUID{}).
			Optional().
			Nillable(),

		field.String("external_booking_id").
			Optional().
			Nillable().
			Comment("Booking ID from an external scheduling system, if any"),
		field.String("meeting_url").
			Optional().
			Nillable().
			Comment("Google Meet / Zoom URL"),

		// Google Calendar sync fields — populated when we push the booking to
		// the tenant's Google Calendar via the gcal integration.
		field.String("google_event_id").
			Optional().
			Nillable().
			Comment("Google Calendar event ID for upsert/delete"),
		field.String("google_calendar_id").
			Optional().
			Nillable().
			Comment("Calendar into which the event was created (default: 'primary')"),
		field.String("google_event_link").
			Optional().
			Nillable().
			Comment("htmlLink returned by Google Calendar (shareable)"),

		field.String("attendee_email").
			Optional().
			Nillable().
			Comment("Primary attendee email (lead) — invited via calendar"),
		field.String("attendee_name").
			Optional().
			Nillable(),
		field.String("organizer_email").
			Optional().
			Nillable().
			Comment("FBO/tenant organizer email"),

		field.Time("scheduled_at"),
		field.Int("duration_minutes").
			Default(30),

		field.String("meeting_type").
			Default("product_demo").
			Comment("product_demo | business_opportunity | follow_up | onboarding"),
		field.String("status").
			Default("scheduled").
			Comment("scheduled | completed | no_show | cancelled | rescheduled"),

		field.Text("notes").
			Optional().
			Nillable().
			Comment("Meeting notes or agenda"),

		field.JSON("metadata", map[string]any{}).
			Optional(),

		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Edges of the ScheduledMeeting.
func (ScheduledMeeting) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("lead", Lead.Type).
			Ref("scheduled_meetings").
			Field("lead_id").
			Unique(),
		edge.From("contact", Contact.Type).
			Ref("scheduled_meetings").
			Field("contact_id").
			Unique(),
	}
}

// Indexes of the ScheduledMeeting.
func (ScheduledMeeting) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("tenant_id", "status"),
		index.Fields("tenant_id", "scheduled_at"),
		index.Fields("tenant_id", "meeting_type"),
		index.Fields("external_booking_id"),
		index.Fields("google_event_id"),
		// Lead timelines and the daily brief look meetings up by lead.
		index.Fields("lead_id"),
	}
}
