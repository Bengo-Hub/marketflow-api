// Package leadresponse holds the lead response rules (owner, 2026-11-24): the automatic follow-up
// every tenant gets by default, the team alert for each new lead, the reminders for leads nobody has
// contacted yet (counted in business hours) and the daily list of uncontacted leads.
//
// Every value has a platform default in service_config (tenant_id NULL) and a tenant override in
// Tenant.metadata.lead_response, set by a tenant admin on the Settings page. The code keeps only
// the documented fallbacks below for a platform row that is missing or unreadable.
package leadresponse

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// TenantKey is the tenant metadata object holding the tenant's overrides:
//
//	{"follow_up": bool, "reminder_minutes": [15, 120, 1440], "business_days": [1, 2, 3, 4, 5, 6],
//	 "business_hours": {"open": "08:00", "close": "18:00"}, "daily_list": bool}
//
// Every key is optional; a missing key uses the platform default. Days count from 0 (Sunday).
const TenantKey = "lead_response"

// Platform defaults in service_config (tenant_id NULL).
const (
	// KeyFollowUp: "true" or "false", whether a tenant gets the automatic follow-up when it set nothing.
	KeyFollowUp = "lead_response.follow_up"
	// KeyReminders: the reminder steps for an uncontacted lead, as durations of business time
	// ("15m,2h,24h").
	KeyReminders = "lead_response.reminders"
	// KeyBusinessDays: the open days, as numbers from 0 (Sunday) or names ("1,2,3,4,5,6" or
	// "mon,tue,wed,thu,fri,sat").
	KeyBusinessDays = "lead_response.business_days"
	// KeyBusinessHours: the opening hours in the tenant's time zone ("08:00-18:00").
	KeyBusinessHours = "lead_response.business_hours"
	// KeyDailyList: "true" or "false", whether the morning email lists every uncontacted lead.
	KeyDailyList = "lead_response.daily_list"
	// KeyAlertWindow: how long the leads after the first in a burst wait to go out together in one
	// team alert ("15m"). Platform only.
	KeyAlertWindow = "lead_response.alert_window"
	// KeyStartedAt: when lead response went live (RFC 3339), written once. Leads created before it
	// are never followed up or reminded about.
	KeyStartedAt = "lead_response.started_at"
)

// Settings are one tenant's effective lead response rules.
type Settings struct {
	FollowUp bool
	// Reminders are business-time offsets from the lead's arrival, ascending.
	Reminders []time.Duration
	// Days marks the open weekdays, indexed by time.Weekday.
	Days [7]bool
	// Open and Close are minutes after local midnight; Open < Close.
	Open, Close int
	DailyList   bool
	AlertWindow time.Duration
	// Location is the tenant's time zone (postingplan.Location); nil reads as UTC.
	Location *time.Location
}

// Fallback is what applies when the platform row is missing or unreadable: follow-up on, reminders
// at 15 minutes, 2 hours and 24 hours, Monday to Saturday 08:00 to 18:00, daily list on, 15 minute
// alert window.
func Fallback() Settings {
	return Settings{
		FollowUp:    true,
		Reminders:   []time.Duration{15 * time.Minute, 2 * time.Hour, 24 * time.Hour},
		Days:        [7]bool{false, true, true, true, true, true, true},
		Open:        8 * 60,
		Close:       18 * 60,
		DailyList:   true,
		AlertWindow: 15 * time.Minute,
	}
}

// Limits on what a reminder list may hold.
const (
	maxReminders       = 5
	maxReminderMinutes = 7 * 24 * 60
)

// Platform reads the platform defaults from service_config over Fallback.
func Platform(ctx context.Context, db *ent.Client) Settings {
	s := Fallback()
	val := func(k string) string { return svcconfig.Value(ctx, db, uuid.Nil, k) }
	if b, err := strconv.ParseBool(val(KeyFollowUp)); err == nil {
		s.FollowUp = b
	}
	if r, ok := parseReminderList(val(KeyReminders)); ok {
		s.Reminders = r
	}
	if d, ok := parseDayList(val(KeyBusinessDays)); ok {
		s.Days = d
	}
	if o, c, ok := parseHourRange(val(KeyBusinessHours)); ok {
		s.Open, s.Close = o, c
	}
	if b, err := strconv.ParseBool(val(KeyDailyList)); err == nil {
		s.DailyList = b
	}
	if d, ok := svcconfig.ParseDuration(val(KeyAlertWindow), time.Minute, 24*time.Hour); ok {
		s.AlertWindow = d
	}
	return s
}

