package adsmanager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

type fakeGraph struct {
	calls      []string
	failVideo  int
	review     []string
	statuses   []string
	adSetInput meta.AdSetInput
	creative   meta.CreativeInput
	form       meta.LeadFormInput
	// failCreative fails that many creative calls (a video Meta is still processing).
	failCreative int
}

// Owner 2026-11-10: a leads boost uses a Meta instant form. One form per boost, made with the
// Page token and kept across retries; the ad set runs on the ad, and the creative points at it.
func TestLaunchLeadsUsesAnInstantForm(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	g := &fakeGraph{review: []string{"PENDING_REVIEW"}, failCreative: 1}
	p := plan(now)
	p.Objective, p.WhatsAppNumber, p.PageToken, p.PrivacyURL = "OUTCOME_LEADS", "", "page-token", "https://shop.example/privacy"
	save := func(context.Context, State) error { return nil }
	check := func(context.Context) (bool, error) { return false, nil }
	ctx := context.Background()

	st, _ := Step(ctx, g, p, State{}, now, check, save)
	if st.FormID != "form1" || st.Stage != StageCreative || st.Attempts != 1 {
		t.Fatalf("the form is kept when the creative must wait: %+v", st)
	}
	st, _ = Step(ctx, g, p, st, now, check, save)
	if st.Stage != StageReview || strings.Count(strings.Join(g.calls, ","), "form:page-token") != 1 {
		t.Fatalf("one form per boost, made with the Page token: %+v %v", st, g.calls)
	}
	if g.adSetInput.OptimizationGoal != "LEAD_GENERATION" || g.adSetInput.DestinationType != "ON_AD" {
		t.Fatalf("ad set: %+v", g.adSetInput)
	}
	if g.creative.CTA != "SIGN_UP" || g.creative.LeadFormID != "form1" {
		t.Fatalf("creative: %+v", g.creative)
	}
	if g.form.PrivacyURL != "https://shop.example/privacy" || len(g.form.Questions) == 0 || !g.form.AskWhatsAppConsent {
		t.Fatalf("form: %+v", g.form)
	}
}

func (f *fakeGraph) CreateCampaign(_ context.Context, _, _ string, in meta.CampaignInput) (*meta.CampaignResponse, error) {
	f.calls = append(f.calls, "campaign:"+in.Status)
	return &meta.CampaignResponse{ID: "c1"}, nil
}
func (f *fakeGraph) CreateAdSet(_ context.Context, _, _ string, in meta.AdSetInput) (string, error) {
	f.calls = append(f.calls, "adset")
	f.adSetInput = in
	return "as1", nil
}
func (f *fakeGraph) UploadAdVideo(context.Context, string, string, string, string) (string, error) {
	f.calls = append(f.calls, "video")
	if f.failVideo > 0 {
		f.failVideo--
		return "", errors.New("389 Unable to fetch video file from URL")
	}
	return "v1", nil
}
func (f *fakeGraph) CreateAdCreative(_ context.Context, _, _ string, in meta.CreativeInput) (string, error) {
	f.calls = append(f.calls, "creative")
	f.creative = in
	if f.failCreative > 0 {
		f.failCreative--
		return "", errors.New("video still processing")
	}
	return "cr1", nil
}
func (f *fakeGraph) CreateLeadForm(_ context.Context, pageToken, _ string, in meta.LeadFormInput) (string, error) {
	f.calls = append(f.calls, "form:"+pageToken)
	f.form = in
	return "form1", nil
}
func (f *fakeGraph) CreateAd(context.Context, string, string, string, string, string) (string, error) {
	f.calls = append(f.calls, "ad")
	return "ad1", nil
}
func (f *fakeGraph) GetAdReview(context.Context, string, string) (*meta.AdReview, error) {
	r := &meta.AdReview{EffectiveStatus: f.review[0]}
	if len(f.review) > 1 {
		f.review = f.review[1:]
	}
	if r.EffectiveStatus == "DISAPPROVED" {
		r.Feedback.Global = map[string]map[string]string{"facebook": {"p": "Misleading claims"}}
	}
	return r, nil
}
func (f *fakeGraph) SetStatus(_ context.Context, _, id, status string) error {
	f.statuses = append(f.statuses, id+":"+status)
	return nil
}

