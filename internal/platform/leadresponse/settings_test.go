package leadresponse

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

func nairobi(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		t.Skipf("no tz data: %v", err)
	}
	return loc
}

func TestFallbackIsTheOwnerDefault(t *testing.T) {
	f := Fallback()
	if !f.FollowUp || !f.DailyList {
		t.Fatal("follow-up and daily list are on by default")
	}
	want := []time.Duration{15 * time.Minute, 2 * time.Hour, 24 * time.Hour}
	if len(f.Reminders) != 3 || f.Reminders[0] != want[0] || f.Reminders[1] != want[1] || f.Reminders[2] != want[2] {
		t.Fatalf("reminders %v, want %v", f.Reminders, want)
	}
	if f.Days[0] || !f.Days[1] || !f.Days[6] || f.Open != 480 || f.Close != 1080 {
		t.Fatalf("business hours are Mon-Sat 08:00-18:00, got %+v", f.ToView())
	}
}

func TestPlatformValueParsers(t *testing.T) {
	if r, ok := parseReminderList("10m, 1h,48h"); !ok || len(r) != 3 || r[2] != 48*time.Hour {
		t.Fatalf("reminders: %v %v", r, ok)
	}
	if _, ok := parseReminderList("2h,1h"); ok {
		t.Fatal("decreasing reminders must be refused")
	}
	if d, ok := parseDayList("mon,tue,wednesday"); !ok || !d[1] || !d[2] || !d[3] || d[0] {
		t.Fatalf("days by name: %v", d)
	}
	if d, ok := parseDayList("0,6"); !ok || !d[0] || !d[6] || d[1] {
		t.Fatalf("days by number: %v", d)
	}
	if o, c, ok := parseHourRange("07:30-17:00"); !ok || o != 450 || c != 1020 {
		t.Fatalf("hours: %d %d %v", o, c, ok)
	}
	if _, _, ok := parseHourRange("18:00-08:00"); ok {
		t.Fatal("close before open must be refused")
	}
}

func TestTenantOverrideWinsPerKey(t *testing.T) {
	var meta map[string]any
	_ = json.Unmarshal([]byte(`{"lead_response":{"follow_up":false,"reminder_minutes":[30,240],"business_days":[1,2,3,4,5],"business_hours":{"open":"09:00","close":"17:00"}}}`), &meta)
	s := Merge(Fallback(), meta, time.UTC)
	if s.FollowUp {
		t.Fatal("tenant switched the follow-up off")
	}
	if !s.DailyList {
		t.Fatal("daily list keeps the platform default when the tenant set nothing")
	}
	v := s.ToView()
	if len(v.ReminderMinutes) != 2 || v.ReminderMinutes[1] != 240 || len(v.BusinessDays) != 5 || v.BusinessHours.Open != "09:00" {
		t.Fatalf("merged view %+v", v)
	}
	// A bad stored value is ignored, never applied.
	_ = json.Unmarshal([]byte(`{"lead_response":{"reminder_minutes":[0],"business_days":[9]}}`), &meta)
	if got := Merge(Fallback(), meta, time.UTC).ToView(); len(got.ReminderMinutes) != 3 || len(got.BusinessDays) != 6 {
		t.Fatalf("invalid overrides must fall back: %+v", got)
	}
}

func TestValidateNormalisesAndRefuses(t *testing.T) {
	var meta map[string]any
	_ = json.Unmarshal([]byte(`{"lead_response":{"follow_up":false,"business_days":[6,1],"business_hours":{"open":"8:00","close":"18:00"},"daily_list":null}}`), &meta)
	if err := Validate(meta); err == nil {
		t.Fatal("8:00 is not HH:MM")
	}
	_ = json.Unmarshal([]byte(`{"lead_response":{"follow_up":false,"business_days":[6,1],"business_hours":{"open":"08:00","close":"18:00"},"daily_list":null}}`), &meta)
	if err := Validate(meta); err != nil {
		t.Fatal(err)
	}
	lr := meta[TenantKey].(map[string]any)
	if _, ok := lr["daily_list"]; ok {
		t.Fatal("null drops the key")
	}
	if days := lr["business_days"].([]any); days[0] != 1 || days[1] != 6 {
		t.Fatalf("days sorted: %v", days)
	}
	for _, bad := range []string{
		`{"lead_response":{"reminder_minutes":[120,15]}}`,
		`{"lead_response":{"reminder_minutes":[1,2,3,4,5,6]}}`,
		`{"lead_response":{"business_days":[]}}`,
		`{"lead_response":{"surprise":true}}`,
		`{"lead_response":{"follow_up":"yes"}}`,
		`{"lead_response":"off"}`,
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(bad), &m)
		if err := Validate(m); err == nil {
			t.Fatalf("%s must be refused", bad)
		}
	}
	var cleared map[string]any
	_ = json.Unmarshal([]byte(`{"lead_response":{}}`), &cleared)
	if err := Validate(cleared); err != nil || cleared[TenantKey] != nil {
		t.Fatalf("an empty object clears the overrides: %v %v", err, cleared[TenantKey])
	}
}

