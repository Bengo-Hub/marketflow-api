// Package postingplan is the one source of when and how often a tenant posts on each platform:
// the best-time windows and the cadence (posts a week or a month). The publish dialog's "best
// time", the calendar's advice and the autopilot all read it here.
//
// Layers, each overriding the one before per platform:
//  1. code defaults (DefaultWindows and DefaultCadence, researched; sources in the MarketFlow
//     docs, guides/posting-times-and-cadence.md),
//  2. the platform's service_config rows posting.best_times and posting.cadence (JSON, editable
//     on the platform service config page),
//  3. the tenant's own choice in Tenant.metadata posting_times and posting_cadence.
//
// Windows are wall-clock times in the tenant's time zone (Africa/Nairobi when unset), so a
// window means the same local hour all year, daylight saving or not.
package postingplan

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// Platforms that take an organic post (publishing.Facebook and the rest).
var Platforms = []string{"facebook", "instagram", "tiktok", "youtube"}

// Config keys.
const (
	PlatformTimesKey   = "posting.best_times" // service_config, platform row
	PlatformCadenceKey = "posting.cadence"    // service_config, platform row
	TenantTimesKey     = "posting_times"      // Tenant.metadata
	TenantCadenceKey   = "posting_cadence"    // Tenant.metadata
)

// Day sets a window can name, besides a comma list of mon..sun.
const (
	DaysWeekdays = "weekdays"
	DaysWeekend  = "weekend"
	DaysDaily    = "daily"
)

// Cadence periods.
const (
	PerWeek  = "week"
	PerMonth = "month"
)

// Limits on what a tenant may set.
const (
	MaxWindowsPerPlatform = 8
	MaxPostsPerWeek       = 21
	MaxPostsPerMonth      = 90
)

// Window is a span of local time on some days: Start inclusive, End exclusive, "HH:MM".
type Window struct {
	Days  string `json:"days"`
	Start string `json:"start"`
	End   string `json:"end"`
}

// Cadence is how many posts a platform gets per week or per month.
type Cadence struct {
	Count int    `json:"count"`
	Per   string `json:"per"`
}

// Layered sources reported with a platform's settings.
const (
	SourceDefault  = "default"
	SourcePlatform = "platform"
	SourceTenant   = "tenant"
)

// Platform is one platform's effective settings.
type Platform struct {
	Windows       []Window `json:"windows"`
	WindowsSource string   `json:"windows_source"`
	Cadence       Cadence  `json:"cadence"`
	CadenceSource string   `json:"cadence_source"`
}

// Settings are a tenant's effective settings.
type Settings struct {
	Timezone  string              `json:"timezone"`
	Loc       *time.Location      `json:"-"`
	Platforms map[string]Platform `json:"platforms"`
}

// DefaultWindows are the researched windows, in local time (Sprout Social 2026 Content Benchmarks
// data, Buffer's 52 million post study, Hootsuite 2025; adapted for East Africa's lunch hour and
// evening phone use).
func DefaultWindows() map[string][]Window {
	return map[string][]Window{
		"facebook": {
			{DaysWeekdays, "12:00", "14:00"},
			{DaysWeekdays, "19:00", "21:00"},
			{DaysWeekend, "10:00", "12:00"},
		},
		"instagram": {
			{DaysWeekdays, "12:00", "14:00"},
			{DaysWeekdays, "18:00", "21:00"},
			{DaysWeekend, "10:00", "12:00"},
		},
		"tiktok": {
			{DaysWeekdays, "15:00", "18:00"},
			{DaysWeekdays, "19:00", "22:00"},
			{DaysWeekend, "09:00", "13:00"},
		},
		"youtube": {
			{DaysWeekdays, "15:00", "18:00"},
			{DaysWeekend, "09:00", "12:00"},
		},
	}
}

// DefaultCadence is a small business's sustainable rhythm: every post is a made video that costs
// AI credits, so these sit at the low end of the published advice (Hootsuite 2025: Instagram and
// TikTok 3 to 5 a week).
func DefaultCadence() map[string]Cadence {
	return map[string]Cadence{
		"facebook":  {3, PerWeek},
		"instagram": {3, PerWeek},
		"tiktok":    {3, PerWeek},
		"youtube":   {2, PerWeek},
	}
}

var dayNames = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// minutes reads "HH:MM" as minutes after midnight.
func minutes(s string) (int, bool) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

// covers reports whether the window's days include d.
func (w Window) covers(d time.Weekday) bool {
	switch strings.ToLower(strings.TrimSpace(w.Days)) {
	case DaysDaily:
		return true
	case DaysWeekdays:
		return d >= time.Monday && d <= time.Friday
	case DaysWeekend:
		return d == time.Saturday || d == time.Sunday
	}
	for _, part := range strings.Split(strings.ToLower(w.Days), ",") {
		if wd, ok := dayNames[strings.TrimSpace(part)]; ok && wd == d {
			return true
		}
	}
	return false
}

