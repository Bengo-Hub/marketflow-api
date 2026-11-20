package marketingos

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// The marketing team's glossary, from service config (platform value, tenant override, edited in
// Platform > Service config). marketflow-ai puts it in every specialist's prompt and corrects the
// wrong spellings in what the team writes, so no invented payment or brand term reaches the owner.
const (
	// ConfigPaymentMethods lists how the business takes payment, one per line or comma separated.
	ConfigPaymentMethods = "marketing.payment_methods"
	// ConfigGlossary lists term fixes, one "wrong = right" per line.
	ConfigGlossary = "marketing.glossary"
)

// Documented fallbacks, used only while service config has no value: Kenyan businesses take
// M-Pesa, and "M-Pay" is a name the models made up for it.
const (
	defaultPaymentMethods = "M-Pesa"
	defaultGlossary       = "M-Pay = M-Pesa\nMpesa = M-Pesa"
)

// GlossaryTerm is one correction: Wrong is never written, Right is.
type GlossaryTerm struct {
	Wrong string `json:"wrong"`
	Right string `json:"right"`
}

// glossaryFor reads the tenant's payment methods and term fixes.
func (s *Service) glossaryFor(ctx context.Context, tenantID uuid.UUID) ([]string, []GlossaryTerm) {
	methods := svcconfig.Value(ctx, s.db, tenantID, ConfigPaymentMethods)
	if methods == "" {
		methods = defaultPaymentMethods
	}
	terms := svcconfig.Value(ctx, s.db, tenantID, ConfigGlossary)
	if terms == "" {
		terms = defaultGlossary
	}
	return ParsePaymentMethods(methods), ParseGlossary(terms)
}

// ParsePaymentMethods splits a list on commas and new lines, dropping blanks and repeats.
func ParsePaymentMethods(v string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == ';' }) {
		p = strings.TrimSpace(p)
		if p != "" && !seen[strings.ToLower(p)] {
			seen[strings.ToLower(p)] = true
			out = append(out, p)
		}
	}
	return out
}

// ParseGlossary reads "wrong = right" lines; a line without both sides is skipped.
func ParseGlossary(v string) []GlossaryTerm {
	var out []GlossaryTerm
	for _, line := range strings.Split(v, "\n") {
		wrong, right, ok := strings.Cut(line, "=")
		wrong, right = strings.TrimSpace(wrong), strings.TrimSpace(right)
		if ok && wrong != "" && right != "" && !strings.EqualFold(wrong, right) {
			out = append(out, GlossaryTerm{Wrong: wrong, Right: right})
		}
	}
	return out
}
