package funnels

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestAdSlug(t *testing.T) {
	id := uuid.MustParse("1a2b3c4d-0000-0000-0000-000000000000")
	cases := map[string]string{
		"Kids Coding Classes, Kisumu!": "kids-coding-classes-kisumu-1a2b3c",
		"   ":                          "ad-1a2b3c",
		"Ñame & co":                    "ame-co-1a2b3c",
	}
	for in, want := range cases {
		if got := adSlug(in, id); got != want {
			t.Errorf("adSlug(%q) = %q, want %q", in, got, want)
		}
	}
	if long := adSlug(strings.Repeat("word ", 30), id); len(long) > 48 {
		t.Errorf("slug too long: %q", long)
	}
}

func TestLeadFormAsksForPhone(t *testing.T) {
	for _, lang := range []string{"en", "sw"} {
		f := leadForm(lang)
		fields := f["fields"].([]map[string]any)
		ok := false
		for _, x := range fields {
			if x["key"] == "phone" && x["required"] == true {
				ok = true
			}
		}
		if f["block_type"] != "form" || !ok {
			t.Fatalf("%s form = %+v", lang, f)
		}
	}
}
