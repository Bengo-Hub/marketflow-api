package leadresponse

import "time"

// maxBusinessSteps bounds the walk over days in AddBusiness (a year of windows is far beyond any
// reminder, which is at most seven days of business time).
const maxBusinessSteps = 800

// AddBusiness is the moment d of business time has passed since from, counting only the open
// hours of open days in the tenant's time zone. A lead that arrives after hours starts counting
// at the next opening, so its first reminder lands the next business morning. With no open day
// the clock runs all the time.
func (s Settings) AddBusiness(from time.Time, d time.Duration) time.Time {
	if !s.anyDay() || s.Open >= s.Close {
		return from.Add(d)
	}
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	cur := from.In(loc)
	for i := 0; i < maxBusinessSteps; i++ {
		start, end := s.window(cur)
		if !s.Days[cur.Weekday()] || !cur.Before(end) {
			cur = s.nextOpening(cur)
			continue
		}
		if cur.Before(start) {
			cur = start
		}
		if avail := end.Sub(cur); d <= avail {
			return cur.Add(d)
		} else {
			d -= avail
		}
		cur = end
	}
	return from.Add(d)
}

// IsOpen reports whether t falls inside the tenant's business hours.
func (s Settings) IsOpen(t time.Time) bool {
	if !s.anyDay() || s.Open >= s.Close {
		return true
	}
	loc := s.Location
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	start, end := s.window(t)
	return s.Days[t.Weekday()] && !t.Before(start) && t.Before(end)
}

func (s Settings) anyDay() bool {
	for _, d := range s.Days {
		if d {
			return true
		}
	}
	return false
}

// window is the opening and closing time on t's local day.
func (s Settings) window(t time.Time) (time.Time, time.Time) {
	y, m, d := t.Date()
	loc := t.Location()
	return time.Date(y, m, d, s.Open/60, s.Open%60, 0, 0, loc), time.Date(y, m, d, s.Close/60, s.Close%60, 0, 0, loc)
}

// nextOpening is the opening time of the first open day after t's local day.
func (s Settings) nextOpening(t time.Time) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	for i := 1; i <= 7; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, loc)
		if s.Days[day.Weekday()] {
			return time.Date(day.Year(), day.Month(), day.Day(), s.Open/60, s.Open%60, 0, 0, loc)
		}
	}
	return time.Date(y, m, d+1, s.Open/60, s.Open%60, 0, 0, loc)
}