// Validate says what is wrong with a window.
func (w Window) Validate() error {
	days := strings.ToLower(strings.TrimSpace(w.Days))
	switch days {
	case DaysDaily, DaysWeekdays, DaysWeekend:
	default:
		if days == "" {
			return errors.New("a window needs its days")
		}
		for _, part := range strings.Split(days, ",") {
			if _, ok := dayNames[strings.TrimSpace(part)]; !ok {
				return fmt.Errorf("unknown day %q (use weekdays, weekend, daily or mon..sun)", strings.TrimSpace(part))
			}
		}
	}
	s, ok1 := minutes(w.Start)
	e, ok2 := minutes(w.End)
	if !ok1 || !ok2 {
		return errors.New("window times are HH:MM")
	}
	if e <= s {
		return fmt.Errorf("window %s to %s ends before it starts", w.Start, w.End)
	}
	return nil
}

// Validate says what is wrong with a cadence.
func (c Cadence) Validate() error {
	switch c.Per {
	case PerWeek:
		if c.Count < 0 || c.Count > MaxPostsPerWeek {
			return fmt.Errorf("posts a week are 0 to %d", MaxPostsPerWeek)
		}
	case PerMonth:
		if c.Count < 0 || c.Count > MaxPostsPerMonth {
			return fmt.Errorf("posts a month are 0 to %d", MaxPostsPerMonth)
		}
	default:
		return errors.New(`cadence per is "week" or "month"`)
	}
	return nil
}

// ParseWindows reads a platform -> windows map (from JSON text or decoded JSON) and validates it.
// A platform with no windows is dropped (it falls back to the layer below).
func ParseWindows(v any) (map[string][]Window, error) {
	raw, err := asJSON(v)
	if err != nil || raw == nil {
		return nil, err
	}
	var m map[string][]Window
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("posting times are a map of platform to a list of {days, start, end}")
	}
	out := map[string][]Window{}
	for p, ws := range m {
		if !slices.Contains(Platforms, p) {
			return nil, fmt.Errorf("unknown platform %q", p)
		}
		if len(ws) > MaxWindowsPerPlatform {
			return nil, fmt.Errorf("%s: at most %d windows", p, MaxWindowsPerPlatform)
		}
		for _, w := range ws {
			if err := w.Validate(); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
		}
		if len(ws) > 0 {
			out[p] = ws
		}
	}
	return out, nil
}

// ParseCadence reads a platform -> cadence map and validates it.
func ParseCadence(v any) (map[string]Cadence, error) {
	raw, err := asJSON(v)
	if err != nil || raw == nil {
		return nil, err
	}
	var m map[string]Cadence
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("posting cadence is a map of platform to {count, per}")
	}
	for p, c := range m {
		if !slices.Contains(Platforms, p) {
			return nil, fmt.Errorf("unknown platform %q", p)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	return m, nil
}

func asJSON(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(x) == "" {
			return nil, nil
		}
		return []byte(x), nil
	case []byte:
		return x, nil
	}
	return json.Marshal(v)
}

// Merge layers the settings: code defaults, then the platform's values, then the tenant's. A
// layer that does not parse is ignored as a whole (the one below stands), so a bad platform row
// never breaks posting.
func Merge(platformTimes, platformCadence, tenantTimes, tenantCadence any, timezone string, loc *time.Location) Settings {
	s := Settings{Timezone: timezone, Loc: loc, Platforms: map[string]Platform{}}
	dw, dc := DefaultWindows(), DefaultCadence()
	for _, p := range Platforms {
		s.Platforms[p] = Platform{Windows: dw[p], WindowsSource: SourceDefault, Cadence: dc[p], CadenceSource: SourceDefault}
	}
	apply := func(times, cadence any, source string) {
		if ws, err := ParseWindows(times); err == nil {
			for p, w := range ws {
				pl := s.Platforms[p]
				pl.Windows, pl.WindowsSource = w, source
				s.Platforms[p] = pl
			}
		}
		if cs, err := ParseCadence(cadence); err == nil {
			for p, c := range cs {
				pl := s.Platforms[p]
				pl.Cadence, pl.CadenceSource = c, source
				s.Platforms[p] = pl
			}
		}
	}
	apply(platformTimes, platformCadence, SourcePlatform)
	apply(tenantTimes, tenantCadence, SourceTenant)
	return s
}

// searchDays bounds every window search: a week of windows always has a slot, unless the
// platform has none at all.
const searchDays = 21

