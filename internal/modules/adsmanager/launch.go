package adsmanager

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// Graph is the part of the Meta client a launch uses (faked in tests).
type Graph interface {
	CreateCampaign(ctx context.Context, token, actID string, in meta.CampaignInput) (*meta.CampaignResponse, error)
	CreateAdSet(ctx context.Context, token, actID string, in meta.AdSetInput) (string, error)
	UploadAdVideo(ctx context.Context, token, actID, fileURL, title string) (string, error)
	CreateAdCreative(ctx context.Context, token, actID string, in meta.CreativeInput) (string, error)
	CreateAd(ctx context.Context, token, actID, name, adSetID, creativeID string) (string, error)
	CreateLeadForm(ctx context.Context, pageToken, pageID string, in meta.LeadFormInput) (string, error)
	GetAdReview(ctx context.Context, token, adID string) (*meta.AdReview, error)
	SetStatus(ctx context.Context, token, objectID, status string) error
}

// Launch stages, kept in Campaign.metadata.launch.stage. Each Meta object id is saved the moment
// it exists, so a crash between steps resumes without creating a second campaign or ad.
const (
	StageCampaign = "campaign"
	StageAdSet    = "adset"
	StageVideo    = "video"
	StageCreative = "creative"
	StageAd       = "ad"
	StageExtras   = "extras"  // the extra creatives, one ad each in the same ad set
	StageReview   = "review"  // Meta is reviewing the PAUSED ad
	StageFunding  = "funding" // approved, waiting for the money to be in place
	StageLive     = "live"    // set ACTIVE
	StageFailed   = "failed"
)

// Plan is everything a launch needs, fixed when the owner confirms the quote.
type Plan struct {
	Name           string
	AdAccountID    string // act_...
	Token          string
	Currency       string
	Budget         int64 // whole units
	Start, End     time.Time
	Objective      string // OUTCOME_ENGAGEMENT (click to WhatsApp) | OUTCOME_LEADS | OUTCOME_TRAFFIC
	PageID         string
	InstagramID    string
	WhatsAppNumber string
	Countries      []string
	AgeMin, AgeMax int
	VideoURL       string
	ThumbnailURL   string
	Message        string
	Headline       string
	Link           string // for OUTCOME_TRAFFIC
	Welcome        string // WhatsApp greeting
	// OUTCOME_LEADS runs a Meta instant form (owner, 2026-11-10): made with the Page's token,
	// linked to the advertiser's privacy policy.
	PageToken  string
	PrivacyURL string
	// Extras are more approved creatives run as more ads in the same ad set, sharing its lifetime
	// budget (the approved total), so the optimiser can pause losers (away decision 23).
	Extras []Variant
}

// Variant is one extra creative of a boost.
type Variant struct {
	VideoURL     string
	ThumbnailURL string
	Message      string
	Headline     string
}

// ExtraState is the launch progress of one extra creative.
type ExtraState struct {
	VideoID    string `json:"meta_video_id,omitempty"`
	CreativeID string `json:"meta_creative_id,omitempty"`
	AdID       string `json:"meta_ad_id,omitempty"`
	Review     string `json:"review_status,omitempty"`
}

// State is the stored launch progress.
type State struct {
	Stage      string    `json:"stage"`
	CampaignID string    `json:"meta_campaign_id,omitempty"`
	AdSetID    string    `json:"meta_adset_id,omitempty"`
	VideoID    string    `json:"meta_video_id,omitempty"`
	FormID     string    `json:"meta_lead_form_id,omitempty"`
	CreativeID string    `json:"meta_creative_id,omitempty"`
	AdID       string    `json:"meta_ad_id,omitempty"`
	Review     string    `json:"review_status,omitempty"`
	Reasons    []string  `json:"review_reasons,omitempty"`
	Error      string    `json:"error,omitempty"`
	Attempts   int       `json:"attempts,omitempty"`
	NextAt     time.Time `json:"next_at,omitempty"`
	PaidOK     bool      `json:"paid_confirmed,omitempty"`
	// FundingSince is when the boost first waited for funds (Sprint 18 L29).
	FundingSince time.Time `json:"funding_since,omitempty"`
	// FundingSeen is what the last funding check read on Meta, for the Add funds step (L62).
	FundingSeen *FundingSeen `json:"funding_seen,omitempty"`
	// Extras follow Plan.Extras by index.
	Extras []ExtraState `json:"extras,omitempty"`
	LiveAt time.Time    `json:"live_at,omitempty"`
}

