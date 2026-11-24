package postingplan

import (
	"testing"
	"time"
)

var eat = func() *time.Location {
	l, err := time.LoadLocation("Africa/Nairobi")
	if err != nil {
		return time.FixedZone("EAT", 3*3600)
	}
	return l
}()

// at is a wall-clock time in EAT. 2026-11-23 is a Monday.
func at(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, eat) }

func TestNextWindowEAT(t *testing.T) {
	fb := DefaultWindows()["facebook"] // weekdays 12-14, 19-21; weekend 10-12
	cases := []struct {
		name     string
		from     time.Time
		want     time.Time
		inWindow bool
	}{
		{"Monday morning waits for lunch", at(23, 8, 30), at(23, 12, 0), false},
		{"inside a window posts at once", at(23, 13, 15), at(23, 13, 15), true},
		{"window end is exclusive", at(23, 14, 0), at(23, 19, 0), false},
		{"Friday night rolls to Saturday morning", at(27, 21, 30), at(28, 10, 0), false},
		{"Sunday after the weekend window rolls to Monday lunch", at(29, 12, 0), at(30, 12, 0), false},
	}
	for _, c := range cases {
		got := Next(fb, c.from, eat)
		if !got.Equal(c.want) {
			t.Errorf("%s: Next = %s, want %s", c.name, got.In(eat), c.want)
		}
	}
	// A UTC input means the same instant: 05:30 UTC Monday is 08:30 EAT.
	if got := Next(fb, time.Date(2026, 11, 23, 5, 30, 0, 0, time.UTC), eat); !got.Equal(at(23, 12, 0)) {
		t.Errorf("UTC input: %s", got.In(eat))
	}
	// EAT has no daylight saving: the same wall-clock window is 09:00 UTC in June and in December.
	for _, d := range []time.Time{time.Date(2026, 6, 15, 6, 0, 0, 0, eat), time.Date(2026, 12, 14, 6, 0, 0, 0, eat)} {
		if got := Next(fb, d, eat).UTC(); got.Hour() != 9 || got.Minute() != 0 {
			t.Errorf("%s: window start is %s UTC, want 09:00", d.Month(), got.Format("15:04"))
		}
	}
	if got := Next(nil, at(23, 8, 0), eat); !got.Equal(at(23, 8, 0)) {
		t.Errorf("no windows returns from")
	}
}

func TestNextWindowAcrossDST(t *testing.T) {
	// A tenant in a zone with daylight saving keeps its local hour across the change.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	w := []Window{{DaysDaily, "12:00", "13:00"}}
	before := Next(w, time.Date(2026, 3, 7, 9, 0, 0, 0, ny), ny) // EST
	after := Next(w, time.Date(2026, 3, 9, 9, 0, 0, 0, ny), ny)  // EDT
	if before.In(ny).Hour() != 12 || after.In(ny).Hour() != 12 || before.UTC().Hour() == after.UTC().Hour() {
		t.Fatalf("local noon both sides: %s %s", before, after)
	}
}

func TestWindowValidation(t *testing.T) {
	bad := []Window{{"", "10:00", "11:00"}, {"funday", "10:00", "11:00"}, {DaysDaily, "11:00", "10:00"}, {DaysDaily, "25:00", "26:00"}}
	for _, w := range bad {
		if w.Validate() == nil {
			t.Errorf("accepted %+v", w)
		}
	}
	if err := (Window{"mon, wed,fri", "07:00", "08:30"}).Validate(); err != nil {
		t.Fatal(err)
	}
	if !(Window{"mon,wed", "07:00", "08:00"}).covers(time.Wednesday) || (Window{"mon,wed", "07:00", "08:00"}).covers(time.Tuesday) {
		t.Fatal("day list")
	}
}

