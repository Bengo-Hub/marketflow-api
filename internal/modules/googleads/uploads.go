package googleads

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// KeyUploadAPI is the platform's service_config switch for conversion uploads: data_manager
// (default) or google_ads, the older Google Ads API call Google closed to new accounts on
// 2026-06-15.
const (
	KeyUploadAPI         = "google_ads.conversion_upload_api"
	UploadAPIDataManager = "data_manager"
	UploadAPIGoogleAds   = "google_ads"
)

// UploadAPI is the API conversions are sent through.
func UploadAPI(ctx context.Context, db *ent.Client) string {
	if svcconfig.Value(ctx, db, uuid.Nil, KeyUploadAPI) == UploadAPIGoogleAds {
		return UploadAPIGoogleAds
	}
	return UploadAPIDataManager
}

// MaySendConversions reports whether a connection's granted scopes cover the upload API. No
// recorded scopes means unknown, so the upload is tried and Google decides.
func MaySendConversions(api string, scopes []string) bool {
	return api != UploadAPIDataManager || len(scopes) == 0 || google.HasScopes(scopes, []string{google.ScopeDataManager})
}

// ConnectedAccount.metadata keys of conversion uploads.
const (
	// metaUpload is the last upload's outcome (Upload).
	metaUpload = "conversion_upload"
	// metaRequests maps a Data Manager request id to its Pending check.
	metaRequests = "conversion_requests"
	// maxPending caps the requests one account tracks; Google's answer to the rest is not read.
	maxPending = 500
)

// keptOnReconnect are settings a reconnect must not lose (saveAccounts replaces the metadata).
var keptOnReconnect = []string{MetaConversionActions, "login_customer_id", metaRequests}

// KeepOnReconnect copies the account's own settings into the metadata a reconnect writes.
func KeepOnReconnect(old, fresh map[string]any) {
	for _, k := range keptOnReconnect {
		if v, ok := old[k]; ok {
			if _, set := fresh[k]; !set {
				fresh[k] = v
			}
		}
	}
}

// Upload statuses.
const (
	UploadSent      = "sent"      // Google took it and is still checking it
	UploadAccepted  = "accepted"  // Google processed it without an error
	UploadFailed    = "failed"    // refused, Reason says why
	UploadReconnect = "reconnect" // the connection lacks the permission
	UploadUnknown   = "unknown"   // Google did not say within its 24 hours
)

// Upload is the outcome of the account's latest conversion upload.
type Upload struct {
	Status string    `json:"status"`
	Reason string    `json:"reason,omitempty"`
	Event  string    `json:"event,omitempty"`
	At     time.Time `json:"at"`
}

// Failure is the outcome of an upload error.
func Failure(err error, event string, at time.Time) Upload {
	text, reconnect := google.PlainUploadError(err)
	u := Upload{Status: UploadFailed, Reason: clip(text, 400), Event: event, At: at.UTC()}
	if reconnect {
		u.Status = UploadReconnect
	}
	return u
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// Pending is a sent request whose result Google has not given yet.
type Pending struct {
	Event string    `json:"event"`
	At    time.Time `json:"at"`
	Next  time.Time `json:"next"`
	// WaitMinutes is the wait before Next, which grows between checks.
	WaitMinutes float64 `json:"wait_minutes"`
}

func decodeMeta(a *ent.ConnectedAccount, key string, out any) bool {
	v, ok := a.Metadata[key]
	if !ok || v == nil {
		return false
	}
	b, err := json.Marshal(v)
	return err == nil && json.Unmarshal(b, out) == nil
}

// LastUpload is the account's latest upload outcome, nil when nothing was sent yet.
func LastUpload(a *ent.ConnectedAccount) *Upload {
	var u Upload
	if !decodeMeta(a, metaUpload, &u) || u.Status == "" {
		return nil
	}
	return &u
}

// PendingRequests are the requests still to check, by Data Manager request id.
func PendingRequests(a *ent.ConnectedAccount) map[string]Pending {
	out := map[string]Pending{}
	decodeMeta(a, metaRequests, &out)
	return out
}

// update replaces metadata with a jsonb expression on its current value, so a token renewal or
// another upload saving at the same moment is not lost.
func update(ctx context.Context, db *ent.Client, id uuid.UUID, expr func(b *entsql.Builder)) error {
	return db.ConnectedAccount.UpdateOneID(id).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(connectedaccount.FieldMetadata, entsql.ExprFunc(expr)) }).
		Exec(ctx)
}