// Merge applies a tenant's metadata overrides to the platform settings. Invalid tenant values are
// ignored (the profile PATCH refuses them, so only a hand-edited row can hold one).
func Merge(platform Settings, tenantMeta map[string]any, loc *time.Location) Settings {
	s := platform
	s.Reminders = append([]time.Duration(nil), platform.Reminders...)
	s.Location = loc
	o, _ := tenantMeta[TenantKey].(map[string]any)
	if o == nil {
		return s
	}
	if b, ok := o["follow_up"].(bool); ok {
		s.FollowUp = b
	}
	if mins, ok := intList(o["reminder_minutes"]); ok && validReminderMinutes(mins) == nil {
		s.Reminders = s.Reminders[:0]
		for _, m := range mins {
			s.Reminders = append(s.Reminders, time.Duration(m)*time.Minute)
		}
	}
	if days, ok := intList(o["business_days"]); ok && validDays(days) == nil {
		s.Days = [7]bool{}
		for _, d := range days {
			s.Days[d] = true
		}
	}
	if bh, ok := o["business_hours"].(map[string]any); ok {
		op, _ := bh["open"].(string)
		cl, _ := bh["close"].(string)
		if a, b, ok := parseHourRange(op + "-" + cl); ok {
			s.Open, s.Close = a, b
		}
	}
	if b, ok := o["daily_list"].(bool); ok {
		s.DailyList = b
	}
	return s
}

// Load is one tenant's effective settings and the platform defaults they were built on.
func Load(ctx context.Context, db *ent.Client, t *ent.Tenant) (effective, platform Settings) {
	platform = Platform(ctx, db)
	var meta map[string]any
	if t != nil {
		meta = t.Metadata
	}
	return Merge(platform, meta, TenantLocation(t)), platform
}

// TenantLocation is the tenant's time zone, Africa/Nairobi when unset or unknown (the same rule
// as postingplan.Location, kept here so this package depends on nothing above ent).
func TenantLocation(t *ent.Tenant) *time.Location {
	if t != nil && t.Timezone != nil && *t.Timezone != "" {
		if loc, err := time.LoadLocation(*t.Timezone); err == nil {
			return loc
		}
	}
	if loc, err := time.LoadLocation("Africa/Nairobi"); err == nil {
		return loc
	}
	return time.FixedZone("EAT", 3*3600)
}

// View is the JSON form of the settings the Settings page shows and edits.
type View struct {
	FollowUp        bool      `json:"follow_up"`
	ReminderMinutes []int     `json:"reminder_minutes"`
	BusinessDays    []int     `json:"business_days"`
	BusinessHours   HoursView `json:"business_hours"`
	DailyList       bool      `json:"daily_list"`
}

// HoursView is the opening hours as "HH:MM".
type HoursView struct {
	Open  string `json:"open"`
	Close string `json:"close"`
}

// ToView renders the settings for the API.
func (s Settings) ToView() View {
	v := View{FollowUp: s.FollowUp, DailyList: s.DailyList, ReminderMinutes: []int{}, BusinessDays: []int{},
		BusinessHours: HoursView{Open: hhmm(s.Open), Close: hhmm(s.Close)}}
	for _, r := range s.Reminders {
		v.ReminderMinutes = append(v.ReminderMinutes, int(r/time.Minute))
	}
	for d, on := range s.Days {
		if on {
			v.BusinessDays = append(v.BusinessDays, d)
		}
	}
	return v
}

// Validate checks and normalises metadata.lead_response in a profile update. Null clears the
// tenant's overrides; an unknown key or a bad value is refused.
func Validate(meta map[string]any) error {
	v, ok := meta[TenantKey]
	if !ok || v == nil {
		return nil
	}
	in, isMap := v.(map[string]any)
	if !isMap {
		return errors.New("lead_response must be an object")
	}
	out := map[string]any{}
	for k, raw := range in {
		if raw == nil {
			continue
		}
		switch k {
		case "follow_up", "daily_list":
			b, isBool := raw.(bool)
			if !isBool {
				return fmt.Errorf("lead_response.%s must be true or false", k)
			}
			out[k] = b
		case "reminder_minutes":
			mins, ok := intList(raw)
			if !ok {
				return errors.New("lead_response.reminder_minutes must be a list of whole minutes")
			}
			if err := validReminderMinutes(mins); err != nil {
				return err
			}
			out[k] = toAnyInts(mins)
		case "business_days":
			days, ok := intList(raw)
			if !ok {
				return errors.New("lead_response.business_days must be a list of days from 0 (Sunday) to 6 (Saturday)")
			}
			if err := validDays(days); err != nil {
				return err
			}
			sort.Ints(days)
			out[k] = toAnyInts(days)
		case "business_hours":
			bh, isMap := raw.(map[string]any)
			if !isMap {
				return errors.New("lead_response.business_hours must have open and close times")
			}
			op, _ := bh["open"].(string)
			cl, _ := bh["close"].(string)
			a, b, ok := parseHourRange(op + "-" + cl)
			if !ok {
				return errors.New("lead_response.business_hours must be HH:MM times with open before close")
			}
			out[k] = map[string]any{"open": hhmm(a), "close": hhmm(b)}
		default:
			return fmt.Errorf("lead_response.%s is not a known setting", k)
		}
	}
	if len(out) == 0 {
		meta[TenantKey] = nil
		return nil
	}
	meta[TenantKey] = out
	return nil
}

