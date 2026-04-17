package tenant

import (
	"fmt"
	"strings"
)

// East African country codes and their expected phone number lengths
// (digits after country code).
var eaCountries = []struct {
	Code   string // country code without +
	Name   string
	Digits int // expected digits after country code
}{
	{"254", "Kenya", 9},
	{"256", "Uganda", 9},
	{"255", "Tanzania", 9},
	{"250", "Rwanda", 9},
	{"257", "Burundi", 8},
	{"251", "Ethiopia", 9},
	{"252", "Somalia", 8},
	{"253", "Djibouti", 8},
	{"211", "South Sudan", 9},
	{"243", "DR Congo", 9},
}

// CleanPhone normalises a phone number to E.164 format (digits only, no +).
// Handles: +254..., 254..., 0...(defaults to Kenya 254), 07..., etc.
// Returns the cleaned number and an error if validation fails.
func CleanPhone(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}

	// Strip everything except digits
	var digits strings.Builder
	for _, r := range raw {
		if r >= '0' && r <= '9' {
			digits.WriteRune(r)
		}
	}
	num := digits.String()
	if num == "" {
		return "", fmt.Errorf("phone number contains no digits")
	}

	// If starts with 0, assume Kenya (+254) and replace leading 0
	if strings.HasPrefix(num, "0") {
		num = "254" + num[1:]
	}

	// Validate against known EA country codes
	for _, c := range eaCountries {
		if strings.HasPrefix(num, c.Code) {
			localPart := num[len(c.Code):]
			if len(localPart) != c.Digits {
				return "", fmt.Errorf("invalid %s phone number: expected %d digits after %s, got %d", c.Name, c.Digits, c.Code, len(localPart))
			}
			return num, nil
		}
	}

	// If no EA code matched but number is 12-15 digits (international), accept as-is
	if len(num) >= 10 && len(num) <= 15 {
		return num, nil
	}

	return "", fmt.Errorf("invalid phone number: must start with an East African country code (254, 256, 255, 250, etc.) or 0 for Kenya")
}

// ValidatePhoneRequired returns an error if the phone is empty or invalid.
// Used for FBO tenants where WhatsApp number is mandatory.
func ValidatePhoneRequired(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("phone number is required for FBO accounts")
	}
	return CleanPhone(raw)
}
