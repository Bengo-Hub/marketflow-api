package main

import (
	"strings"
	"testing"
)

// The default follow-up every tenant gets (owner, 2026-11-24): an instant hello by email and, with
// consent, WhatsApp; a follow-up a day later and another two days after that. Every word comes from
// the tenant's own variables, the email text carries no HTML (notifications-api strips it, so a
// link inside a tag would vanish) and nothing invents urgency.
func TestInstantFollowUpTemplate(t *testing.T) {
	var steps []seqStep
	for _, s := range defaultSequences() {
		if s.Name == "Instant follow-up" {
			steps = s.Steps
		}
	}
	if len(steps) != 4 {
		t.Fatalf("want 4 steps, got %d", len(steps))
	}
	delays := []int{0, 0, 24, 48}
	for i, st := range steps {
		if st.DelayHours != delays[i] {
			t.Fatalf("step %d delay %d, want %d", i, st.DelayHours, delays[i])
		}
		if !st.Optional {
			t.Fatalf("step %d must be optional, so a lead it cannot reach does not stop the rest", i)
		}
		text := strings.ToLower(st.Subject + " " + st.Body)
		if strings.Contains(text, "<a ") || strings.Contains(text, "codevertex") {
			t.Fatalf("step %d: no HTML and no platform brand in a tenant's message: %q", i, text)
		}
		for _, w := range []string{"hurry", "limited", "only today", "last chance", "expires", "act now"} {
			if strings.Contains(text, w) {
				t.Fatalf("step %d invents urgency (%q)", i, w)
			}
		}
		if !strings.Contains(st.Body, "{{fbo_name}}") {
			t.Fatalf("step %d must name the tenant", i)
		}
	}
	if steps[1].Channel != "whatsapp" || steps[1].WATemplate != "mf_lead_hello_v1_btn" || steps[1].WAButton != "book/{{tenant_slug}}" ||
		len(steps[1].WAParams) != 2 {
		t.Fatalf("the WhatsApp hello is the tenant-number template with the booking button: %+v", steps[1])
	}
	m := stepMaps(steps)
	if m[1]["optional"] != true || m[1]["wa_template"] != "mf_lead_hello_v1_btn" {
		t.Fatalf("stored form lost the WhatsApp template: %v", m[1])
	}
	if _, ok := m[0]["wa_template"]; ok {
		t.Fatal("an email step has no WhatsApp template")
	}
}
