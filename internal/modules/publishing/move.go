package publishing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/publishing/postingplan"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// bestMoveTime is where "move to the best time" lands: the platform's next best-time window at or
// after the requested time, or after now when none (or a past one) is given. Same rule a new post
// uses (Schedule with best_time), from the one posting plan.
func bestMoveTime(plan postingplan.Settings, platform string, requested, now time.Time) time.Time {
	from := requested
	if !from.After(now) {
		from = now.Add(time.Minute)
	}
	return plan.BestTime(platform, from).UTC().Truncate(time.Second)
}

// errNotMovable is a post the worker has taken, or one whose time has come.
var errNotMovable = errors.New("this post is already on its way and can no longer be moved")

// Reschedule moves a post that is still waiting for its time. A claimed job stays pending while its
// lease pushes scheduled_at forward, so status alone cannot tell it is free: the stored plan must
// still match scheduled_at. The update repeats that scheduled_at, as Claim does, so a move and a
// claim cannot both win.
func (s *EntStore) Reschedule(ctx context.Context, tenantID, id uuid.UUID, when, now time.Time) (*ent.PublishJob, error) {
	row, err := s.DB.PublishJob.Query().Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tenantID)).Only(ctx)
	if err != nil {
		return nil, err
	}
	planned, _ := row.Metadata["planned_at"].(string)
	at, err := time.Parse(time.RFC3339, planned)
	if err != nil || row.Status != StatusPending || !row.ScheduledAt.After(now) || !at.Equal(row.ScheduledAt.Truncate(time.Second)) {
		return nil, errNotMovable
	}
	when = when.UTC().Truncate(time.Second)
	patch, err := json.Marshal(map[string]string{"planned_at": when.Format(time.RFC3339)})
	if err != nil {
		return nil, err
	}
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + publishjob.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	n, err := s.DB.PublishJob.Update().
		Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tenantID), publishjob.StatusEQ(StatusPending), publishjob.ScheduledAtEQ(row.ScheduledAt)).
		SetScheduledAt(when).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(publishjob.FieldMetadata, value) }).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errNotMovable
	}
	return s.DB.PublishJob.Get(ctx, id)
}

// Move handles POST /api/v1/publishing/jobs/{id}/move {scheduled_at}.
// @Summary Move a scheduled post to another time
// @Tags Publishing
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Job UUID"
// @Success 200 {object} JobView
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /publishing/jobs/{id}/move [post]
func (h *Handler) Move(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok || !h.allowed(w, r, tid) {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	var in struct {
		ScheduledAt time.Time `json:"scheduled_at"`
		// BestTime moves the post to its platform's next best-time window at or after
		// ScheduledAt (or now when it is empty), the same rule a new post uses.
		BestTime bool `json:"best_time,omitempty"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	now := time.Now().UTC()
	if in.BestTime {
		job, err := h.Store.DB.PublishJob.Query().Where(publishjob.IDEQ(id), publishjob.TenantIDEQ(tid)).Only(r.Context())
		if err != nil {
			h.fail(w, err)
			return
		}
		in.ScheduledAt = bestMoveTime(postingplan.Load(r.Context(), h.Store.DB, tid), job.Platform, in.ScheduledAt, now)
	}
	if !in.ScheduledAt.After(now) || in.ScheduledAt.After(now.AddDate(0, 6, 0)) {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "choose a time in the future, up to 6 months ahead", nil)
		return
	}
	row, err := h.Store.Reschedule(r.Context(), tid, id, in.ScheduledAt, now)
	if errors.Is(err, errNotMovable) {
		httpapi.Error(w, http.StatusConflict, "conflict", err.Error(), nil)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, view(row))
}