func TestMergeLayers(t *testing.T) {
	platform := `{"tiktok":[{"days":"daily","start":"20:00","end":"22:00"}]}`
	tenant := map[string]any{"facebook": []any{map[string]any{"days": "weekdays", "start": "07:00", "end": "09:00"}}}
	cad := map[string]any{"instagram": map[string]any{"count": 12, "per": "month"}}
	s := Merge(platform, `not json`, tenant, cad, "Africa/Nairobi", eat)
	if s.Platforms["tiktok"].WindowsSource != SourcePlatform || s.Platforms["tiktok"].Windows[0].Start != "20:00" {
		t.Fatalf("platform layer: %+v", s.Platforms["tiktok"])
	}
	if s.Platforms["facebook"].WindowsSource != SourceTenant || s.Platforms["youtube"].WindowsSource != SourceDefault {
		t.Fatalf("tenant layer: %+v", s.Platforms)
	}
	if c := s.Platforms["instagram"]; c.Cadence != (Cadence{12, PerMonth}) || c.CadenceSource != SourceTenant {
		t.Fatalf("cadence: %+v", c)
	}
	// A layer that does not validate is ignored as a whole.
	s = Merge(`{"facebook":[{"days":"weekdays","start":"14:00","end":"13:00"}]}`, nil, nil, nil, "", eat)
	if s.Platforms["facebook"].WindowsSource != SourceDefault {
		t.Fatal("invalid platform row applied")
	}
	if _, err := ParseCadence(map[string]any{"facebook": map[string]any{"count": 50, "per": "week"}}); err == nil {
		t.Fatal("50 a week accepted")
	}
	if _, err := ParseWindows(map[string]any{"myspace": []any{}}); err == nil {
		t.Fatal("unknown platform accepted")
	}
}

func TestCadenceDue(t *testing.T) {
	week := Cadence{3, PerWeek}
	now := at(25, 9, 0) // Wednesday
	booked := []time.Time{at(23, 12, 0), at(24, 12, 0)}
	if !Due(week, booked, now, eat) {
		t.Fatal("2 of 3 is due")
	}
	booked = append(booked, at(26, 12, 0)) // a scheduled Thursday post counts
	if Due(week, booked, now, eat) {
		t.Fatal("3 of 3 is not due")
	}
	// Last week's posts do not count; Monday 00:00 EAT starts the week.
	if !Due(week, booked, at(30, 0, 0), eat) {
		t.Fatal("new week is due")
	}
	if Due(Cadence{0, PerWeek}, nil, now, eat) {
		t.Fatal("zero cadence is never due")
	}
	// 8 a month in a 30-day month is 2 a week, and stops at 8 for the month.
	month := Cadence{8, PerMonth}
	if Due(month, []time.Time{at(23, 12, 0), at(24, 12, 0)}, now, eat) {
		t.Fatal("weekly share of a monthly cadence is met")
	}
	var full []time.Time
	for d := 2; d <= 16; d += 2 {
		full = append(full, at(d, 12, 0))
	}
	if Due(month, full, now, eat) {
		t.Fatal("month is full")
	}
}

func TestSlotSpreadsPosts(t *testing.T) {
	p := Platform{Windows: DefaultWindows()["instagram"], Cadence: Cadence{3, PerWeek}}
	var booked []time.Time
	from := at(23, 8, 0) // Monday morning
	var got []time.Time
	for i := 0; i < 3; i++ {
		s := Slot(p, from, eat, booked)
		got = append(got, s)
		booked = append(booked, s)
	}
	want := []time.Time{at(23, 12, 0), at(25, 12, 0), at(27, 12, 0)} // Mon, Wed, Fri
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Fatalf("slot %d = %s, want %s (all %v)", i, got[i].In(eat), want[i], got)
		}
	}
	// Daily cadence never skips a day.
	p.Cadence = Cadence{7, PerWeek}
	if s := Slot(p, at(24, 8, 0), eat, []time.Time{at(23, 12, 0)}); !s.Equal(at(24, 12, 0)) {
		t.Fatalf("daily: %s", s.In(eat))
	}
}

func TestPlanOnlyDuePlatforms(t *testing.T) {
	s := Merge(nil, nil, nil, map[string]any{"youtube": map[string]any{"count": 1, "per": "week"}}, "Africa/Nairobi", eat)
	booked := map[string][]time.Time{"youtube": {at(23, 16, 0)}}
	plan := s.Plan([]string{"facebook", "youtube", "tiktok"}, booked, at(24, 10, 0))
	if _, ok := plan["youtube"]; ok {
		t.Fatal("youtube met its cadence")
	}
	if !plan["facebook"].Equal(at(24, 12, 0)) || !plan["tiktok"].Equal(at(24, 15, 0)) {
		t.Fatalf("plan = %v", plan)
	}
	if got := WeekStart(at(29, 23, 0), eat); !got.Equal(at(23, 0, 0)) {
		t.Fatalf("Sunday belongs to the week from Monday: %s", got)
	}
}