const currentMeta = "COALESCE(" + connectedaccount.FieldMetadata + ", '{}'::jsonb)"

// PatchMetadata merges keys into the account's metadata.
func PatchMetadata(ctx context.Context, db *ent.Client, id uuid.UUID, patch map[string]any) error {
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	return update(ctx, db, id, func(b *entsql.Builder) {
		b.WriteString("(" + currentMeta + " || ")
		b.Arg(string(raw))
		b.WriteString("::jsonb)")
	})
}

// SaveUpload records the latest upload outcome.
func SaveUpload(ctx context.Context, db *ent.Client, id uuid.UUID, u Upload) error {
	return PatchMetadata(ctx, db, id, map[string]any{metaUpload: u})
}

// Track records a sent request: its outcome so far, and when to ask Google about it.
func Track(ctx context.Context, db *ent.Client, a *ent.ConnectedAccount, requestID string, p Pending, u Upload) error {
	if requestID == "" || len(PendingRequests(a)) >= maxPending {
		return SaveUpload(ctx, db, a.ID, u)
	}
	outcome, err := json.Marshal(map[string]any{metaUpload: u})
	if err != nil {
		return err
	}
	req, err := json.Marshal(map[string]Pending{requestID: p})
	if err != nil {
		return err
	}
	return update(ctx, db, a.ID, func(b *entsql.Builder) {
		b.WriteString("(" + currentMeta + " || ")
		b.Arg(string(outcome))
		b.WriteString("::jsonb || jsonb_build_object('" + metaRequests + "', COALESCE(" + connectedaccount.FieldMetadata + "->'" + metaRequests + "', '{}'::jsonb) || ")
		b.Arg(string(req))
		b.WriteString("::jsonb))")
	})
}

// Reschedule moves a request's next check.
func Reschedule(ctx context.Context, db *ent.Client, id uuid.UUID, requestID string, p Pending) error {
	req, err := json.Marshal(map[string]Pending{requestID: p})
	if err != nil {
		return err
	}
	return update(ctx, db, id, func(b *entsql.Builder) {
		b.WriteString("(" + currentMeta + " || jsonb_build_object('" + metaRequests + "', COALESCE(" + connectedaccount.FieldMetadata + "->'" + metaRequests + "', '{}'::jsonb) || ")
		b.Arg(string(req))
		b.WriteString("::jsonb))")
	})
}

// Resolve stops tracking a request and records its outcome.
func Resolve(ctx context.Context, db *ent.Client, id uuid.UUID, requestID string, u Upload) error {
	outcome, err := json.Marshal(map[string]any{metaUpload: u})
	if err != nil {
		return err
	}
	return update(ctx, db, id, func(b *entsql.Builder) {
		b.WriteString("((" + currentMeta + " #- ARRAY['" + metaRequests + "', ")
		b.Arg(requestID)
		b.WriteString("::text]) || ")
		b.Arg(string(outcome))
		b.WriteString("::jsonb)")
	})
}

// WithPending keeps accounts that have a request to check.
func WithPending(s *entsql.Selector) {
	s.Where(entsql.P(func(b *entsql.Builder) {
		b.WriteString("COALESCE(" + s.C(connectedaccount.FieldMetadata) + "->'" + metaRequests + "', '{}'::jsonb) <> '{}'::jsonb")
	}))
}

// LoginCustomer is the manager account the connection signs in through, "" for direct access.
func LoginCustomer(a *ent.ConnectedAccount) string {
	v, _ := a.Metadata["login_customer_id"].(string)
	return google.CustomerDigits(strings.TrimSpace(v))
}
