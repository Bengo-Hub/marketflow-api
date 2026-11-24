package leads

import (
	"fmt"
	"os"
	"strings"
)

// Not every form submission is a lead (owner, 2026-11-24). A lead is a person interested in the
// tenant's products or services; someone asking for attachment, an internship or a job, or a
// business pitching its own services, is not, and must never be nurtured, alerted on or counted.
// The public lead endpoint and the service API check each submission against keyword lists:
// MF_LEAD_FILTER_ATTACHMENT, MF_LEAD_FILTER_JOB and MF_LEAD_FILTER_SERVICE_OFFER (comma
// separated) replace the platform defaults, and a tenant can add its own words in
// metadata.lead_filter {attachment: [...], job: [...], service_offer: [...]}.

// Non-lead kinds.
const (
	NonLeadAttachment   = "attachment"
	NonLeadJob          = "job"
	NonLeadServiceOffer = "service_offer"
)

// LeadFilter holds the keyword lists, lower case.
type LeadFilter struct {
	Attachment   []string
	Job          []string
	ServiceOffer []string
}

var defaultLeadFilter = LeadFilter{
	Attachment: []string{"attachment application", "industrial attachment", "attachment opportunity", "attachment place",
		"attachment position", "for attachment", "for an attachment", "internship", " intern ", "student attachment"},
	Job: []string{"job opportunity", "vacancy", "vacancies", "employment", "hire me", "my cv", "my resume", "curriculum vitae",
		"interested in joining", "join your team", "looking for a job", "apply for a position", "position of"},
	ServiceOffer: []string{"hi agency owner", "we are a web development company", "we’re a web development company",
		"we're a web development company", "our services include", "white label partner", "seo services", "we offer our services"},
}

// PlatformLeadFilter is the defaults with the MF_LEAD_FILTER_* overrides.
func PlatformLeadFilter() LeadFilter {
	f := defaultLeadFilter
	if v := splitWords(os.Getenv("MF_LEAD_FILTER_ATTACHMENT")); len(v) > 0 {
		f.Attachment = v
	}
	if v := splitWords(os.Getenv("MF_LEAD_FILTER_JOB")); len(v) > 0 {
		f.Job = v
	}
	if v := splitWords(os.Getenv("MF_LEAD_FILTER_SERVICE_OFFER")); len(v) > 0 {
		f.ServiceOffer = v
	}
	return f
}

func splitWords(s string) []string {
	var out []string
	for _, w := range strings.Split(s, ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			out = append(out, w)
		}
	}
	return out
}

// WithTenant adds the tenant's own words (metadata.lead_filter) to the platform lists.
func (f LeadFilter) WithTenant(meta map[string]any) LeadFilter {
	lf, _ := meta["lead_filter"].(map[string]any)
	add := func(base []string, key string) []string {
		list, _ := lf[key].([]any)
		out := append([]string{}, base...)
		for _, v := range list {
			if s, ok := v.(string); ok {
				out = append(out, splitWords(s)...)
			}
		}
		return out
	}
	return LeadFilter{Attachment: add(f.Attachment, "attachment"), Job: add(f.Job, "job"), ServiceOffer: add(f.ServiceOffer, "service_offer")}
}

// NonLeadKind returns why a submission is not a lead, or "" for a real lead. It reads the
// interest, the form answers and the message, notes and topic in metadata.
func (f LeadFilter) NonLeadKind(in CreateInput) string {
	parts := []string{}
	if in.Interest != nil {
		parts = append(parts, *in.Interest)
	}
	for _, k := range []string{"message", "notes", "topic", "service"} {
		if s, ok := in.Metadata[k].(string); ok {
			parts = append(parts, s)
		}
	}
	for _, v := range in.QualificationAnswers {
		parts = append(parts, fmt.Sprint(v))
	}
	text := " " + strings.Join(strings.Fields(strings.ToLower(strings.Join(parts, " "))), " ") + " "
	has := func(words []string) bool {
		for _, w := range words {
			if strings.Contains(text, w) {
				return true
			}
		}
		return false
	}
	switch {
	case has(f.Attachment):
		return NonLeadAttachment
	case has(f.Job):
		return NonLeadJob
	case has(f.ServiceOffer):
		return NonLeadServiceOffer
	}
	return ""
}
