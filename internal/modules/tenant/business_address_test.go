package tenant

import (
	"strings"
	"testing"
)

func TestValidateBusinessAddress(t *testing.T) {
	if err := ValidateBusinessAddress(map[string]any{"business_address": "Pioneer House, Oginga Street, Kisumu CBD", "business_city": "Kisumu"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateBusinessAddress(map[string]any{"business_address": strings.Repeat("a", 201)}); err == nil {
		t.Fatal("a long address must fail")
	}
	if err := ValidateBusinessAddress(map[string]any{"business_city": 5}); err == nil {
		t.Fatal("a number is not a city")
	}
	if err := ValidateBusinessAddress(map[string]any{"business_address": nil}); err != nil {
		t.Fatal("null clears the field")
	}
}