// Changed reports whether the normalised value v differs from the stored one.
func Changed(stored map[string]any, v any) bool {
	old, _ := stored[TenantKey].(map[string]any)
	next, _ := v.(map[string]any)
	if len(old) == 0 && len(next) == 0 {
		return false
	}
	return !reflect.DeepEqual(normalJSON(old), normalJSON(next))
}

// normalJSON turns numbers into float64, the way a stored JSON value reads back, so a fresh
// value compares equal to the stored one.
func normalJSON(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		switch x := v.(type) {
		case []any:
			list := make([]any, len(x))
			for i, e := range x {
				if n, ok := toInt(e); ok {
					list[i] = float64(n)
				} else {
					list[i] = e
				}
			}
			out[k] = list
		default:
			out[k] = v
		}
	}
	return out
}

func validReminderMinutes(mins []int) error {
	if len(mins) == 0 || len(mins) > maxReminders {
		return fmt.Errorf("lead_response.reminder_minutes needs 1 to %d reminders", maxReminders)
	}
	prev := 0
	for _, m := range mins {
		if m < 1 || m > maxReminderMinutes {
			return errors.New("each reminder must be between 1 minute and 7 days")
		}
		if m <= prev {
			return errors.New("reminders must be in increasing order")
		}
		prev = m
	}
	return nil
}

func validDays(days []int) error {
	if len(days) == 0 {
		return errors.New("lead_response.business_days needs at least one day")
	}
	seen := map[int]bool{}
	for _, d := range days {
		if d < 0 || d > 6 || seen[d] {
			return errors.New("lead_response.business_days must list days 0 (Sunday) to 6 (Saturday) once each")
		}
		seen[d] = true
	}
	return nil
}

func parseReminderList(v string) ([]time.Duration, bool) {
	if strings.TrimSpace(v) == "" {
		return nil, false
	}
	var out []time.Duration
	var prev time.Duration
	for _, p := range strings.Split(v, ",") {
		d, ok := svcconfig.ParseDuration(p, time.Minute, maxReminderMinutes*time.Minute)
		if !ok || d <= prev {
			return nil, false
		}
		out = append(out, d)
		prev = d
	}
	if len(out) > maxReminders {
		return nil, false
	}
	return out, true
}

var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

func parseDayList(v string) ([7]bool, bool) {
	var days [7]bool
	any := false
	for _, p := range strings.Split(v, ",") {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		d, ok := dayNames[p[:min(3, len(p))]]
		if !ok {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 6 {
				return [7]bool{}, false
			}
			d = n
		}
		days[d] = true
		any = true
	}
	return days, any
}

// parseHourRange reads "HH:MM-HH:MM" into minutes after midnight, open before close.
func parseHourRange(v string) (int, int, bool) {
	parts := strings.Split(strings.TrimSpace(v), "-")
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, ok1 := parseHHMM(parts[0])
	b, ok2 := parseHHMM(parts[1])
	if !ok1 || !ok2 || a >= b {
		return 0, 0, false
	}
	return a, b, true
}

func parseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) != 5 {
		return 0, false
	}
	if s == "24:00" {
		return 24 * 60, true
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, false
	}
	return t.Hour()*60 + t.Minute(), true
}

func hhmm(m int) string { return fmt.Sprintf("%02d:%02d", m/60, m%60) }

func intList(v any) ([]int, bool) {
	list, ok := v.([]any)
	if !ok {
		if ints, isInts := v.([]int); isInts {
			return append([]int(nil), ints...), true
		}
		return nil, false
	}
	out := make([]int, 0, len(list))
	for _, e := range list {
		n, ok := toInt(e)
		if !ok {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	}
	return 0, false
}

func toAnyInts(in []int) []any {
	out := make([]any, len(in))
	for i, n := range in {
		out[i] = n
	}
	return out
}