func TestChangedComparesStoredForm(t *testing.T) {
	var stored map[string]any
	_ = json.Unmarshal([]byte(`{"lead_response":{"reminder_minutes":[15,120]}}`), &stored)
	next := map[string]any{TenantKey: map[string]any{"reminder_minutes": []any{15, 120}}}
	if err := Validate(next); err != nil {
		t.Fatal(err)
	}
	if Changed(stored, next[TenantKey]) {
		t.Fatal("the same value sent back is not a change")
	}
	next = map[string]any{TenantKey: map[string]any{"reminder_minutes": []any{15, 60}}}
	_ = Validate(next)
	if !Changed(stored, next[TenantKey]) {
		t.Fatal("a new value is a change")
	}
}

func TestAddBusinessCountsOnlyOpenHours(t *testing.T) {
	loc := nairobi(t)
	s := Fallback()
	s.Location = loc
	at := func(y int, m time.Month, d, h, min int) time.Time { return time.Date(y, m, d, h, min, 0, 0, loc) }
	// 2026-11-24 is a Tuesday.
	cases := []struct {
		name string
		from time.Time
		d    time.Duration
		want time.Time
	}{
		{"inside hours", at(2026, 11, 24, 10, 0), 15 * time.Minute, at(2026, 11, 24, 10, 15)},
		{"after hours: next morning", at(2026, 11, 24, 22, 0), 15 * time.Minute, at(2026, 11, 25, 8, 15)},
		{"before opening", at(2026, 11, 24, 6, 0), 2 * time.Hour, at(2026, 11, 24, 10, 0)},
		{"spills over closing", at(2026, 11, 24, 17, 0), 2 * time.Hour, at(2026, 11, 25, 9, 0)},
		{"Saturday evening skips Sunday", at(2026, 11, 28, 17, 30), time.Hour, at(2026, 11, 30, 8, 30)},
		{"24 business hours span days", at(2026, 11, 24, 10, 0), 24 * time.Hour, at(2026, 11, 26, 14, 0)},
	}
	for _, c := range cases {
		if got := s.AddBusiness(c.from, c.d); !got.Equal(c.want) {
			t.Errorf("%s: got %s, want %s", c.name, got.In(loc), c.want)
		}
	}
	if !s.IsOpen(at(2026, 11, 24, 9, 0)) || s.IsOpen(at(2026, 11, 29, 9, 0)) || s.IsOpen(at(2026, 11, 24, 18, 0)) {
		t.Fatal("IsOpen disagrees with Mon-Sat 08:00-18:00")
	}
	// No open day at all: the clock runs all the time.
	none := s
	none.Days = [7]bool{}
	if got := none.AddBusiness(at(2026, 11, 24, 22, 0), time.Hour); !got.Equal(at(2026, 11, 24, 23, 0)) {
		t.Fatalf("no business days: %s", got)
	}
}

func TestCountsAsContactAndReply(t *testing.T) {
	in, out := "inbound", "outbound"
	if !IsReply("whatsapp", in) || IsReply("form_submission", in) || IsReply("whatsapp", out) {
		t.Fatal("IsReply")
	}
	if CountsAsContact("call", &out, nil) {
		t.Fatal("an automated activity is not a person's contact")
	}
	by := uuid.New()
	if !CountsAsContact("call", nil, &by) || !CountsAsContact("email", &out, &by) {
		t.Fatal("a call or email a person logged is a contact")
	}
	if CountsAsContact("note", nil, &by) || CountsAsContact("whatsapp", &in, &by) {
		t.Fatal("a note or an inbound message is not")
	}
}
