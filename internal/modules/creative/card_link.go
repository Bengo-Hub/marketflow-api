package creative

import (
	"context"
	"fmt"
	"strings"

	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/shortlink"
	"github.com/bengobox/marketflow-api/internal/modules/shortlinks"
	"github.com/bengobox/marketflow-api/internal/platform/postcaption"
)

// The end card shows the ad's lead page in a short form a person can read and type (owner,
// 2026-11-25). It is drawn on the card only: no script line carries it, so it is never spoken.

// CardLinkKey is "off" to leave the lead page off end cards; a tenant's row wins.
const CardLinkKey = "creative.end_card_lead_link"

// cardLinkMax is the longest address the end card draws in one readable line (marketflow-ai).
const cardLinkMax = 48

// cardLinkKind is marketflow-ai's media.ContactPage.
const cardLinkKind = "page"

// WithCardLink puts each ad's lead page on its end card.
func (r *Renderer) WithCardLink(k *PostKit) *Renderer {
	r.cardKit = k
	return r
}

// CardLink is the ad's lead page as the end card shows it, and the page it stands for ("" without a
// published page): the page's own address when short, else a short link to it.
func (k *PostKit) CardLink(ctx context.Context, v *ent.CreativeVersion) (shown, page string) {
	b := k.LeadPage(ctx, v.TenantID, v.BriefID)
	if b == nil {
		return "", ""
	}
	tagged := postcaption.LeadURL(k.Base, k.publishedSlug(ctx, b), b.ID.String(), "end_card", "video")
	if tagged == "" {
		return "", ""
	}
	page, _, _ = strings.Cut(tagged, "?")
	if shown = bareURL(page); len(shown) <= cardLinkMax {
		return shown, page
	}
	code, err := k.shortCode(ctx, b, tagged)
	if err != nil {
		k.Svc.logger.Warn("short link for an end card not made", zap.String("brief_id", b.ID.String()), zap.Error(err))
		return "", ""
	}
	if shown = bareURL(strings.TrimRight(k.Base, "/") + "/s/" + code); len(shown) > cardLinkMax {
		return "", ""
	}
	return shown, page
}

// shortCode is the ad's one short link to its lead page. Short link targets never change, so a
// second call finds the first.
func (k *PostKit) shortCode(ctx context.Context, b *ent.CreativeBrief, target string) (string, error) {
	code := ""
	err := withTx(ctx, k.Svc.db, func(tx *ent.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "end-card-link:"+b.ID.String()); err != nil {
			return fmt.Errorf("lock short link: %w", err)
		}
		l, err := tx.ShortLink.Query().Where(shortlink.TenantID(b.TenantID), shortlink.TargetURL(target), shortlink.IsActive(true), shortlink.ExpiresAtIsNil()).First(ctx)
		if ent.IsNotFound(err) {
			title := "End card: " + excerpt(b.Title, 100)
			l, err = shortlinks.NewService(tx.Client(), k.Svc.logger, k.Base).Create(ctx, shortlinks.CreateInput{
				TenantID: b.TenantID, TargetURL: target, LinkType: "funnel", Title: &title})
		}
		if err != nil {
			return err
		}
		code = l.Code
		return nil
	})
	return code, err
}

// cardContacts are the end card's contacts, with the ad's lead page last unless the card already
// shows it. A pharmacy's medicine ad runs as the Board approved it, so nothing is added to it.
func (r *Renderer) cardContacts(ctx context.Context, v *ent.CreativeVersion, place placeInfo) []contact {
	if r.cardKit == nil || strings.EqualFold(svcconfigOr(ctx, r.db, v.TenantID, CardLinkKey, "on"), "off") {
		return place.Contacts
	}
	st, err := r.cardKit.Svc.PPBStatusForBrief(ctx, v.TenantID, v.BriefID)
	if err != nil {
		r.logger.Warn("end card link left out: the pharmacy check failed", zap.String("version_id", v.ID.String()), zap.Error(err))
		return place.Contacts
	}
	if st.Required {
		return place.Contacts
	}
	shown, page := r.cardKit.CardLink(ctx, v)
	if shown == "" {
		return place.Contacts
	}
	for _, c := range place.Contacts {
		if s := bareURL(c.Value); s == bareURL(shown) || s == bareURL(page) {
			return place.Contacts
		}
	}
	return append(append([]contact{}, place.Contacts...), contact{Kind: cardLinkKind, Value: shown})
}

// bareURL is an address as a person reads it: no scheme, www, query or closing slash. Only the host
// is lower-cased, because a short link's code keeps its case.
func bareURL(s string) string {
	s = strings.TrimSpace(s)
	if _, rest, ok := strings.Cut(s, "://"); ok {
		s = rest
	}
	s, _, _ = strings.Cut(s, "?")
	s, _, _ = strings.Cut(s, "#")
	host, path, _ := strings.Cut(strings.TrimRight(s, "/"), "/")
	host = strings.TrimPrefix(strings.ToLower(host), "www.")
	if path == "" {
		return host
	}
	return host + "/" + path
}
