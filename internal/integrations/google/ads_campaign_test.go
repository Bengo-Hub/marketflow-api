package google

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func fakeAds(t *testing.T, h http.HandlerFunc) Ads {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return Ads{HTTP: srv.Client(), Base: srv.URL, Version: "v26", AccessToken: "tok", LoginCustomerID: "999"}
}

func TestSpendStatus(t *testing.T) {
	ads := fakeAds(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("login-customer-id") != "999" {
			t.Errorf("login-customer-id not sent")
		}
		if strings.Contains(body.Query, "billing_setup") {
			_, _ = w.Write([]byte(`{"results":[{"billingSetup":{"status":"PENDING"}},{"billingSetup":{"status":"APPROVED"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"customer":{"status":"ENABLED"}}]}`))
	})
	st, err := ads.SpendStatus(context.Background(), "123-456-7890")
	if err != nil || !st.CanSpend || st.Billing != "APPROVED" {
		t.Fatalf("status %+v err %v", st, err)
	}
}

func TestBudgetOptions(t *testing.T) {
	var path string
	ads := fakeAds(t, func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"recommendations":[{"campaignBudgetRecommendation":{"recommendedBudgetAmountMicros":"2000000000",
			"budgetOptions":[{"budgetAmountMicros":"1000000000","impact":{"potentialMetrics":{"clicks":40}}},
			{"budgetAmountMicros":"2000000000","impact":{"potentialMetrics":{"clicks":"75","conversions":3}}}]}}]}`))
	})
	opts, err := ads.BudgetOptions(context.Background(), "1", RecommendationInput{FinalURL: "https://x.test"})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v26/customers/1/recommendations:generate" || len(opts) != 2 || !opts[1].Recommended || opts[1].Clicks != 75 || opts[0].Recommended {
		t.Fatalf("path %s opts %+v", path, opts)
	}
}

func TestCreatePMaxOneAtomicMutate(t *testing.T) {
	var body struct {
		Ops []map[string]map[string]map[string]any `json:"mutateOperations"`
	}
	ads := fakeAds(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v26/customers/1234567890/googleAds:mutate" {
			t.Errorf("path %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"mutateOperationResponses":[{"campaignBudgetResult":{"resourceName":"customers/1/campaignBudgets/5"}},{"campaignResult":{"resourceName":"customers/1/campaigns/7"}}]}`))
	})
	spec := PMaxSpec{Name: "MF run", DailyMicros: 1_500_000_000, Start: time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
		End: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), FinalURL: "https://shop.test",
		Headlines: []string{"a", "b", "c"}, LongHeadlines: []string{"long"}, Descriptions: []string{"d1", "d2"},
		BusinessName: "Shop", Logo: []byte{1}, Landscape: []byte{2}, Square: []byte{3}, YouTubeIDs: []string{"yt1"}, GeoTargets: []int64{2404}}
	rn, err := ads.CreatePMax(context.Background(), "123-456-7890", spec)
	if err != nil || rn != "customers/1/campaigns/7" {
		t.Fatalf("rn %q err %v", rn, err)
	}
	var campaign map[string]any
	var groupAt, firstLinkAt, video int
	for i, op := range body.Ops {
		for k, v := range op {
			switch k {
			case "campaignOperation":
				campaign = v["create"]
			case "assetGroupOperation":
				groupAt = i
			case "assetGroupAssetOperation":
				if firstLinkAt == 0 {
					firstLinkAt = i
				}
				if v["create"]["fieldType"] == "YOUTUBE_VIDEO" {
					video++
				}
			}
		}
	}
	if campaign["advertisingChannelType"] != "PERFORMANCE_MAX" || campaign["endDateTime"] != "2026-12-01 23:59:59" ||
		campaign["status"] != "ENABLED" || campaign["containsEuPoliticalAdvertising"] == nil {
		t.Fatalf("campaign = %v", campaign)
	}
	if groupAt == 0 || firstLinkAt < groupAt || video != 1 {
		t.Fatalf("asset group at %d, first link at %d, video links %d", groupAt, firstLinkAt, video)
	}
	if b := body.Ops[0]["campaignBudgetOperation"]["create"]; b["period"] != "DAILY" || b["amountMicros"] != "1500000000" || b["explicitlyShared"] != false {
		t.Fatalf("budget = %v", b)
	}
}

func TestCreatePMaxRefusesAnIncompleteSpec(t *testing.T) {
	ads := fakeAds(t, func(w http.ResponseWriter, r *http.Request) { t.Error("called Google with an incomplete spec") })
	if _, err := ads.CreatePMax(context.Background(), "1", PMaxSpec{Name: "x"}); err == nil || !strings.Contains(err.Error(), "logo") {
		t.Fatalf("err = %v", err)
	}
}

func TestFindCampaignQuotesName(t *testing.T) {
	var q string
	ads := fakeAds(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Query string }
		_ = json.NewDecoder(r.Body).Decode(&body)
		q = body.Query
		_, _ = w.Write([]byte(`{"results":[{"campaign":{"resourceName":"customers/1/campaigns/9"}}]}`))
	})
	rn, err := ads.FindCampaign(context.Background(), "1", "Mama's shop")
	if err != nil || rn != "customers/1/campaigns/9" || !strings.Contains(q, `Mama\'s shop`) {
		t.Fatalf("rn %q err %v q %s", rn, err, q)
	}
}

func TestCountryGeoTarget(t *testing.T) {
	if CountryGeoTarget("ke") != 2404 || CountryGeoTarget("ZZ") != 0 {
		t.Fatal("geo target")
	}
}