// FundingSeen is the ad account's funding as last read from Meta while a boost waits for money:
// the balance when Meta shows one (prepaid accounts), what is still missing, and when it was read.
type FundingSeen struct {
	Funding   Funding   `json:"funding"`
	Balance   int64     `json:"balance"`   // whole units; 0 when Meta shows none
	Shortfall int64     `json:"shortfall"` // whole units still to add
	Currency  string    `json:"currency,omitempty"`
	At        time.Time `json:"at"`
}

// seenFrom records a fresh quote as the funding last seen.
func seenFrom(q Quote, now time.Time) *FundingSeen {
	return &FundingSeen{Funding: q.Funding, Balance: q.Balance, Shortfall: q.Shortfall, Currency: q.Currency, At: now.UTC()}
}

// Saver stores the state after every step.
type Saver func(ctx context.Context, st State) error

// FundingCheck re-reads the account's funding (the quote's Ready) just before going live.
type FundingCheck func(ctx context.Context) (bool, error)

// Launch limits.
const (
	MaxLaunchAttempts = 5
	ReviewPoll        = 5 * time.Minute
	FundingPoll       = time.Minute
	// After FundingFastFor of waiting, funds are checked every FundingPollSlow until the boost ends.
	FundingFastFor  = 30 * time.Minute
	FundingPollSlow = 15 * time.Minute
)

