package custom_agents

import (
	"errors"
	"strings"
	"testing"
)

func okConfig() AutopilotConfig {
	return AutopilotConfig{OfferingIDs: []string{"o1"}, Objective: "whatsapp", Cadence: "weekly", Weekday: 1, Time: "09:00",
		Platforms: []string{"facebook"}, AccountIDs: []string{"a1"}, CreditCap: 200}
}

func TestAutopilotValidateDefaults(t *testing.T) {
	c := okConfig()
	if err := c.Validate(false); err != nil {
		t.Fatal(err)
	}
	if c.Approval != ApproveEach || c.Languages[0] != "en" || c.VideoTiers[0] != "template" {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestAutopilotValidateGuards(t *testing.T) {
	c := okConfig()
	c.Approval = AutoPublish
	if err := c.Validate(false); !errors.Is(err, ErrAutoPublishNotAllowed) {
		t.Fatalf("auto-publish without the feature: %v", err)
	}
	c = okConfig()
	c.Approval = AutoPublish
	if err := c.Validate(true); err != nil || c.VetoHours != DefaultVeto {
		t.Fatalf("auto-publish with the feature gets the default veto: %v %+v", err, c)
	}
	c = okConfig()
	c.Paid, c.PerAdBudget, c.MonthlyAdCap, c.PeriodDays = true, 5000, 3000, 7
	if err := c.Validate(false); err == nil || !strings.Contains(err.Error(), "above the monthly ceiling") {
		t.Fatalf("per-ad above ceiling: %v", err)
	}
	c = okConfig()
	c.Objective, c.Cadence, c.Time, c.CreditCap = "sales", "hourly", "9am", 0
	err := c.Validate(false)
	for _, want := range []string{"objective", "cadence", "HH:MM", "credit ceiling"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %q in %v", want, err)
		}
	}
}

func TestEnvelopeAndCredits(t *testing.T) {
	c := okConfig()
	c.Paid, c.PerAdBudget, c.MonthlyAdCap = true, 2000, 5000
	if !c.WithinEnvelope(3000) || c.WithinEnvelope(3001) {
		t.Fatal("the envelope must allow exactly up to the ceiling")
	}
	if !c.CreditsFit(150, 50) || c.CreditsFit(151, 50) {
		t.Fatal("credit ceiling")
	}
}
