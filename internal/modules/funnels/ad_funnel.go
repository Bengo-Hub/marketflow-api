package funnels

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// AdFunnelInput is what an ad knows about itself. Only these facts reach the page, so it never
// promises more than the ad does.
type AdFunnelInput struct {
	TenantID    uuid.UUID
	BriefID     uuid.UUID
	Title       string
	Offering    string
	Description string
	ImageURL    string
	Language    string // en | sw
}

// AdFunnel is the published page an ad links to.
type AdFunnel struct {
	ID   uuid.UUID `json:"id"`
	Slug string    `json:"slug"`
}

// CreateForAd publishes a lead page for a new ad: the ad's own headline and photo over a name and
// phone form. It is built from a template, so it costs nothing, and the owner can edit or archive
// it like any funnel. The slug carries part of the ad's id, so it is unique across businesses.
func (s *Service) CreateForAd(ctx context.Context, in AdFunnelInput) (AdFunnel, error) {
	headline := firstNonEmpty(in.Offering, in.Title)
	if headline == "" {
		return AdFunnel{}, fmt.Errorf("ad funnel: the ad has no title")
	}
	meta := map[string]any{"source": "ad", "brief_id": in.BriefID.String(), "headline": clipRunes(headline, 120)}
	if d := strings.TrimSpace(in.Description); d != "" {
		meta["subheadline"] = clipRunes(d, 240)
	}
	if u, err := url.Parse(in.ImageURL); err == nil && u.Scheme == "https" && u.Host != "" {
		meta["hero_image_url"] = in.ImageURL
	}
	kind := "generic"
	if strings.TrimSpace(in.Offering) != "" {
		kind = "product_inquiry"
	}
	f, err := s.Create(ctx, CreateInput{
		TenantID:   in.TenantID,
		Name:       clipRunes("Ad: "+firstNonEmpty(in.Title, headline), 120),
		Slug:       adSlug(headline, in.BriefID),
		FunnelType: &kind,
		Steps:      []map[string]any{leadForm(in.Language)},
		Metadata:   meta,
	})
	if err != nil {
		return AdFunnel{}, err
	}
	if _, err := f.Update().SetStatus("published").SetPublishedAt(time.Now()).Save(ctx); err != nil {
		return AdFunnel{}, fmt.Errorf("publish ad funnel: %w", err)
	}
	return AdFunnel{ID: f.ID, Slug: f.Slug}, nil
}

// ForAds adapts CreateForAd to creative.LeadFunnelMaker without the two packages importing
// each other.
func (s *Service) ForAds() func(ctx context.Context, tenantID, briefID uuid.UUID, title, offering, description, imageURL, language string) (uuid.UUID, string, error) {
	return func(ctx context.Context, tenantID, briefID uuid.UUID, title, offering, description, imageURL, language string) (uuid.UUID, string, error) {
		f, err := s.CreateForAd(ctx, AdFunnelInput{TenantID: tenantID, BriefID: briefID, Title: title, Offering: offering,
			Description: description, ImageURL: imageURL, Language: language})
		return f.ID, f.Slug, err
	}
}

// leadForm matches the portal's default form block.
func leadForm(language string) map[string]any {
	title, cta, name, phone := "Leave your details", "Send", "Name", "Phone or WhatsApp"
	if language == "sw" {
		title, cta, name, phone = "Acha maelezo yako", "Tuma", "Jina", "Simu au WhatsApp"
	}
	return map[string]any{
		"block_type": "form",
		"title":      title,
		"cta_text":   cta,
		"fields": []map[string]any{
			{"key": "first_name", "label": name, "type": "text", "required": true},
			{"key": "phone", "label": phone, "type": "tel", "required": true},
			{"key": "email", "label": "Email", "type": "email"},
		},
	}
}

// adSlug is the headline in link form plus six characters of the ad's id.
func adSlug(headline string, briefID uuid.UUID) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(headline) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	base := strings.Trim(b.String(), "-")
	if base == "" {
		base = "ad"
	}
	return base + "-" + strings.ReplaceAll(briefID.String(), "-", "")[:6]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func clipRunes(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}
