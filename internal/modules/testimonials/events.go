package testimonials

import (
	eventslib "github.com/Bengo-Hub/shared-events"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Testimonial event names. The outbox publishes on "<aggregate>.<event>", so a testimonial that
// becomes published goes out on marketflow.testimonial.published, inside the worker stream's
// marketflow.> binding (the autopilot testimonial trigger consumes it).
const (
	TestimonialAggregate      = "marketflow.testimonial"
	TestimonialPublishedEvent = "published"
	TestimonialPublishedTopic = TestimonialAggregate + "." + TestimonialPublishedEvent
)

// publishedEvent builds the event for a testimonial that has just become published. It carries no
// review text or author: consumers load the testimonial by id when they need it.
func publishedEvent(t *ent.Testimonial) *eventslib.Event {
	return eventslib.NewEvent(TestimonialPublishedEvent, TestimonialAggregate, t.ID, t.TenantID, map[string]any{
		"testimonial_id": t.ID.String(),
		"rating":         ratingOf(t),
	})
}

// becamePublished reports whether a save turned a testimonial published: a new published one, or
// an existing one switched from hidden to published. Edits to one already published do not count,
// so a typo fix never starts a new ad.
func becamePublished(wasPublished bool, t *ent.Testimonial) bool {
	return t.IsPublished && !wasPublished
}
