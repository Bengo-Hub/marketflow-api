package gcal

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"go.uber.org/zap"
)

// ErrNotConnected is returned when the tenant hasn't linked a Google Calendar
// (or the platform OAuth client isn't configured at all).
var ErrNotConnected = errors.New("gcal: tenant has not connected a Google Calendar")

// Slot is a single bookable window.
type Slot struct {
	Start time.Time
	End   time.Time
}

// BusinessHours configures the bookable window for a single day.
type BusinessHours struct {
	StartHour   int
	EndHour     int
	SlotMinutes int
	Weekdays    []time.Weekday
}

// DefaultBusinessHours is the fallback used when a tenant hasn't overridden
// their scheduling config: 9am-5pm, Monday-Friday, 30-minute slots.
func DefaultBusinessHours() BusinessHours {
	return BusinessHours{
		StartHour:   9,
		EndHour:     17,
		SlotMinutes: 30,
		Weekdays: []time.Weekday{
			time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday,
		},
	}
}

// AvailabilityService computes open booking slots for a tenant from their
// linked Google Calendar's free/busy data.
type AvailabilityService struct {
	db     *ent.Client
	logger *zap.Logger
}

// NewAvailabilityService constructs an AvailabilityService.
func NewAvailabilityService(db *ent.Client, logger *zap.Logger) *AvailabilityService {
	return &AvailabilityService{db: db, logger: logger.Named("gcal.availability")}
}

// ComputeSlots returns the open slots for tenantID on the given date (in the
// given IANA timezone). Returns ErrNotConnected if the tenant hasn't linked a
// Google Calendar.
func (s *AvailabilityService) ComputeSlots(ctx context.Context, tenantID string, date time.Time, timezone string) ([]Slot, error) {
	platform, err := loadPlatformCreds(ctx, s.db)
	if err != nil {
		return nil, ErrNotConnected
	}
	tenant, err := loadTenantCreds(ctx, s.db, tenantID)
	if err != nil {
		return nil, ErrNotConnected
	}

	hours, err := s.loadBusinessHours(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	dateLocal := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)

	if !weekdayAllowed(dateLocal.Weekday(), hours.Weekdays) {
		return []Slot{}, nil
	}

	dayStart := time.Date(dateLocal.Year(), dateLocal.Month(), dateLocal.Day(), hours.StartHour, 0, 0, 0, loc)
	dayEnd := time.Date(dateLocal.Year(), dateLocal.Month(), dateLocal.Day(), hours.EndHour, 0, 0, 0, loc)

	cli := New(platform, tenant)
	busy, err := cli.FreeBusy(ctx, dayStart.UTC(), dayEnd.UTC())
	if err != nil {
		return nil, err
	}

	slotDur := time.Duration(hours.SlotMinutes) * time.Minute
	now := time.Now()
	slots := []Slot{}
	for t := dayStart; !t.Add(slotDur).After(dayEnd); t = t.Add(slotDur) {
		slotEnd := t.Add(slotDur)
		if t.Before(now) {
			continue
		}
		if overlapsAny(t, slotEnd, busy) {
			continue
		}
		slots = append(slots, Slot{Start: t, End: slotEnd})
	}
	return slots, nil
}

func weekdayAllowed(day time.Weekday, allowed []time.Weekday) bool {
	for _, d := range allowed {
		if d == day {
			return true
		}
	}
	return false
}

func overlapsAny(start, end time.Time, busy []BusyInterval) bool {
	for _, b := range busy {
		if start.Before(b.End) && end.After(b.Start) {
			return true
		}
	}
	return false
}

// loadBusinessHours reads per-tenant overrides from the same ProviderSetting
// group used for the tenant's Google Calendar OAuth creds, falling back to
// DefaultBusinessHours() for anything unset.
func (s *AvailabilityService) loadBusinessHours(ctx context.Context, tenantID string) (BusinessHours, error) {
	hours := DefaultBusinessHours()
	rows, err := s.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ("google_calendar"),
			providersetting.ProviderNameEQ("oauth"),
		).
		All(ctx)
	if err != nil {
		return hours, err
	}
	for _, r := range rows {
		switch r.Key {
		case "business_hours_start":
			if v, err := strconv.Atoi(r.Value); err == nil {
				hours.StartHour = v
			}
		case "business_hours_end":
			if v, err := strconv.Atoi(r.Value); err == nil {
				hours.EndHour = v
			}
		case "slot_minutes":
			if v, err := strconv.Atoi(r.Value); err == nil && v > 0 {
				hours.SlotMinutes = v
			}
		case "business_days":
			if days := parseWeekdays(r.Value); len(days) > 0 {
				hours.Weekdays = days
			}
		}
	}
	return hours, nil
}

// parseWeekdays parses a comma-separated list of weekday numbers (0=Sunday .. 6=Saturday).
func parseWeekdays(s string) []time.Weekday {
	var out []time.Weekday
	for _, p := range strings.Split(s, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 || n > 6 {
			continue
		}
		out = append(out, time.Weekday(n))
	}
	return out
}
