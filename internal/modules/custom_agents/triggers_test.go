package custom_agents

import (
	"testing"
	"time"
)

func TestValidateTrigger(t *testing.T) {
	ok := []struct {
		typ string
		cfg map[string]any
	}{
		{TriggerManual, nil},
		{TriggerEvent, map[string]any{"event": "lead.created"}},
		{TriggerEvent, map[string]any{"event": "crm.lead.qualified"}},
		{TriggerEvent, map[string]any{"event": "crm.contact.created"}},
		{TriggerEvent, map[string]any{"event": "crm.deal.stage_changed"}},
		{TriggerSchedule, map[string]any{"frequency": "daily", "time": "08:00"}},
		{TriggerSchedule, map[string]any{"frequency": "weekly", "time": "23:59", "weekday": float64(0)}},
	}
	for _, c := range ok {
		if err := ValidateTrigger(c.typ, c.cfg); err != nil {
			t.Errorf("%s %v: %v", c.typ, c.cfg, err)
		}
	}
	bad := []struct {
		typ string
		cfg map[string]any
	}{
		{"on_lead_created", nil},
		{TriggerEvent, map[string]any{"event": "deal.stage_changed"}}, // the subject is crm.deal.stage_changed
		{TriggerEvent, nil},
		{TriggerSchedule, map[string]any{"frequency": "hourly", "time": "08:00"}},
		{TriggerSchedule, map[string]any{"frequency": "daily", "time": "8:00"}},
		{TriggerSchedule, map[string]any{"frequency": "daily", "time": "24:00"}},
		{TriggerSchedule, map[string]any{"frequency": "weekly", "time": "08:00"}},
		{TriggerSchedule, map[string]any{"frequency": "weekly", "time": "08:00", "weekday": float64(7)}},
		{TriggerSchedule, map[string]any{"frequency": "weekly", "time": "08:00", "weekday": 1.5}},
	}
	for _, c := range bad {
		if err := ValidateTrigger(c.typ, c.cfg); err == nil {
			t.Errorf("%s %v accepted", c.typ, c.cfg)
		}
	}
}

func TestScheduleAtUsesTheDaysZone(t *testing.T) {
	nbo, _ := time.LoadLocation("Africa/Nairobi")
	s, err := ParseSchedule(map[string]any{"frequency": "daily", "time": "06:45"})
	if err != nil {
		t.Fatal(err)
	}
	got := s.At(time.Date(2026, 10, 23, 15, 0, 0, 0, nbo))
	if want := time.Date(2026, 10, 23, 6, 45, 0, 0, nbo); !got.Equal(want) {
		t.Fatalf("At = %v, want %v", got, want)
	}
}