// Step moves a launch forward until it has to wait (review, funding, a retry) or ends. It
// returns only a Saver error; Meta errors are kept on the state.
func Step(ctx context.Context, g Graph, p Plan, st State, now time.Time, funded FundingCheck, save Saver) (State, error) {
	if st.Stage == "" {
		st.Stage = StageCampaign
	}
	fail := func(msg string) (State, error) {
		st.Stage, st.Error = StageFailed, msg
		return st, save(ctx, st)
	}
	retry := func(err error) (State, error) {
		st.Attempts++
		st.Error = err.Error()
		if st.Attempts >= MaxLaunchAttempts {
			return fail("gave up after " + fmt.Sprint(st.Attempts) + " tries: " + err.Error())
		}
		wait := time.Minute << (st.Attempts - 1)
		var ge *meta.GraphError
		if errors.As(err, &ge) && ge.RateLimited() {
			wait = time.Hour
		}
		st.NextAt = now.Add(wait)
		return st, save(ctx, st)
	}
	advance := func(next string) error {
		st.Stage, st.Attempts, st.Error = next, 0, ""
		return save(ctx, st)
	}
	budget, err := ToMinor(p.Budget, p.Currency)
	if err != nil {
		return fail(err.Error())
	}

	for {
		switch st.Stage {
		case StageCampaign:
			c, err := g.CreateCampaign(ctx, p.Token, p.AdAccountID, meta.CampaignInput{Name: p.Name, Objective: p.Objective, Status: "PAUSED"})
			if err != nil {
				return retry(err)
			}
			st.CampaignID = c.ID
			if err := advance(StageAdSet); err != nil {
				return st, err
			}
		case StageAdSet:
			in := meta.AdSetInput{Name: p.Name, CampaignID: st.CampaignID, LifetimeBudget: budget, Start: p.Start, End: p.End,
				PageID: p.PageID, Countries: p.Countries, AgeMin: p.AgeMin, AgeMax: p.AgeMax}
			switch p.Objective {
			case "OUTCOME_ENGAGEMENT":
				in.OptimizationGoal, in.DestinationType, in.WhatsAppNumber = "CONVERSATIONS", "WHATSAPP", p.WhatsAppNumber
			case "OUTCOME_LEADS":
				in.OptimizationGoal, in.DestinationType = "LEAD_GENERATION", "ON_AD"
			default:
				in.OptimizationGoal = "LINK_CLICKS"
			}
			id, err := g.CreateAdSet(ctx, p.Token, p.AdAccountID, in)
			if err != nil {
				return retry(err)
			}
			st.AdSetID = id
			if err := advance(StageVideo); err != nil {
				return st, err
			}
		case StageVideo:
			id, err := g.UploadAdVideo(ctx, p.Token, p.AdAccountID, p.VideoURL, p.Name)
			if err != nil {
				return retry(err)
			}
			st.VideoID = id
			if err := advance(StageCreative); err != nil {
				return st, err
			}
		case StageCreative:
			cta := "LEARN_MORE"
			if p.Objective == "OUTCOME_ENGAGEMENT" {
				cta = "WHATSAPP_MESSAGE"
			} else if p.Objective == "OUTCOME_LEADS" {
				cta = "SIGN_UP"
				// The instant form is made once and saved before the creative, so a creative
				// that must wait for Meta's video processing never makes a second form.
				if st.FormID == "" {
					id, err := g.CreateLeadForm(ctx, p.PageToken, p.PageID, meta.LeadFormInput{
						Name: p.Name + " form", PrivacyURL: p.PrivacyURL, Questions: []string{"FULL_NAME", "PHONE", "EMAIL"},
						AskWhatsAppConsent: true})
					if err != nil {
						return retry(err)
					}
					st.FormID = id
					if err := save(ctx, st); err != nil {
						return st, err
					}
				}
			}
			id, err := g.CreateAdCreative(ctx, p.Token, p.AdAccountID, meta.CreativeInput{Name: p.Name, PageID: p.PageID,
				InstagramUserID: p.InstagramID, VideoID: st.VideoID, ThumbnailURL: p.ThumbnailURL, Message: p.Message,
				Title: p.Headline, CTA: cta, Link: p.Link, WelcomeMessage: p.Welcome, LeadFormID: st.FormID})
			if err != nil {
				// A video Meta is still processing fails the creative; the retry waits for it.
				return retry(err)
			}
			st.CreativeID = id
			if err := advance(StageAd); err != nil {
				return st, err
			}
		case StageAd:
			id, err := g.CreateAd(ctx, p.Token, p.AdAccountID, p.Name, st.AdSetID, st.CreativeID)
			if err != nil {
				return retry(err)
			}
			st.AdID = id
			if len(p.Extras) > 0 {
				if err := advance(StageExtras); err != nil {
					return st, err
				}
				continue
			}
			st.NextAt = now.Add(ReviewPoll)
			if err := advance(StageReview); err != nil {
				return st, err
			}
			return st, nil // review takes time; come back later
		case StageExtras:
			// One ad per extra creative in the same ad set, saved after each Meta call so a retry
			// resumes where it stopped.
			for len(st.Extras) < len(p.Extras) {
				st.Extras = append(st.Extras, ExtraState{})
			}
			for i, v := range p.Extras {
				x := &st.Extras[i]
				if x.VideoID == "" {
					id, err := g.UploadAdVideo(ctx, p.Token, p.AdAccountID, v.VideoURL, p.Name)
					if err != nil {
						return retry(err)
					}
					x.VideoID = id
					if err := save(ctx, st); err != nil {
						return st, err
					}
				}
				if x.CreativeID == "" {
					in := extraCreativeInput(p, st, v, x.VideoID)
					id, err := g.CreateAdCreative(ctx, p.Token, p.AdAccountID, in)
					if err != nil {
						return retry(err)
					}
					x.CreativeID = id
					if err := save(ctx, st); err != nil {
						return st, err
					}
				}
				if x.AdID == "" {
					id, err := g.CreateAd(ctx, p.Token, p.AdAccountID, p.Name+" "+strconv.Itoa(i+2), st.AdSetID, x.CreativeID)
					if err != nil {
						return retry(err)
					}
					x.AdID = id
					if err := save(ctx, st); err != nil {
						return st, err
					}
				}
			}
			st.NextAt = now.Add(ReviewPoll)
			if err := advance(StageReview); err != nil {
				return st, err
			}
			return st, nil
		case StageReview:
			// The extras first: a rejected extra is dropped, a pending one is waited for.
			for i := range st.Extras {
				x := &st.Extras[i]
				if x.AdID == "" || x.Review == "DISAPPROVED" || x.Review == "WITH_ISSUES" {
					continue
				}
				r, err := g.GetAdReview(ctx, p.Token, x.AdID)
				if err != nil {
					return retry(err)
				}
				x.Review = r.EffectiveStatus
				switch r.EffectiveStatus {
				case "PENDING_REVIEW", "IN_PROCESS", "PENDING_BILLING_INFO", "":
					st.NextAt = now.Add(ReviewPoll)
					return st, save(ctx, st)
				}
			}
			r, err := g.GetAdReview(ctx, p.Token, st.AdID)
			if err != nil {
				return retry(err)
			}
			st.Review = r.EffectiveStatus
			switch r.EffectiveStatus {
			case "DISAPPROVED":
				st.Reasons = r.Reasons()
				return fail("Meta did not approve the ad: " + strings.Join(st.Reasons, "; "))
			case "PENDING_REVIEW", "IN_PROCESS", "PENDING_BILLING_INFO", "":
				st.NextAt = now.Add(ReviewPoll)
				return st, save(ctx, st)
			case "WITH_ISSUES":
				return fail("Meta reports issues with the ad; open it in Ads Manager to see them")
			}
			// Back to the status it was created with (PAUSED), or PREAPPROVED: approved.
			st.NextAt = now
			if err := advance(StageFunding); err != nil {
				return st, err
			}
		case StageFunding:
			if !p.End.After(now) {
				return fail("the boost's end date passed before it could start")
			}
			ok, err := funded(ctx)
			if err != nil {
				return retry(err)
			}
			if !ok && !st.PaidOK {
				if st.FundingSince.IsZero() {
					st.FundingSince = now
				}
				poll := FundingPoll
				if now.Sub(st.FundingSince) >= FundingFastFor {
					poll = FundingPollSlow
				}
				st.NextAt = now.Add(poll)
				return st, save(ctx, st)
			}
			// Ads first (the main ad and every approved extra), then ad set, then campaign: nothing
			// delivers until the last switch.
			ids := []string{st.AdID}
			for _, x := range st.Extras {
				if x.AdID != "" && x.Review != "DISAPPROVED" && x.Review != "WITH_ISSUES" {
					ids = append(ids, x.AdID)
				}
			}
			for _, id := range append(ids, st.AdSetID, st.CampaignID) {
				if err := g.SetStatus(ctx, p.Token, id, "ACTIVE"); err != nil {
					return retry(err)
				}
			}
			st.LiveAt = now
			if err := advance(StageLive); err != nil {
				return st, err
			}
			return st, nil
		default: // live or failed
			return st, nil
		}
	}
}

// creativeCTA is the call to action for a boost objective.
func creativeCTA(objective string) string {
	switch objective {
	case "OUTCOME_ENGAGEMENT":
		return "WHATSAPP_MESSAGE"
	case "OUTCOME_LEADS":
		return "SIGN_UP"
	}
	return "LEARN_MORE"
}

// extraCreativeInput is an extra creative: the boost's Page, link, form and greeting with the
// variant's own video, thumbnail and text (falling back to the boost's text when empty).
func extraCreativeInput(p Plan, st State, v Variant, videoID string) meta.CreativeInput {
	msg, title := v.Message, v.Headline
	if msg == "" {
		msg = p.Message
	}
	if title == "" {
		title = p.Headline
	}
	return meta.CreativeInput{Name: p.Name, PageID: p.PageID, InstagramUserID: p.InstagramID, VideoID: videoID,
		ThumbnailURL: v.ThumbnailURL, Message: msg, Title: title, CTA: creativeCTA(p.Objective), Link: p.Link,
		WelcomeMessage: p.Welcome, LeadFormID: st.FormID}
}
