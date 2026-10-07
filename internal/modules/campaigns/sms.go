package campaigns

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Broadcaster hands a bulk send to notifications-api's broadcast engine.
type Broadcaster interface {
	CreateBroadcast(ctx context.Context, req notifications.BroadcastRequest) (*notifications.BroadcastResult, error)
}

// WithBroadcaster wires the broadcast engine used for campaign sends.
func (s *Service) WithBroadcaster(b Broadcaster) *Service {
	s.broadcaster = b
	return s
}

// ErrNoBroadcaster is returned when campaign sends are not wired to notifications-api.
var ErrNoBroadcaster = errors.New("campaign sending is not configured")

// SendSMSInput holds the data needed to send a bulk SMS for a campaign.
type SendSMSInput struct {
	TenantID    uuid.UUID
	CampaignID  uuid.UUID
	SegmentID   *uuid.UUID
	Message     string
	RequestedBy string
}

// SendSMSResult is the broadcast the send became.
type SendSMSResult struct {
	BroadcastID string
	Status      string
}

// SendSMS hands a campaign's bulk SMS to notifications-api's broadcast engine. The engine pages
// the tenant's contacts (or the segment's members) through MarketFlow's reach endpoint, applies
// marketing consent and opt-outs, adds the STOP line, paces the sends, and waits for the
// tenant's approval in notifications-ui. This replaced a one-event-per-contact outbox fan-out
// that loaded every contact at once and ignored the segment.
func (s *Service) SendSMS(ctx context.Context, in SendSMSInput) (*SendSMSResult, error) {
	if s.broadcaster == nil {
		return nil, ErrNoBroadcaster
	}
	camp, err := s.client.Campaign.Query().
		Where(campaign.ID(in.CampaignID), campaign.TenantID(in.TenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("load campaign: %w", err)
	}
	audience := map[string]any{"type": "tenant_customers"}
	if in.SegmentID != nil {
		audience["segment_id"] = in.SegmentID.String()
	}
	res, err := s.broadcaster.CreateBroadcast(ctx, notifications.BroadcastRequest{
		TenantID:    in.TenantID.String(),
		RequestedBy: in.RequestedBy,
		Source:      "marketflow",
		SourceRef:   camp.ID.String(),
		Submit:      true,
		Broadcast: map[string]any{
			"title":    camp.Name,
			"kind":     "marketing",
			"channels": []string{"sms"},
			"content":  map[string]any{"sms": map[string]any{"body": in.Message}},
			"audience": audience,
		},
	})
	if err != nil {
		return nil, err
	}

	meta := map[string]any{}
	for k, v := range camp.Metadata {
		meta[k] = v
	}
	meta["broadcast_id"] = res.ID
	if err := s.client.Campaign.UpdateOneID(camp.ID).SetMetadata(meta).Exec(ctx); err != nil {
		s.logger.Warn("campaign: could not record the broadcast id", zap.Error(err))
	}
	s.logger.Info("campaign sms handed to the broadcast engine",
		zap.String("campaign_id", camp.ID.String()), zap.String("broadcast_id", res.ID))
	return &SendSMSResult{BroadcastID: res.ID, Status: res.Status}, nil
}
