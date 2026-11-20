package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
	"github.com/google/uuid"
)

// accountPlatforms maps a ConnectedAccount account_type to the platform it publishes to.
var accountPlatforms = map[string]string{
	"facebook_page":      Facebook,
	"instagram_business": Instagram,
	"youtube_channel":    YouTube,
	"tiktok_creator":     TikTok,
}

// PlatformForAccountType is "" for account types that cannot take an organic post (ad accounts).
func PlatformForAccountType(t string) string { return accountPlatforms[t] }

// ClaimLease is how long a claimed job is held before another worker may take it over; a pod
// that dies mid upload loses the lease and the next claim resumes from the stored state.
const ClaimLease = 15 * time.Minute

// EntStore reads and writes PublishJob rows.
type EntStore struct {
	DB   *ent.Client
	Keys *encryption.KeyProvider
	// YouTube renews YouTube channel access tokens; nil uses google.NewTokens(DB, Keys).
	YouTube TokenSource
	// TikTok renews TikTok account access tokens; nil uses tiktok.NewTokens(DB, Keys).
	TikTok TokenSource
}

// Claim takes up to limit due jobs. Each is claimed by one conditional UPDATE (still running,
// scheduled_at unchanged) that only one replica can win, the nurture scheduler's pattern; the
// lease moves scheduled_at forward, so the partial (status, scheduled_at) index serves the read.
func (s *EntStore) Claim(ctx context.Context, now time.Time, limit int) ([]*Job, error) {
	rows, err := s.DB.PublishJob.Query().
		Where(
			publishjob.StatusIn(StatusPending, StatusUploading, StatusProcessing),
			publishjob.ScheduledAtLTE(now),
		).
		Order(ent.Asc(publishjob.FieldScheduledAt)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Job, 0, len(rows))
	for _, row := range rows {
		lease := now.Add(ClaimLease)
		n, err := s.DB.PublishJob.Update().
			Where(publishjob.IDEQ(row.ID), publishjob.StatusEQ(row.Status), publishjob.ScheduledAtEQ(row.ScheduledAt)).
			SetScheduledAt(lease).
			Save(ctx)
		if err != nil || n == 0 {
			continue // another replica took it
		}
		j, err := s.load(ctx, row)
		if err != nil {
			// The asset was purged or the row is unreadable: fail it now rather than retry forever.
			_, _ = s.DB.PublishJob.UpdateOneID(row.ID).SetStatus(StatusFailed).SetError(err.Error()).Save(ctx)
			continue
		}
		j.ScheduledAt = lease
		out = append(out, j)
	}
	return out, nil
}

func (s *EntStore) load(ctx context.Context, row *ent.PublishJob) (*Job, error) {
	asset, err := s.DB.CreativeAsset.Query().
		Where(creativeasset.IDEQ(row.AssetID), creativeasset.TenantIDEQ(row.TenantID)).
		Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("the video for this post no longer exists")
	}
	if asset.PurgedAt != nil {
		return nil, fmt.Errorf("the video for this post was removed")
	}
	fps, _ := asset.Metadata["fps"].(float64)
	j := &Job{
		ID:          row.ID.String(),
		TenantID:    row.TenantID.String(),
		AccountID:   row.ConnectedAccountID.String(),
		Platform:    row.Platform,
		Status:      row.Status,
		Attempts:    row.Attempts,
		ExternalID:  row.ExternalID,
		Permalink:   row.Permalink,
		Error:       row.Error,
		ScheduledAt: row.ScheduledAt,
		Video: Video{URL: asset.URL, Bytes: asset.Bytes, DurationMS: asset.DurationMs,
			Width: asset.Width, Height: asset.Height, FPS: fps},
	}
	j.Post.Caption = row.Caption
	j.Post.AIGenerated, _ = row.Metadata["ai_generated"].(bool)
	j.Post.MadeForKids, _ = row.Metadata["made_for_kids"].(bool)
	if raw, ok := row.Metadata["tiktok"]; ok {
		var o TikTokOptions
		if b, err := json.Marshal(raw); err == nil && json.Unmarshal(b, &o) == nil && o.PrivacyLevel != "" {
			j.Post.TikTok = &o
		}
	}
	if raw, ok := row.Metadata["upload"]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &j.State)
	}
	if v, ok := row.Metadata["finished_at"].(string); ok {
		j.FinishedAt, _ = time.Parse(time.RFC3339, v)
	}
	return j, nil
}

// Save writes the job's progress in one UPDATE. The upload state is merged into metadata with
// jsonb ||, so keys written by the API (ai_generated, made_for_kids) are never lost.
func (s *EntStore) Save(ctx context.Context, j *Job) error {
	id, err := uuid.Parse(j.ID)
	if err != nil {
		return err
	}
	patch := map[string]any{"upload": j.State}
	if !j.FinishedAt.IsZero() {
		patch["finished_at"] = j.FinishedAt.UTC().Format(time.RFC3339)
	}
	b, _ := json.Marshal(patch)
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + publishjob.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(b))
		bd.WriteString("::jsonb)")
	})
	u := s.DB.PublishJob.Update().
		Where(publishjob.IDEQ(id), publishjob.StatusNEQ(StatusCancelled)).
		SetStatus(j.Status).
		SetAttempts(j.Attempts).
		SetError(truncateErr(j.Error)).
		SetExternalID(firstNonEmpty(j.ExternalID, j.State.ExternalID)).
		SetPermalink(j.Permalink).
		SetScheduledAt(j.ScheduledAt).
		Modify(func(ub *entsql.UpdateBuilder) { ub.Set(publishjob.FieldMetadata, value) })
	if j.Status == StatusPublished {
		u = u.SetPublishedAt(time.Now().UTC())
	}
	n, err := u.Save(ctx)
	if err == nil && n == 0 {
		return ErrCancelled
	}
	return err
}