// Next is the earliest instant at or after from inside one of the windows (from itself when it
// is inside one), in loc's wall-clock time. No windows returns from.
func Next(windows []Window, from time.Time, loc *time.Location) time.Time {
	if len(windows) == 0 || loc == nil {
		return from
	}
	local := from.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	for i := 0; i < searchDays; i++ {
		d := day.AddDate(0, 0, i)
		best := time.Time{}
		for _, w := range windows {
			if !w.covers(d.Weekday()) {
				continue
			}
			s, ok1 := minutes(w.Start)
			e, ok2 := minutes(w.End)
			if !ok1 || !ok2 || e <= s {
				continue
			}
			start := time.Date(d.Year(), d.Month(), d.Day(), s/60, s%60, 0, 0, loc)
			end := time.Date(d.Year(), d.Month(), d.Day(), e/60, e%60, 0, 0, loc)
			var t time.Time
			switch {
			case !from.After(start):
				t = start
			case from.Before(end):
				t = from
			default:
				continue
			}
			if best.IsZero() || t.Before(best) {
				best = t
			}
		}
		if !best.IsZero() {
			return best
		}
	}
	return from
}

// WeeklyTarget is a cadence's posts in one week: a monthly count is spread evenly over the month.
func WeeklyTarget(c Cadence, daysInMonth int) int {
	if c.Per == PerMonth {
		if daysInMonth <= 0 {
			daysInMonth = 30
		}
		return int(math.Ceil(float64(c.Count) * 7 / float64(daysInMonth)))
	}
	return c.Count
}

// WeekStart is Monday 00:00 of t's week in loc; MonthStart the 1st at 00:00.
func WeekStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	back := (int(l.Weekday()) + 6) % 7 // Monday 0 .. Sunday 6
	d := l.AddDate(0, 0, -back)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
}

func MonthStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc)
}

// daysIn is the number of days in t's month in loc.
func daysIn(t time.Time, loc *time.Location) int {
	return MonthStart(t, loc).AddDate(0, 1, -1).Day()
}

// Counts are a platform's posts in the week and the month of the given time.
type Counts struct {
	Week  int `json:"week"`
	Month int `json:"month"`
}

// Count counts the booked times falling in at's week and month.
func Count(booked []time.Time, at time.Time, loc *time.Location) Counts {
	ws, ms := WeekStart(at, loc), MonthStart(at, loc)
	we, me := ws.AddDate(0, 0, 7), ms.AddDate(0, 1, 0)
	var c Counts
	for _, b := range booked {
		if !b.Before(ws) && b.Before(we) {
			c.Week++
		}
		if !b.Before(ms) && b.Before(me) {
			c.Month++
		}
	}
	return c
}

// Due reports whether a platform still needs a post in at's week (and month, for a monthly
// cadence), given the posts already booked (scheduled or published).
func Due(c Cadence, booked []time.Time, at time.Time, loc *time.Location) bool {
	if c.Count <= 0 {
		return false
	}
	n := Count(booked, at, loc)
	if c.Per == PerMonth {
		return n.Month < c.Count && n.Week < WeeklyTarget(c, daysIn(at, loc))
	}
	return n.Week < c.Count
}

// localDay is the day number of t in loc (days since the epoch, by calendar date).
func localDay(t time.Time, loc *time.Location) int {
	l := t.In(loc)
	return int(time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// Slot is the next post time for a platform at or after from: inside a window, and spread so
// posts keep about 7 / weekly-target days apart (3 a week lands Monday, Wednesday, Friday
// rather than three days running). booked are the platform's other posts. When the spacing
// cannot be kept within the search, it is the next window instant.
func Slot(p Platform, from time.Time, loc *time.Location, booked []time.Time) time.Time {
	first := Next(p.Windows, from, loc)
	target := WeeklyTarget(p.Cadence, daysIn(from, loc))
	if target <= 0 || len(booked) == 0 || len(p.Windows) == 0 {
		return first
	}
	gap := 7 / target
	if gap < 1 {
		gap = 1
	}
	days := make([]int, 0, len(booked))
	for _, b := range booked {
		days = append(days, localDay(b, loc))
	}
	sort.Ints(days)
	t := first
	for i := 0; i < searchDays*4 && !t.IsZero(); i++ {
		d := localDay(t, loc)
		clash := false
		for _, b := range days {
			if abs(d-b) < gap {
				clash = true
				break
			}
		}
		if !clash {
			return t
		}
		// Try the next day's first window.
		l := t.In(loc)
		next := time.Date(l.Year(), l.Month(), l.Day()+1, 0, 0, 0, 0, loc)
		if next.Sub(from) > searchDays*24*time.Hour {
			break
		}
		t = Next(p.Windows, next, loc)
	}
	return first
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
