package analytics

import (
	"testing"

	"github.com/Bengo-Hub/reports"
)

func TestRatios(t *testing.T) {
	cpl, roi, roas := ratios(1000, 4, 3000)
	if cpl == nil || *cpl != 250 || roi == nil || *roi != 200 || roas == nil || *roas != 3 {
		t.Fatalf("got %v %v %v", cpl, roi, roas)
	}
	cpl, roi, roas = ratios(0, 4, 3000)
	if cpl != nil || roi != nil || roas != nil {
		t.Fatal("no spend gives no ratios")
	}
}

func TestOptPercent(t *testing.T) {
	up, down := 150.0, -20.0
	if optPercent(&up) != "+150%" || optPercent(&down) != "-20%" || optPercent(nil) != "-" {
		t.Fatal("percent captions")
	}
}

func TestLeadBarsWeeklyBeyondAMonth(t *testing.T) {
	daily := []DayCount{{Day: "2026-11-02", Count: 1}, {Day: "2026-11-03", Count: 2}, {Day: "2026-11-09", Count: 5}}
	if bars := leadBars(daily, 30); len(bars) != 3 || bars[0].Label != "02 Nov" {
		t.Fatalf("daily bars: %+v", bars)
	}
	bars := leadBars(daily, 90)
	if len(bars) != 2 || bars[0].Value != 3 || bars[1].Value != 5 || bars[0].Label != "w/c 02 Nov" {
		t.Fatalf("weekly bars: %+v", bars)
	}
}

func TestReportCurrency(t *testing.T) {
	if reportCurrency(nil) != "KES" {
		t.Fatal("default KES")
	}
	if reportCurrency([]CampaignROI{{Currency: "usd"}, {Currency: "USD"}}) != "USD" {
		t.Fatal("shared currency")
	}
	if reportCurrency([]CampaignROI{{Currency: "USD"}, {Currency: "KES"}}) != "KES" {
		t.Fatal("mixed currencies fall back to KES")
	}
}

func TestHexColor(t *testing.T) {
	for _, c := range []string{"#fff", "#0F766E"} {
		if !hexColor(c) {
			t.Errorf("%s should pass", c)
		}
	}
	for _, c := range []string{"", "fff", "#ffff", "#zzz", "red"} {
		if hexColor(c) {
			t.Errorf("%s should fail", c)
		}
	}
}

// The report model renders in every format without data from the database.
func TestReportRendersEveryFormat(t *testing.T) {
	v := 120.0
	r := &reports.Report{Title: "Marketing report", TenantName: "Test", Currency: "KES",
		Sections: []reports.Section{{Kind: reports.SectionChart, Title: "New leads", Bars: leadBars([]DayCount{{Day: "2026-11-02", Count: 1}}, 7)},
			{Kind: reports.SectionTable, Title: "Return on each campaign",
				Columns: []reports.Column{{Header: "Campaign"}, {Header: "Per lead", Money: true}},
				Rows:    [][]reports.Cell{{reports.Text("Launch"), reports.Text(optMoney("KES", &v))}}}}}
	for _, f := range []reports.Format{reports.FormatPDF, reports.FormatCSV, reports.FormatXLSX} {
		if b, _, err := reports.Generate(r, f); err != nil || len(b) == 0 {
			t.Fatalf("%s: %v", f, err)
		}
	}
}