// ErrCancelled stops a running job whose owner cancelled it; Save never overwrites the cancel.
var ErrCancelled = errors.New("the post was cancelled")

// Account loads an active connected account of the tenant and decrypts its token.
func (s *EntStore) Account(ctx context.Context, tenantID, accountID string) (Account, error) {
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		return Account{}, err
	}
	aid, err := uuid.Parse(accountID)
	if err != nil {
		return Account{}, err
	}
	acc, err := s.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(aid), connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active")).
		Only(ctx)
	if err != nil {
		return Account{}, err
	}
	if s.Keys == nil {
		return Account{}, errors.New("no token")
	}
	if acc.AccountType == accountYouTubeChannel {
		// Google access tokens last an hour; the refresh token stored at connect renews them.
		src := s.YouTube
		if src == nil {
			src = google.NewTokens(s.DB, s.Keys)
		}
		tok, err := src.AccessToken(ctx, acc)
		if err != nil {
			return Account{}, &AccountTokenError{Platform: YouTube, Err: err, Reconnect: googleNeedsReconnect(err)}
		}
		return Account{ID: acc.ID.String(), ExternalID: acc.ExternalID, Token: tok, Scopes: acc.Scopes}, nil
	}
	if acc.AccountType == tiktok.AccountCreator {
		// TikTok access tokens last 24 hours; the refresh token stored at connect renews them.
		src := s.TikTok
		if src == nil {
			src = tiktok.NewTokens(s.DB, s.Keys)
		}
		tok, err := src.AccessToken(ctx, acc)
		if err != nil {
			return Account{}, &AccountTokenError{Platform: TikTok, Err: err, Reconnect: tiktokNeedsReconnect(err)}
		}
		return Account{ID: acc.ID.String(), ExternalID: acc.ExternalID, Token: tok, Scopes: acc.Scopes}, nil
	}
	if acc.TokenCiphertext == "" {
		return Account{}, errors.New("no token")
	}
	tok, err := s.Keys.Decrypt(ctx, acc.TokenCiphertext)
	if err != nil {
		return Account{}, err
	}
	return Account{ID: acc.ID.String(), ExternalID: acc.ExternalID, Token: tok, Scopes: acc.Scopes}, nil
}

// accountYouTubeChannel is the ConnectedAccount type of a YouTube channel.
const accountYouTubeChannel = "youtube_channel"

// TokenSource renews a connected account's short-lived access token before use (YouTube).
type TokenSource interface {
	AccessToken(ctx context.Context, acc *ent.ConnectedAccount) (string, error)
}

// AccountTokenError means the account exists but no working token could be had for it. With
// Reconnect the platform refused the stored grant and only connecting again fixes it; otherwise
// it is worth another try later (the platform was down).
type AccountTokenError struct {
	Platform  string
	Err       error
	Reconnect bool
}

func (e *AccountTokenError) Error() string {
	if e.Reconnect {
		return Title(e.Platform) + " no longer accepts this connection (" + e.Err.Error() +
			"). Connect it again on the Integrations page, then retry the post"
	}
	return Title(e.Platform) + " token could not be renewed: " + e.Err.Error()
}

func (e *AccountTokenError) Unwrap() error { return e.Err }

// tiktokNeedsReconnect: TikTok said invalid_grant, or the row has no refresh token at all.
func tiktokNeedsReconnect(err error) bool {
	var te *tiktok.TokenError
	return errors.Is(err, tiktok.ErrNoRefreshToken) || (errors.As(err, &te) && te.Revoked())
}

// googleNeedsReconnect: Google said invalid_grant, or the row has no refresh token at all.
func googleNeedsReconnect(err error) bool {
	var te *google.TokenError
	return errors.Is(err, google.ErrNoRefreshToken) || (errors.As(err, &te) && te.Revoked())
}

// Enabled reads the platform switch: ProviderSetting tenant "platform", provider_type
// "publishing", key "<platform>_enabled", value "true". No row means off.
func (s *EntStore) Enabled(ctx context.Context, platform string) bool {
	row, err := s.DB.ProviderSetting.Query().
		Where(
			providersetting.TenantIDEQ("platform"),
			providersetting.ProviderTypeEQ("publishing"),
			providersetting.KeyEQ(platform+"_enabled"),
			providersetting.IsActiveEQ(true),
		).
		First(ctx)
	return err == nil && strings.EqualFold(strings.TrimSpace(row.Value), "true")
}

func truncateErr(s string) string {
	if len(s) > 2000 {
		return s[:2000]
	}
	return s
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
