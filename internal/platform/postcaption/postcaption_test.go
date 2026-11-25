package postcaption

import (
	"strings"
	"testing"
)

func TestCompose(t *testing.T) {
	got := Compose(Parts{Words: "Soft soap\nOrder today", Contacts: []string{"WhatsApp: +254 700 000 000", " "}, Link: "https://x.example/f/soap", PPB: "PPB approval ref. 1"})
	want := "Soft soap\nOrder today\n\nWhatsApp: +254 700 000 000\n\nhttps://x.example/f/soap\n\nPPB approval ref. 1"
	if got != want {
		t.Fatalf("caption %q", got)
	}
	if got := Compose(Parts{Title: "Soap", Contacts: []string{"Call: 1"}}); got != "Soap\n\nCall: 1" {
		t.Fatalf("without words the title is the first line: %q", got)
	}
	if got := Compose(Parts{Words: "Call: 1 now", Contacts: []string{"Call: 1"}}); got != "Call: 1 now" {
		t.Fatalf("a contact already in the words: %q", got)
	}
	if Compose(Parts{}) != "" {
		t.Fatal("nothing")
	}
}

func TestComposeKeepsTheEndWithinTheLimit(t *testing.T) {
	long := "First line\n" + strings.Repeat("word \U0001F600 ", 600)
	got := Compose(Parts{Words: long, Contacts: []string{"WhatsApp: +254"}, Link: "https://x.example/f/a", PPB: "PPB ref"})
	if units(got) > Limit || !strings.HasPrefix(got, "First line\n") || !strings.HasSuffix(got, "WhatsApp: +254\n\nhttps://x.example/f/a\n\nPPB ref") {
		t.Fatalf("%d units: %q", units(got), got[len(got)-80:])
	}
}

func TestWords(t *testing.T) {
	if got := Words(map[string]any{"end_card": map[string]any{"headline": "A", "cta": "Buy"}}); got != "A\nBuy" {
		t.Fatalf("%q", got)
	}
	if got := Words(map[string]any{"headline": "P", "end_card": map[string]any{"headline": "A", "subline": "S"}}); got != "P\nS" {
		t.Fatalf("%q", got)
	}
	if got := Words(map[string]any{"caption": " C ", "headline": "P"}); got != "C" {
		t.Fatalf("%q", got)
	}
}

func TestLeadURL(t *testing.T) {
	// The same link marketflow-ui's adLeadUrl builds.
	if got := LeadURL("https://app.example/", "ad-1a2b3c", "1a2b3c4d-0000-0000-0000-000000000000", "facebook", "paid"); got !=
		"https://app.example/f/ad-1a2b3c?utm_campaign=ad-1a2b3c4d&utm_medium=paid&utm_source=facebook" {
		t.Fatal(got)
	}
	if LeadURL("", "x", "1", "a", "b") != "" || LeadURL("https://a", "", "1", "a", "b") != "" {
		t.Fatal("no base or slug")
	}
}
