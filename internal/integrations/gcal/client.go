// Package gcal integrates Google Calendar for meeting creation.
//
// Credentials model:
//   - Platform-level (shared across all tenants): Google OAuth client ID,
//     client secret, and redirect URL are stored as a ProviderSetting row
//     with tenant_id="platform", provider_type="google_calendar".
//   - Tenant-level: each tenant that links their Google Calendar stores a
//     refresh_token + chosen calendar_id (default "primary") as a ProviderSetting
//     row with tenant_id=<uuid>. No access token is stored — the client
//     refreshes on demand using the refresh_token.
//
// The agentic RAG loop calls marketflow-api's POST /api/v1/meetings with the
// platform API key; that handler builds a ScheduledMeeting and invokes
// gcal.Client.CreateEvent() which pushes the event to the tenant's calendar.
package gcal

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"golang.org/x/oauth2"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// PlatformCreds holds the shared OAuth client configuration.
type PlatformCreds struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
}

// TenantCreds holds per-tenant refresh token + chosen calendar.
type TenantCreds struct {
	RefreshToken string
	CalendarID   string // "primary" if empty
}

// EventInput describes a calendar event to create.
type EventInput struct {
	Title          string
	Description    string
	StartsAt       time.Time
	DurationMins   int
	AttendeeEmail  string
	AttendeeName   string
	OrganizerEmail string
	Location       string
	CreateMeetLink bool // attach a Google Meet conference
}

// EventResult is what the Google API returned.
type EventResult struct {
	EventID    string
	CalendarID string
	HTMLLink   string
	MeetURL    string
}

// Client wraps a per-tenant Calendar service. Construct one per tenant call.
type Client struct {
	platform PlatformCreds
	tenant   TenantCreds
}

// New constructs a Client. CalendarID falls back to "primary".
func New(platform PlatformCreds, tenant TenantCreds) *Client {
	if tenant.CalendarID == "" {
		tenant.CalendarID = "primary"
	}
	return &Client{platform: platform, tenant: tenant}
}

// CreateEvent pushes an event to the tenant's Google Calendar.
func (c *Client) CreateEvent(ctx context.Context, in EventInput) (*EventResult, error) {
	if c.platform.ClientID == "" || c.platform.ClientSecret == "" {
		return nil, fmt.Errorf("gcal: platform OAuth client not configured")
	}
	if c.tenant.RefreshToken == "" {
		return nil, fmt.Errorf("gcal: tenant has not linked a Google Calendar")
	}

	conf := &oauth2.Config{
		ClientID:     c.platform.ClientID,
		ClientSecret: c.platform.ClientSecret,
		RedirectURL:  c.platform.RedirectURL,
		Scopes:       []string{calendar.CalendarEventsScope},
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.google.com/o/oauth2/auth",
			TokenURL: "https://oauth2.googleapis.com/token",
		},
	}
	tok := &oauth2.Token{RefreshToken: c.tenant.RefreshToken}
	httpClient := conf.Client(ctx, tok)

	svc, err := calendar.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("gcal service: %w", err)
	}

	end := in.StartsAt.Add(time.Duration(in.DurationMins) * time.Minute)
	ev := &calendar.Event{
		Summary:     in.Title,
		Description: in.Description,
		Location:    in.Location,
		Start:       &calendar.EventDateTime{DateTime: in.StartsAt.Format(time.RFC3339)},
		End:         &calendar.EventDateTime{DateTime: end.Format(time.RFC3339)},
	}
	if in.AttendeeEmail != "" {
		ev.Attendees = []*calendar.EventAttendee{
			{Email: in.AttendeeEmail, DisplayName: in.AttendeeName},
		}
	}
	call := svc.Events.Insert(c.tenant.CalendarID, ev).SendUpdates("all")
	if in.CreateMeetLink {
		ev.ConferenceData = &calendar.ConferenceData{
			CreateRequest: &calendar.CreateConferenceRequest{
				RequestId:             fmt.Sprintf("mf-%d", in.StartsAt.UnixNano()),
				ConferenceSolutionKey: &calendar.ConferenceSolutionKey{Type: "hangoutsMeet"},
			},
		}
		call = call.ConferenceDataVersion(1)
	}
	created, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("gcal insert: %w", err)
	}
	out := &EventResult{
		EventID:    created.Id,
		CalendarID: c.tenant.CalendarID,
		HTMLLink:   created.HtmlLink,
	}
	if created.ConferenceData != nil {
		for _, ep := range created.ConferenceData.EntryPoints {
			if ep.EntryPointType == "video" && ep.Uri != "" {
				out.MeetURL = ep.Uri
				break
			}
		}
	}
	return out, nil
}

// MarshalResult is a convenience for storing the provider metadata blob.
func (r *EventResult) MarshalResult() json.RawMessage {
	b, _ := json.Marshal(r)
	return b
}
