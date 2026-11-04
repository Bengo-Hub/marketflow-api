package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"go.uber.org/zap"
)

// WhatsAppReferralHandler attributes a click-to-WhatsApp lead (Sprint 18). notifications-api
// publishes the first message a person sends from an ad, with Meta's ctwa_clid and the ad id.
// The click id is stored on the Lead once (the first message only, never overwritten); a new
// person becomes a Lead with source "ctwa", linked to the boost that ran the ad, and
// lead.created starts the follow-up.
type WhatsAppReferralHandler struct {
	db     *ent.Client
	leads  *leads.Service
	logger *zap.Logger
}

func NewWhatsAppReferralHandler(db *ent.Client, logger *zap.Logger) *WhatsAppReferralHandler {
	return &WhatsAppReferralHandler{db: db, leads: leads.NewService(db, logger), logger: logger.Named("whatsapp-referral")}
}

func (*WhatsAppReferralHandler) Subject() string { return "notifications.whatsapp.referral" }
func (*WhatsAppReferralHandler) Durable() string { return "marketflow-whatsapp-referral" }

type waReferral struct {
	TenantID    string `json:"tenant_id"`
	From        string `json:"from"`
	ContactName string `json:"contact_name"`
	CtwaClid    string `json:"ctwa_clid"`
	AdID        string `json:"ad_id"`
	Headline    string `json:"headline"`
}

func (h *WhatsAppReferralHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	_, in, err := eventslib.DecodeEvent[waReferral](msg.Data())
	if err != nil || in.CtwaClid == "" || in.From == "" {
		return dropPoison(h.logger, msg, "decode whatsapp.referral", fmt.Errorf("bad event: %v", err))
	}
	tid, err := uuid.Parse(in.TenantID)
	if err != nil {
		return dropPoison(h.logger, msg, "whatsapp.referral tenant", err)
	}
	return h.attribute(ctx, tid, in)
}

func (h *WhatsAppReferralHandler) attribute(ctx context.Context, tid uuid.UUID, in waReferral) error {
	phone := "+" + strings.TrimPrefix(in.From, "+")
	attrib := map[string]any{"ctwa_clid": in.CtwaClid, "ad_id": in.AdID, "ad_headline": in.Headline}
	var campaignID *uuid.UUID
	if in.AdID != "" {
		if c, err := h.db.Campaign.Query().Where(campaign.TenantIDEQ(tid), campaign.MetaAdIDEQ(in.AdID)).First(ctx); err == nil {
			campaignID = &c.ID
			attrib["campaign_id"] = c.ID.String()
			if c.CreativeVersionID != nil {
				attrib["creative_version_id"] = c.CreativeVersionID.String()
			}
		}
	}

	// An existing lead with this phone keeps its first click id; only a lead without one gets it.
	existing, err := h.db.Lead.Query().Where(lead.TenantIDEQ(tid), lead.PhoneEQ(phone)).Order(ent.Desc(lead.FieldCreatedAt)).First(ctx)
	switch {
	case err == nil:
		b, _ := json.Marshal(attrib)
		value := entsql.ExprFunc(func(bd *entsql.Builder) {
			bd.WriteString("(COALESCE(" + lead.FieldMetadata + ", '{}'::jsonb) || ")
			bd.Arg(string(b))
			bd.WriteString("::jsonb)")
		})
		_, err = h.db.Lead.Update().
			Where(lead.IDEQ(existing.ID), func(s *entsql.Selector) {
				s.Where(entsql.Not(entsql.ExprP("jsonb_exists(COALESCE(" + s.C(lead.FieldMetadata) + ", '{}'::jsonb), 'ctwa_clid')")))
			}).
			Modify(func(u *entsql.UpdateBuilder) { u.Set(lead.FieldMetadata, value) }).
			Save(ctx)
		return err
	case !ent.IsNotFound(err):
		return err
	}
	in2 := leads.CreateInput{TenantID: tid, Phone: &phone, Source: "ctwa", LeadType: "customer", CampaignID: campaignID, Metadata: attrib}
	if name := strings.TrimSpace(in.ContactName); name != "" {
		in2.FirstName = &name
	}
	// Opening the chat from an ad lets the business answer inside that conversation, but it is not
	// consent to later WhatsApp marketing (Kenya Data Protection Act), so whatsapp_consent stays
	// unset and the nurture's WhatsApp steps do not fire for this lead.
	if _, err := h.leads.Create(ctx, in2); err != nil {
		return fmt.Errorf("ctwa lead: %w", err)
	}
	return nil
}
