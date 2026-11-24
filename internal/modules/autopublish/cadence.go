package autopublish

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
)

// cadencePlan is the posting plan for an agent that follows the tenant's cadence: the platforms
// still short of their posts this week (or month) and each one's best-time slot at or after
// earliest. why is set when no platform needs a post, so the draft waits for the owner.
func (p *Pipeline) cadencePlan(ctx context.Context, tenantID uuid.UUID, want []string, earliest time.Time) ([]string, map[string]time.Time, string) {
	s := postingplan.Load(ctx, p.DB, tenantID)
	booked, err := postingplan.Booked(ctx, p.DB, tenantID, want, postingplan.BookedSince(earliest, s.Loc))
	if err != nil {
		// The plan could not be read: post as before rather than hold a finished video.
		return want, nil, ""
	}
	plan := s.Plan(want, booked, earliest)
	if len(plan) == 0 {
		return nil, nil, "every platform already has its posts for this week or month under your posting cadence"
	}
	platforms := make([]string, 0, len(plan))
	for _, pl := range want {
		if _, ok := plan[pl]; ok {
			platforms = append(platforms, pl)
		}
	}
	return platforms, plan, ""
}

// earliest is the first time in m, or def when m is empty.
func earliest(m map[string]time.Time, def time.Time) time.Time {
	out := time.Time{}
	for _, t := range m {
		if out.IsZero() || t.Before(out) {
			out = t
		}
	}
	if out.IsZero() {
		return def
	}
	return out.UTC().Truncate(time.Second)
}

// latest is the last time in m, or def when m is empty or all earlier.
func latest(m map[string]time.Time, def time.Time) time.Time {
	out := def
	for _, t := range m {
		if t.After(out) {
			out = t
		}
	}
	return out
}
