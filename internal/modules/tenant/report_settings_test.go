package tenant

import (
	"strings"
	"testing"
)

func TestValidateReportSettings(t *testing.T) {
	ok := []map[string]any{
		{},
		{WeeklyReportKey: "on"}, {WeeklyReportKey: "off"}, {WeeklyReportKey: nil},
		{AverageSaleValueKey: 2500.0}, {AverageSaleValueKey: 0.0},
		{EmailFooterKey: "Shop 4, Mega Plaza, Kisumu. Open Monday to Saturday."},
	}
	for _, m := range ok {
		if err := ValidateReportSettings(m); err != nil {
			t.Errorf("%v: %v", m, err)
		}
	}
	bad := []map[string]any{
		{WeeklyReportKey: "yes"}, {WeeklyReportKey: true},
		{AverageSaleValueKey: -1.0}, {AverageSaleValueKey: "2500"},
		{EmailFooterKey: 12.0}, {EmailFooterKey: strings.Repeat("a", 301)},
	}
	for _, m := range bad {
		if err := ValidateReportSettings(m); err == nil {
			t.Errorf("%v: want an error", m)
		}
	}
}

func TestReportSettingReaders(t *testing.T) {
	if !WeeklyReportOn(map[string]any{}) || WeeklyReportOn(map[string]any{WeeklyReportKey: "off"}) {
		t.Fatal("weekly report defaults on and turns off")
	}
	if AverageSaleValue(map[string]any{AverageSaleValueKey: 1200.0}) != 1200 || AverageSaleValue(map[string]any{}) != 0 {
		t.Fatal("average sale value")
	}
	if got := EmailFooter(map[string]any{EmailFooterKey: "  Shop 4\nKisumu  "}); got != "Shop 4 Kisumu" {
		t.Fatalf("footer = %q", got)
	}
}

func TestPublicProfileCacheKeys(t *testing.T) {
	if keys := PublicProfileCacheKeys(nil); keys != nil {
		t.Fatal("nil tenant has no keys")
	}
}
