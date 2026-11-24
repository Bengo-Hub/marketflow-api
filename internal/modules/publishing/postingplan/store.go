package postingplan

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// DefaultTimezone is the zone of a tenant that set none. East Africa Time has no daylight saving.
const DefaultTimezone = "Africa/Nairobi"

// Location is the tenant's time zone, DefaultTimezone when unset or unknown.
func Location(t *ent.Tenant) (string, *time.Location) {
	if t != nil && t.Timezone != nil && *t.Timezone != "" {
		if loc, err := time.LoadLocation(*t.Timezone); err == nil {
			return *t.Timezone, loc
		}
	}
	if loc, err := time.LoadLocation(DefaultTimezone); err == nil {
		return DefaultTimezone, loc
	}
	return DefaultTimezone, time.FixedZone("EAT", 3*3600)
}

// Load reads a tenant's effective settings: one tenant read and two service_config reads.
func Load(ctx context.Context, db *ent.Client, tenantID uuid.UUID) Settings {
	pt := svcconfig.Value(ctx, db, uuid.Nil, PlatformTimesKey)
	pc := svcconfig.Value(ctx, db, uuid.Nil, PlatformCadenceKey)
	var tt, tc any
	t, err := db.Tenant.Get(ctx, tenantID)
	if err == nil {
		tt, tc = t.Metadata[TenantTimesKey], t.Metadata[TenantCadenceKey]
	} else {
		t = nil
	}
	tz, loc := Location(t)
	return Merge(pt, pc, tt, tc, tz, loc)
}

// live are the job states that count as a post: waiting, going out or out.
var live = []string{"pending", "uploading", "processing", "published"}

// Booked lists each platform's posts (scheduled or published) from since on, in one query.
// Bounded: a tenant posts a few times a day at most.
func Booked(ctx context.Context, db *ent.Client, tenantID uuid.UUID, platforms []string, since time.Time) (map[string][]time.Time, error) {
	out := map[string][]time.Time{}
	if len(platforms) == 0 {
		return out, nil
	}
	var rows []struct {
		Platform    string    `json:"platform"`
		ScheduledAt time.Time `json:"scheduled_at"`
	}
	err := db.PublishJob.Query().
		Where(
			publishjob.TenantIDEQ(tenantID),
			publishjob.PlatformIn(platforms...),
			publishjob.StatusIn(live...),
			publishjob.ScheduledAtGTE(since),
		).
		Limit(2000).
		Select(publishjob.FieldPlatform, publishjob.FieldScheduledAt).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		out[r.Platform] = append(out[r.Platform], r.ScheduledAt)
	}
	return out, nil
}

// BookedSince is where Booked should start for decisions at now: the earlier of the week's and
// the month's start, less a week so spacing sees the last posts of the week before.
func BookedSince(now time.Time, loc *time.Location) time.Time {
	ws, ms := WeekStart(now, loc), MonthStart(now, loc)
	if ms.Before(ws) {
		ws = ms
	}
	return ws.AddDate(0, 0, -7)
}

// DuePlatforms are the platforms among want that still need a post in now's week or month.
func (s Settings) DuePlatforms(want []string, booked map[string][]time.Time, now time.Time) []string {
	var out []string
	for _, p := range want {
		pl, ok := s.Platforms[p]
		if !ok || slices.Contains(out, p) {
			continue
		}
		if Due(pl.Cadence, booked[p], now, s.Loc) {
			out = append(out, p)
		}
	}
	return out
}

// Plan gives each due platform its post time at or after earliest: inside its windows and
// spaced from its other posts. Platforms whose cadence is met get none.
func (s Settings) Plan(want []string, booked map[string][]time.Time, earliest time.Time) map[string]time.Time {
	out := map[string]time.Time{}
	for _, p := range s.DuePlatforms(want, booked, earliest) {
		out[p] = Slot(s.Platforms[p], earliest, s.Loc, booked[p])
	}
	return out
}

// BestTime is a platform's next window instant at or after from (from for an unknown platform).
func (s Settings) BestTime(platform string, from time.Time) time.Time {
	pl, ok := s.Platforms[platform]
	if !ok {
		return from
	}
	return Next(pl.Windows, from, s.Loc)
}