func plan(now time.Time) Plan {
	return Plan{Name: "Holiday", AdAccountID: "act_1", Token: "t", Currency: "KES", Budget: 5000,
		Start: now, End: now.AddDate(0, 0, 7), Objective: "OUTCOME_ENGAGEMENT", PageID: "pg1",
		WhatsAppNumber: "254700000000", Countries: []string{"KE"}, VideoURL: "https://m/v.mp4"}
}

func TestLaunchHappyPathAndResume(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	g := &fakeGraph{failVideo: 1, review: []string{"PENDING_REVIEW", "PAUSED"}}
	var saves []State
	save := func(_ context.Context, st State) error { saves = append(saves, st); return nil }
	funded := false
	check := func(context.Context) (bool, error) { return funded, nil }
	ctx := context.Background()

	st, err := Step(ctx, g, plan(now), State{}, now, check, save)
	if err != nil || st.Stage != StageVideo || st.Attempts != 1 || st.CampaignID != "c1" || st.AdSetID != "as1" {
		t.Fatalf("a failed video upload keeps the campaign and ad set: %+v %v", st, err)
	}
	if g.adSetInput.LifetimeBudget != 500000 || g.adSetInput.DestinationType != "WHATSAPP" || g.adSetInput.OptimizationGoal != "CONVERSATIONS" {
		t.Fatalf("ad set input: %+v", g.adSetInput)
	}
	// The retry resumes at the video: no second campaign.
	st, _ = Step(ctx, g, plan(now), st, now, check, save)
	if st.Stage != StageReview || strings.Count(strings.Join(g.calls, ","), "campaign") != 1 || g.creative.CTA != "WHATSAPP_MESSAGE" {
		t.Fatalf("resume: %+v %v", st, g.calls)
	}
	if g.calls[0] != "campaign:PAUSED" {
		t.Fatalf("the campaign must be created PAUSED: %v", g.calls)
	}
	st, _ = Step(ctx, g, plan(now), st, now, check, save) // still in review
	if st.Stage != StageReview || len(g.statuses) != 0 {
		t.Fatalf("pending review: %+v", st)
	}
	st, _ = Step(ctx, g, plan(now), st, now, check, save) // approved, not funded yet
	if st.Stage != StageFunding || len(g.statuses) != 0 {
		t.Fatalf("approved but unfunded must wait: %+v %v", st, g.statuses)
	}
	funded = true
	st, _ = Step(ctx, g, plan(now), st, now, check, save)
	if st.Stage != StageLive || strings.Join(g.statuses, ",") != "ad1:ACTIVE,as1:ACTIVE,c1:ACTIVE" {
		t.Fatalf("go live: %+v %v", st, g.statuses)
	}
	if len(saves) < 7 {
		t.Fatalf("state must be saved after every step, got %d saves", len(saves))
	}
}

func TestLaunchDisapprovedAndExpired(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	save := func(context.Context, State) error { return nil }
	yes := func(context.Context) (bool, error) { return true, nil }
	g := &fakeGraph{review: []string{"DISAPPROVED"}}
	st, _ := Step(context.Background(), g, plan(now), State{Stage: StageReview, AdID: "ad1"}, now, yes, save)
	if st.Stage != StageFailed || !strings.Contains(st.Error, "Misleading claims") || len(g.statuses) != 0 {
		t.Fatalf("disapproved: %+v", st)
	}
	p := plan(now)
	p.End = now.Add(-time.Hour)
	st, _ = Step(context.Background(), &fakeGraph{}, p, State{Stage: StageFunding, AdID: "ad1"}, now, yes, save)
	if st.Stage != StageFailed {
		t.Fatalf("an expired boost never goes live: %+v", st)
	}
	p = plan(now)
	p.Currency = "XYZ"
	if st, _ = Step(context.Background(), &fakeGraph{}, p, State{}, now, yes, save); st.Stage != StageFailed {
		t.Fatal("an unknown currency fails before any Meta call")
	}
}

func TestLaunchGivesUp(t *testing.T) {
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	save := func(context.Context, State) error { return nil }
	g := &fakeGraph{failVideo: 99}
	st := State{Stage: StageVideo}
	for i := 0; i < MaxLaunchAttempts; i++ {
		st, _ = Step(context.Background(), g, plan(now), st, now, nil, save)
	}
	if st.Stage != StageFailed || !strings.Contains(st.Error, "gave up") {
		t.Fatalf("five failures must fail: %+v", st)
	}
}
