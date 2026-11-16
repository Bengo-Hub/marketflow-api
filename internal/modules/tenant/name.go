package tenant

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxTenantNameRunes is the longest business name a profile save takes.
const MaxTenantNameRunes = 120

// ValidateTenantName refuses a business name with angle brackets or control characters, or one
// longer than MaxTenantNameRunes (audit H1). The name shows in emails, pages and WhatsApp messages.
func ValidateTenantName(name string) error {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxTenantNameRunes {
		return errors.New("the business name can be at most 120 characters")
	}
	for _, r := range name {
		if r == '<' || r == '>' {
			return errors.New("the business name cannot contain < or >")
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			return errors.New("the business name cannot contain control characters")
		}
	}
	return nil
}
