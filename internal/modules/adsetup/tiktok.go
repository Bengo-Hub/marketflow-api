package adsetup

import (
	"context"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
)

// tiktokInput is everything a TikTok checklist needs.
type tiktokInput struct {
	Client       *tiktok.Client
	Token        string
	AdvertiserID string
	Business     string
	// HomeURL and PaymentURL come from the Run ads link settings.
	HomeURL, PaymentURL string
	Cfg                 Config
}

func tiktokChecklist(ctx context.Context, in tiktokInput) []Item {
	var (
		info                      tiktok.AdvertiserInfo
		pixels                    []tiktok.Pixel
		forms                     int
		ads                       []tiktok.AdStatus
		eInfo, ePix, eForms, eAds error
	)
	c, id := in.Client, in.AdvertiserID
	together(
		func() { info, eInfo = c.Advertiser(ctx, in.Token, id) },
		func() { pixels, ePix = c.Pixels(ctx, in.Token, id) },
		func() { forms, eForms = c.LeadPages(ctx, in.Token, id) },
		func() { ads, eAds = c.AdStatuses(ctx, in.Token, id) },
	)
	items := []Item{in.accountItem(info, eInfo), in.fundingItem(info, eInfo), in.pixelItem(pixels, ePix), in.eventItem(pixels, ePix),
		in.formItem(forms, eForms), in.bcItem(info, eInfo), in.policyItem(ads, eAds)}
	return seal(items)
}

func (in tiktokInput) accountItem(a tiktok.AdvertiserInfo, err error) Item {
	it := Item{Key: "tiktok.account", Title: "Ad account active"}
	l := &Link{Label: "Open TikTok Ads Manager", URL: in.HomeURL}
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	switch {
	case a.Status == "STATUS_ENABLE":
		it.Status, it.Detail = StatusOK, "TikTok has this ad account active."
	case a.Status == "":
		it.Status, it.Link, it.Detail = StatusUnknown, l, "TikTok did not say whether this ad account is active. Open Ads Manager to check."
	case strings.Contains(a.Status, "PENDING") || strings.Contains(a.Status, "UNAUDITED"):
		it.Status, it.Link, it.Detail = StatusWarning, l, "TikTok is still reviewing this ad account. Ads start once it is approved."
	default:
		it.Status, it.Link = StatusAction, l
		it.Detail = "TikTok reports this ad account as " + strings.ToLower(words(strings.TrimPrefix(a.Status, "STATUS_"))) + ", so ads may not run."
		if a.RejectReason != "" {
			it.Detail += " Reason: " + clip(a.RejectReason, 200)
		}
	}
	return it
}

func (in tiktokInput) fundingItem(a tiktok.AdvertiserInfo, err error) Item {
	it := Item{Key: "tiktok.funding", Title: "Balance and payment"}
	l := &Link{Label: "Open TikTok payment page", URL: in.PaymentURL}
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	it.Link = l
	switch {
	case a.Balance == nil:
		it.Status, it.Detail = StatusUnknown, "TikTok does not show MarketFlow the balance of this ad account. Check it on the payment page before a campaign starts."
	case *a.Balance > 0:
		it.Status, it.Link, it.Detail = StatusOK, nil, fmt.Sprintf("The balance is %s %.2f.", a.Currency, *a.Balance)
	default:
		it.Status, it.Detail = StatusWarning, "The balance is zero. Add money or an automatic payment card on the payment page before you run ads. With an automatic card, a zero balance is normal."
	}
	return it
}

func (in tiktokInput) pixelItem(pixels []tiktok.Pixel, err error) Item {
	it := Item{Key: "tiktok.pixel", Title: "Pixel"}
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, in.Cfg.link("tiktok_events", "Open Events Manager", in.AdvertiserID))
	}
	if len(pixels) > 0 {
		it.Status, it.Detail = StatusOK, "This ad account has the pixel \""+pixels[0].Name+"\"."
		return it
	}
	it.Status, it.Detail = StatusWarning, "This ad account has no pixel, so TikTok cannot learn what people do on your website."
	it.Fix = &Fix{Label: "Create the pixel", Changes: []string{
		"Create one pixel on this ad account.",
		"No ad is created. The pixel collects nothing until it is added to your website.",
	}, Fields: []Field{{Key: "name", Label: "Pixel name", Value: clip(strings.TrimSpace(in.Business+" pixel"), 40), Max: 40, Required: true}},
		run: func(ctx context.Context, v map[string]string) error {
			return in.Client.CreatePixel(ctx, in.Token, in.AdvertiserID, v["name"])
		}}
	return it
}

func (in tiktokInput) eventItem(pixels []tiktok.Pixel, err error) Item {
	it := Item{Key: "tiktok.events", Title: "Pixel events"}
	l := in.Cfg.link("tiktok_events", "Open Events Manager", in.AdvertiserID)
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	if len(pixels) == 0 {
		it.Status, it.Detail = StatusWarning, "First create the pixel (the check above)."
		return it
	}
	it.Link = l
	for _, p := range pixels {
		if p.Activity == "ACTIVE" && len(p.Events) > 0 {
			it.Status, it.Link, it.Detail = StatusOK, nil, fmt.Sprintf("The pixel is active with %d events set up.", len(p.Events))
			return it
		}
	}
	it.Status = StatusWarning
	it.Detail = "The pixel is not active or has no events. Add it to your website and set up the event Submit form; TikTok's Events Manager walks you through it."
	return it
}

func (in tiktokInput) formItem(n int, err error) Item {
	it := Item{Key: "tiktok.lead_form", Title: "Lead form (instant form)"}
	l := in.Cfg.link("tiktok_forms", "Open instant forms", in.AdvertiserID)
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	if n > 0 {
		it.Status, it.Detail = StatusOK, fmt.Sprintf("This ad account has %d published lead forms.", n)
		return it
	}
	it.Status, it.Link = StatusWarning, l
	it.Detail = "This ad account has no published lead form. TikTok only lets you build one in Ads Manager."
	return it
}

func (in tiktokInput) bcItem(a tiktok.AdvertiserInfo, err error) Item {
	it := Item{Key: "tiktok.business_center", Title: "Business Center"}
	l := in.Cfg.link("tiktok_bc", "Open Business Center", in.AdvertiserID)
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	if a.OwnerBC != "" {
		it.Status, it.Detail = StatusOK, "A Business Center owns this ad account."
		return it
	}
	it.Status, it.Link = StatusInfo, l
	it.Detail = "TikTok shows no Business Center for this ad account. You can run ads without one; it helps when several people share the account."
	return it
}

func (in tiktokInput) policyItem(ads []tiktok.AdStatus, err error) Item {
	it := Item{Key: "tiktok.policy", Title: "Ads within TikTok's policies"}
	l := &Link{Label: "Open TikTok Ads Manager", URL: in.HomeURL}
	if err != nil {
		return unread(it.Key, it.Title, "TikTok", err, l)
	}
	for _, a := range ads {
		if strings.Contains(a.Status, "DENY") || strings.Contains(a.Status, "REJECT") || strings.Contains(a.Status, "DISAPPROVE") {
			it.Points = append(it.Points, a.Name)
		}
	}
	if len(it.Points) == 0 {
		it.Status, it.Detail = StatusOK, "TikTok reports no rejected ad."
		return it
	}
	it.Status, it.Link, it.Detail = StatusAction, l, "TikTok rejected these ads. Open one to see why and edit it."
	return it
}
