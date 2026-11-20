package marketingos

import (
	"reflect"
	"testing"
)

func TestParsePaymentMethods(t *testing.T) {
	got := ParsePaymentMethods("M-Pesa, cash\nCard; m-pesa ,  ")
	if !reflect.DeepEqual(got, []string{"M-Pesa", "cash", "Card"}) {
		t.Fatalf("methods = %v", got)
	}
}

func TestParseGlossary(t *testing.T) {
	got := ParseGlossary("M-Pay = M-Pesa\nbroken line\n = x\nMpesa=M-Pesa\nsame = SAME")
	want := []GlossaryTerm{{Wrong: "M-Pay", Right: "M-Pesa"}, {Wrong: "Mpesa", Right: "M-Pesa"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("glossary = %+v", got)
	}
	if len(ParsePaymentMethods(defaultPaymentMethods)) != 1 || len(ParseGlossary(defaultGlossary)) != 2 {
		t.Fatal("the documented fallbacks must parse")
	}
}
