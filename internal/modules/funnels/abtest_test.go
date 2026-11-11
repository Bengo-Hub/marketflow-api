package funnels

import (
	"testing"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Sprint 7 #53: a funnel can show a second version (B) of its headline, subheadline and hero
// image to part of its visitors; each lead records the version it saw.
func TestValidateABTest(t *testing.T) {
	ok := []map[string]any{
		nil,
		{"headline": "x"},
		{MetaABTest: nil},
		{MetaABTest: map[string]any{"enabled": false}},
		{MetaABTest: map[string]any{"enabled": true, "split": 50.0, "b": map[string]any{"headline": "Try this"}}},
		{MetaABTest: map[string]any{"enabled": true, "split": 30.0, "b": map[string]any{"hero_image_url": "https://x.com/a.jpg"}}},
	}
	for _, m := range ok {
		if err := ValidateABTest(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	bad := []map[string]any{
		{MetaABTest: "yes"},
		{MetaABTest: map[string]any{"enabled": true, "split": 50.0}},                                                    // nothing different in B
		{MetaABTest: map[string]any{"enabled": true, "split": 95.0, "b": map[string]any{"headline": "x"}}},              // split out of range
		{MetaABTest: map[string]any{"enabled": true, "split": 50.0, "b": map[string]any{"hero_image_url": "http://x"}}}, // not https
		{MetaABTest: map[string]any{"enabled": true, "split": 50.0, "b": map[string]any{"headline": string(make([]byte, 300))}}},
	}
	for _, m := range bad {
		if err := ValidateABTest(m); err == nil {
			t.Errorf("%v: want an error", m)
		}
	}
}

func TestVariantFor(t *testing.T) {
	ab := ABTest{Enabled: true, Split: 30}
	if VariantFor(ab, 0) != "b" || VariantFor(ab, 29) != "b" || VariantFor(ab, 30) != "a" || VariantFor(ab, 99) != "a" {
		t.Fatal("buckets below the split see B")
	}
	if VariantFor(ABTest{Split: 50}, 0) != "a" {
		t.Fatal("a stopped test always shows A")
	}
	if NormalVariant("B") != "b" || NormalVariant("a") != "a" || NormalVariant("<x>") != "" {
		t.Fatal("only a or b is recorded")
	}
}

func TestPublicABTest(t *testing.T) {
	f := &ent.Funnel{Name: "F", Slug: "f", Metadata: map[string]any{
		MetaABTest: map[string]any{"enabled": true, "split": 40.0, "b": map[string]any{"headline": "B headline", "hero_image_url": "javascript:x"}},
	}}
	p := ToPublic(f, "shop")
	if p.ABTest == nil || p.ABTest.Split != 40 || p.ABTest.B.Headline != "B headline" || p.ABTest.B.HeroImageURL != "" {
		t.Fatalf("public A/B: %+v", p.ABTest)
	}
	f.Metadata[MetaABTest] = map[string]any{"enabled": false, "b": map[string]any{"headline": "x"}}
	if ToPublic(f, "shop").ABTest != nil {
		t.Fatal("a stopped test is not public")
	}
}
