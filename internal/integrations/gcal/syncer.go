package gcal

import (
	"context"
	"fmt"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"go.uber.org/zap"
)

// Syncer implements meetings.CalendarSyncer by pushing the meeting into the
// tenant's Google Calendar. It pulls credentials from the ProviderSetting
// store: the platform-level OAuth client + the tenant's refresh_token.
type Syncer struct {
	db     *ent.Client
	logger *zap.Logger
}

// NewSyncer constructs a Syncer.
func NewSyncer(db *ent.Client, logger *zap.Logger) *Syncer {
	return &Syncer{db: db, logger: logger.Named("gcal.syncer")}
}

// Sync pushes the meeting to the tenant's Google Calendar. Returns nil
// (without an error) when the tenant hasn't linked their calendar — the
// local record remains authoritative and the caller proceeds normally.
func (s *Syncer) Sync(ctx context.Context, m *ent.ScheduledMeeting) (*meetings.SyncResult, error) {
	if m == nil {
		return nil, nil
	}
	if m.AttendeeEmail == nil || *m.AttendeeEmail == "" {
		// No attendee to invite; don't bother with calendar sync.
		return nil, nil
	}

	// Platform OAuth client (shared).
	platform, err := s.loadPlatformCreds(ctx)
	if err != nil {
		s.logger.Debug("gcal platform creds not configured", zap.Error(err))
		return nil, nil
	}
	// Tenant refresh token + calendar id.
	tenant, err := s.loadTenantCreds(ctx, m.TenantID.String())
	if err != nil {
		s.logger.Debug("tenant has not linked Google Calendar",
			zap.String("tenant", m.TenantID.String()))
		return nil, nil
	}

	cli := New(platform, tenant)
	in := EventInput{
		Title:          fmt.Sprintf("MarketFlow: %s", humanMeetingType(m.MeetingType)),
		DurationMins:   m.DurationMinutes,
		StartsAt:       m.ScheduledAt,
		CreateMeetLink: m.MeetingURL == nil || *m.MeetingURL == "",
	}
	if m.AttendeeEmail != nil {
		in.AttendeeEmail = *m.AttendeeEmail
	}
	if m.AttendeeName != nil {
		in.AttendeeName = *m.AttendeeName
	}
	if m.OrganizerEmail != nil {
		in.OrganizerEmail = *m.OrganizerEmail
	}
	if m.Notes != nil {
		in.Description = *m.Notes
	}

	res, err := cli.CreateEvent(ctx, in)
	if err != nil {
		return nil, err
	}
	return &meetings.SyncResult{
		EventID:    res.EventID,
		CalendarID: res.CalendarID,
		HTMLLink:   res.HTMLLink,
		MeetURL:    res.MeetURL,
	}, nil
}

func (s *Syncer) loadPlatformCreds(ctx context.Context) (PlatformCreds, error) {
	rows, err := s.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ("google_calendar"),
			providersetting.ProviderNameEQ("oauth"),
		).
		All(ctx)
	if err != nil {
		return PlatformCreds{}, err
	}
	out := PlatformCreds{}
	for _, r := range rows {
		switch r.Key {
		case "client_id":
			out.ClientID = r.Value
		case "client_secret":
			out.ClientSecret = r.Value
		case "redirect_url":
			out.RedirectURL = r.Value
		}
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		return PlatformCreds{}, fmt.Errorf("gcal: platform oauth not configured")
	}
	return out, nil
}

func (s *Syncer) loadTenantCreds(ctx context.Context, tenantID string) (TenantCreds, error) {
	rows, err := s.db.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ(tenantID),
			providersetting.ProviderTypeEQ("google_calendar"),
			providersetting.ProviderNameEQ("oauth"),
		).
		All(ctx)
	if err != nil {
		return TenantCreds{}, err
	}
	out := TenantCreds{}
	for _, r := range rows {
		switch r.Key {
		case "refresh_token":
			out.RefreshToken = r.Value
		case "calendar_id":
			out.CalendarID = r.Value
		}
	}
	if out.RefreshToken == "" {
		return TenantCreds{}, fmt.Errorf("gcal: tenant has no refresh_token")
	}
	return out, nil
}

func humanMeetingType(t string) string {
	switch t {
	case "product_demo":
		return "Product demo"
	case "business_opportunity":
		return "Business opportunity call"
	case "follow_up":
		return "Follow-up"
	case "onboarding":
		return "Onboarding"
	default:
		if t == "" {
			return "Meeting"
		}
		return t
	}
}
