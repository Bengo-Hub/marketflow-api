package publishing

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/google/uuid"
)

// Posts an ad autopilot schedules at the end of its veto window (Sprint 20, owner 2026-11-14).
// They are ordinary organic publish jobs, so the worker, the calendar and the cancel route treat
// them like any post; metadata.autopilot marks them. A veto is a conditional cancel of the jobs
// still waiting, the same UPDATE the calendar's Cancel uses.

// VetoRequest is one autopilot video to post to the agent's chosen accounts.
type VetoRequest struct {
	TenantID   uuid.UUID
	AssetID    uuid.UUID
	VersionID  uuid.UUID
	AgentID    uuid.UUID
	AccountIDs []string
	// Platforms are the agent's chosen platforms; an account on another platform is skipped.
	Platforms []string
	Caption   string
	At        time.Time
}

// VetoSchedule is what was scheduled and which accounts were left out, with the reason.
type VetoSchedule struct {
	Jobs    []JobView         `json:"jobs"`
	Skipped map[string]string `json:"skipped,omitempty"` // account id or name -> reason
}

// ScheduleForVeto creates the organic jobs of an autopilot video at req.At. Unlike the Studio's
// Schedule it does not need an approved version: the veto window is the approval, recorded when
// the window ends. Every account still goes through the same checks (the tenant's, active, a
// platform that takes organic posts, its switch on, preflight); an account that fails one is
// skipped with its reason, so one disconnected page does not stop the others. No usable account
// schedules nothing and returns no jobs.
func (s *EntStore) ScheduleForVeto(ctx context.Context, req VetoRequest) (VetoSchedule, error) {
	out := VetoSchedule{Skipped: map[string]string{}}
	asset, err := s.DB.CreativeAsset.Get(ctx, req.AssetID)
	if err != nil {
		return out, err
	}
	if asset.TenantID != req.TenantID || asset.PurgedAt != nil {
		return out, fmt.Errorf("the video for this post is not available")
	}
	fps, _ := asset.Metadata["fps"].(float64)
	video := Video{URL: asset.URL, Bytes: asset.Bytes, DurationMS: asset.DurationMs, Width: asset.Width, Height: asset.Height, FPS: fps}
	post := Post{Caption: strings.TrimSpace(req.Caption), AIGenerated: true}
	// Stored to the second, the same as planned_at, so a veto can tell a waiting job from a claimed one.
	at := req.At.UTC().Truncate(time.Second)
	seen := map[uuid.UUID]bool{}
	for _, raw := range req.AccountIDs {
		aid, err := uuid.Parse(raw)
		if err != nil || seen[aid] {
			continue
		}
		seen[aid] = true
		acc, err := s.DB.ConnectedAccount.Query().
			Where(connectedaccount.IDEQ(aid), connectedaccount.TenantIDEQ(req.TenantID), connectedaccount.StatusEQ("active")).
			Only(ctx)
		if err != nil {
			out.Skipped[raw] = "not connected any more"
			continue
		}
		platform := PlatformForAccountType(acc.AccountType)
		switch {
		case platform == "":
			out.Skipped[acc.DisplayName] = "cannot take an organic post"
			continue
		case len(req.Platforms) > 0 && !slices.Contains(req.Platforms, platform):
			out.Skipped[acc.DisplayName] = "not one of the agent's platforms"
			continue
		case !s.Enabled(ctx, platform):
			out.Skipped[acc.DisplayName] = fmt.Sprintf("posting to %s is not switched on yet", title(platform))
			continue
		}
		if problems := Preflight(platform, video, post, acc.Scopes, 0); len(problems) > 0 {
			out.Skipped[acc.DisplayName] = strings.Join(problems, "; ")
			continue
		}
		key := asset.ID.String() + ":" + acc.ID.String()
		row, err := s.DB.PublishJob.Create().
			SetTenantID(req.TenantID).
			SetAssetID(asset.ID).
			SetConnectedAccountID(acc.ID).
			SetPlatform(platform).
			SetCaption(post.Caption).
			SetScheduledAt(at).
			SetDedupeKey(key).
			SetMetadata(map[string]any{
				"ai_generated":  true,
				"made_for_kids": false,
				"planned_at":    at.Format(time.RFC3339),
				"autopilot": map[string]any{
					"agent_id": req.AgentID.String(), "version_id": req.VersionID.String(), "veto_until": at.Format(time.RFC3339),
				},
			}).
			Save(ctx)
		if ent.IsConstraintError(err) {
			// This video is already scheduled to the account: by this same step taken before (its
			// job is this video's and counts), or by a person (that one post stands, never a second).
			existing, qerr := s.DB.PublishJob.Query().Where(publishjob.DedupeKeyEQ(key), publishjob.TenantIDEQ(req.TenantID)).Only(ctx)
			if qerr == nil {
				if ap, ok := existing.Metadata["autopilot"].(map[string]any); ok && ap["version_id"] == req.VersionID.String() {
					out.Jobs = append(out.Jobs, view(existing))
					continue
				}
			}
			out.Skipped[acc.DisplayName] = "already scheduled to this account"
			continue
		}
		if err != nil {
			return out, err
		}
		out.Jobs = append(out.Jobs, view(row))
	}
	return out, nil
}

