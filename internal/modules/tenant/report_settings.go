package tenant

import (
	"errors"
	"strings"
)

// Sprint 7 #48, #52 and #54: report and email settings, kept in Tenant.metadata and saved through
// the profile PATCH like the sales settings.
const (
	// WeeklyReportKey switches the Monday report email, "on" (unset means on) or "off".
	WeeklyReportKey = "weekly_report"
	// AverageSaleValueKey is what one converted lead is worth to the business when no won deal
	// says so. Unset or 0 uses the average of the tenant's own won deals.
	AverageSaleValueKey = "average_sale_value"
	// EmailFooterKey is the line closing every email the business sends to its leads and
	// customers (address, opening hours, a sign-off). notifications-api prints it under the
	// shared layout's contact line as brand_note.
	EmailFooterKey = "email_footer"
)

const (
	maxAverageSaleValue = 1e11
	maxEmailFooterLen   = 300
)

// ValidateReportSettings checks the report and email keys of a profile metadata update. A null
// value clears the key.
func ValidateReportSettings(meta map[string]any) error {
	if v, ok := meta[WeeklyReportKey]; ok && v != nil {
		if s, isStr := v.(string); !isStr || (s != "on" && s != "off") {
			return errors.New(`weekly_report must be "on" or "off"`)
		}
	}
	if v, ok := meta[AverageSaleValueKey]; ok && v != nil {
		n, isNum := v.(float64)
		if !isNum || n < 0 || n > maxAverageSaleValue {
			return errors.New("average_sale_value must be a number from 0 up to 100 billion")
		}
	}
	if v, ok := meta[EmailFooterKey]; ok && v != nil {
		s, isStr := v.(string)
		if !isStr {
			return errors.New("email_footer must be text")
		}
		if len([]rune(strings.TrimSpace(s))) > maxEmailFooterLen {
			return errors.New("email_footer must be at most 300 characters")
		}
	}
	return nil
}

// WeeklyReportOn reports whether the tenant gets the Monday report email.
func WeeklyReportOn(meta map[string]any) bool {
	s, _ := meta[WeeklyReportKey].(string)
	return s != "off"
}

// AverageSaleValue is the owner's own figure for one converted lead, 0 when unset.
func AverageSaleValue(meta map[string]any) float64 {
	n, _ := meta[AverageSaleValueKey].(float64)
	if n < 0 {
		return 0
	}
	return n
}

// EmailFooter is the tenant's email closing line, trimmed and on one line.
func EmailFooter(meta map[string]any) string {
	s, _ := meta[EmailFooterKey].(string)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxEmailFooterLen {
		s = string(r[:maxEmailFooterLen])
	}
	return s
}