// claimTolerance separates a claimed job's lease (ClaimLease past the claim) from its planned
// time, which is stored to the second.
const claimTolerance = time.Minute

// VetoOutcome is what a veto did to a video's jobs.
type VetoOutcome struct {
	Cancelled int `json:"cancelled"`
	// Gone are jobs past the point of a veto (uploading to or already on the platform).
	Gone []JobView `json:"gone,omitempty"`
}

// CancelForVeto cancels a tenant's autopilot jobs that are still waiting. One conditional UPDATE
// per call, so a veto and the worker's claim cannot both win; a job the worker already took is
// reported in Gone. A cancelled job's dedupe key is released, so the owner can still post the
// video later from the Studio.
func (s *EntStore) CancelForVeto(ctx context.Context, tenantID uuid.UUID, jobIDs []uuid.UUID) (VetoOutcome, error) {
	var out VetoOutcome
	if len(jobIDs) == 0 {
		return out, nil
	}
	// The worker's claim moves scheduled_at forward but leaves the status pending until its first
	// save, so a claimed job is told apart by its lease: scheduled_at past the planned time.
	rows, err := s.DB.PublishJob.Query().
		Where(publishjob.IDIn(jobIDs...), publishjob.TenantIDEQ(tenantID)).
		All(ctx)
	if err != nil {
		return out, err
	}
	for _, row := range rows {
		planned := row.ScheduledAt
		if raw, ok := row.Metadata["planned_at"].(string); ok {
			if t, err := time.Parse(time.RFC3339, raw); err == nil {
				planned = t
			}
		}
		n := 0
		if row.Status == StatusPending && !row.ScheduledAt.After(planned.Add(claimTolerance)) {
			n, err = s.DB.PublishJob.Update().
				Where(publishjob.IDEQ(row.ID), publishjob.TenantIDEQ(tenantID), publishjob.StatusEQ(StatusPending),
					publishjob.ScheduledAtEQ(row.ScheduledAt)).
				SetStatus(StatusCancelled).
				SetDedupeKey("vetoed:" + row.ID.String()).
				Save(ctx)
			if err != nil {
				return out, err
			}
		}
		if n > 0 {
			out.Cancelled++
			continue
		}
		if fresh, err := s.DB.PublishJob.Get(ctx, row.ID); err == nil {
			row = fresh
		}
		if row.Status != StatusCancelled {
			out.Gone = append(out.Gone, view(row))
		}
	}
	return out, nil
}

// JobsByID returns a tenant's jobs, for the agent dashboard.
func (s *EntStore) JobsByID(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) ([]JobView, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.DB.PublishJob.Query().
		Where(publishjob.IDIn(ids...), publishjob.TenantIDEQ(tenantID)).
		Order(ent.Asc(publishjob.FieldScheduledAt)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]JobView, 0, len(rows))
	for _, r := range rows {
		out = append(out, view(r))
	}
	return out, nil
}
