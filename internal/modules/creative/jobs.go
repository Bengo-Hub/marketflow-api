package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Motion posters and video ads take minutes, so marketflow-ai runs them as
// media jobs: the renderer starts one and returns, and marketflow-ai pushes the
// outcome to POST /api/v1/internal/creative/jobs/{version_id}. Polling
// GET /internal/creative/jobs/{version_id} is only the fallback for a push that
// never arrived (owner rule 2026-10-24: push first, poll as the fallback).

const (
	// jobPollAfter is how long a job may go without news before the renderer
	// asks marketflow-ai for it (the push normally comes first).
	jobPollAfter = 2 * time.Minute
	// jobDeadline fails a job still unsettled this long after it first started.
	jobDeadline  = 45 * time.Minute
	jobPollBatch = 20
)

// Job is marketflow-ai's media job state: the start reply, the pushed body and
// the status poll all carry it.
type Job struct {
	Status string `json:"status"` // running | done | failed
	Result *struct {
		URL          string  `json:"url"`
		Key          string  `json:"key"`
		Bytes        int64   `json:"bytes"`
		Seconds      float64 `json:"seconds"`
		Width        int     `json:"width"`
		Height       int     `json:"height"`
		SHA256       string  `json:"sha256"`
		Route        string  `json:"route"`
		CostUSD      float64 `json:"cost_usd"`
		Attempts     int     `json:"attempts"`
		CaptionLines int     `json:"caption_lines"`
	} `json:"result,omitempty"`
	CreditsCharged int    `json:"credits_charged"`
	Error          string `json:"error,omitempty"`
	// Refunded is false on a failed job that was lost with its pod: its charge
	// stands, so it is started again under the same charge key.
	Refunded bool `json:"refunded"`
}

// jobKinds are the version kinds made by media jobs.
var jobKinds = []string{KindVideo, KindVideoAd}

// errJobBusy is a 429: marketflow-ai is making as many videos as it may.
var errJobBusy = errors.New("marketflow-ai is busy with other videos")

// errJobLost is a 404 from the status poll: marketflow-ai has no job for it.
var errJobLost = errors.New("marketflow-ai has no job for this version")

// aiError is a 400 or 422 from marketflow-ai: its message tells the owner what
// to change, and nothing was charged.
type aiError struct{ msg string }

func (e *aiError) Error() string { return e.msg }

// startJob asks marketflow-ai to start a claimed version's media job (path is
// the start route, body its request). It returns at once; the job settles
// through the push (or the fallback poll).
func (r *Renderer) startJob(ctx context.Context, v *ent.CreativeVersion, meta map[string]any, path string, body any) {
	meta["attempts"] = intOf(meta["attempts"]) + 1
	if _, ok := meta["job_started_at"]; !ok {
		meta["job_started_at"] = r.nowFn().UTC().Format(time.RFC3339)
	}
	_, _ = r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Save(ctx)

	job, err := r.jobRequest(ctx, http.MethodPost, path, body)
	switch {
	case errors.Is(err, errJobBusy):
		// Not an attempt: back in the queue for the next tick.
		meta["attempts"] = intOf(meta["attempts"]) - 1
		_ = r.db.CreativeVersion.UpdateOneID(v.ID).SetStatus(StatusQueued).SetMetadata(meta).Exec(ctx)
	case err != nil:
		r.failJob(ctx, v.ID, err)
	case job.Status != "running":
		// Already done (a restart after the video was made): settle it now.
		if err := r.SettleJob(ctx, v.ID, *job); err != nil {
			r.logger.Error("video made but not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
		}
	}
}

// pollJobs is the fallback for a lost push: it asks marketflow-ai about jobs
// that have had no news for jobPollAfter, and fails those past the deadline.
// A video ad still writing its script is not a job (recoverStale has it).
func (r *Renderer) pollJobs(ctx context.Context) {
	waiting, err := r.db.CreativeVersion.Query().
		Where(creativeversion.StatusEQ(StatusRendering), creativeversion.KindIn(jobKinds...),
			creativeversion.UpdatedAtLT(r.nowFn().Add(-jobPollAfter))).
		Limit(jobPollBatch).
		All(ctx)
	if err != nil {
		return
	}
	for _, v := range waiting {
		if v.Kind == KindVideoAd && stage(v) != StageRender {
			continue
		}
		job, err := r.jobRequest(ctx, http.MethodGet, "/api/v1/internal/creative/jobs/"+v.ID.String(), nil)
		if err == nil {
			if err := r.SettleJob(ctx, v.ID, *job); err != nil {
				r.logger.Warn("job poll not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
			}
		}
		if err != nil || job.Status == "running" {
			r.touchJob(ctx, v, errors.Is(err, errJobLost))
		}
	}
}

// touchJob records a poll with no result so the next one waits jobPollAfter;
// a job past its deadline fails, and one marketflow-ai has no job for goes
// back in the queue.
func (r *Renderer) touchJob(ctx context.Context, v *ent.CreativeVersion, lost bool) {
	started, _ := time.Parse(time.RFC3339, str(v.Metadata["job_started_at"]))
	u := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering), creativeversion.UpdatedAt(v.UpdatedAt))
	meta := copyMap(v.Metadata)
	switch {
	case !started.IsZero() && r.nowFn().Sub(started) > jobDeadline, lost && intOf(meta["attempts"]) >= maxAttempts:
		u.SetStatus(StatusFailed).SetError("the video took too long; try again")
	case lost:
		u.SetStatus(StatusQueued)
	default:
		meta["polled_at"] = r.nowFn().UTC().Format(time.RFC3339)
		u.SetMetadata(meta)
	}
	_, _ = u.Save(ctx)
}

// SettleJob records a media job's outcome on its version, from the push or the
// poll. It is idempotent: only a version still rendering changes, so a late or
// repeated report does nothing.
func (r *Renderer) SettleJob(ctx context.Context, id uuid.UUID, job Job) error {
	v, err := r.db.CreativeVersion.Query().
		Where(creativeversion.ID(id), creativeversion.KindIn(jobKinds...)).
		Only(ctx)
	if err != nil {
		return err
	}
	if v.Status != StatusRendering {
		return nil
	}
	pending := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering))
	switch {
	case job.Status == "done" && job.Result != nil && job.Result.URL != "" && job.Result.Key != "":
		res := job.Result
		meta := copyMap(v.Metadata)
		meta["route"], meta["cost_usd"], meta["provider_attempts"] = res.Route, res.CostUSD, res.Attempts
		if res.CaptionLines > 0 {
			meta["caption_lines"] = res.CaptionLines
		}
		return withTx(ctx, r.db, func(tx *ent.Tx) error {
			n, err := tx.CreativeVersion.Update().
				Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering)).
				SetStatus(StatusReady).SetCredits(v.Credits + job.CreditsCharged).SetMetadata(meta).ClearError().
				Save(ctx)
			if err != nil || n == 0 {
				return err
			}
			a := tx.CreativeAsset.Create().
				SetTenantID(v.TenantID).SetVersionID(id).SetKind(AssetVideo).
				SetStorageKey(res.Key).SetURL(res.URL).SetBytes(res.Bytes).
				SetWidth(res.Width).SetHeight(res.Height).SetDurationMs(int(res.Seconds * 1000)).
				SetExpiresAt(r.nowFn().Add(DraftTTL))
			if res.SHA256 != "" {
				a.SetSha256(res.SHA256)
			}
			_, err = a.Save(ctx)
			return err
		})
	case job.Status == "failed" && job.Refunded:
		msg := job.Error
		if msg == "" {
			msg = "the video could not be made right now; nothing was charged"
		}
		_, err := pending.SetStatus(StatusFailed).SetError(excerpt(msg, 480)).Save(ctx)
		return err
	case job.Status == "failed":
		// Lost with its pod and not refunded: start again under the same charge key.
		if intOf(v.Metadata["attempts"]) >= maxAttempts {
			_, err := pending.SetStatus(StatusFailed).SetError("the video took too long; try again").Save(ctx)
			return err
		}
		_, err := pending.SetStatus(StatusQueued).Save(ctx)
		return err
	}
	return nil // still running
}

func (r *Renderer) jobRequest(ctx context.Context, method, path string, body any) (*Job, error) {
	data, status, err := r.aiCall(ctx, method, path, body, 30*time.Second)
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK, http.StatusAccepted:
		var job Job
		if err := json.Unmarshal(data, &job); err != nil || job.Status == "" {
			return nil, fmt.Errorf("marketflow-ai sent an unreadable job reply")
		}
		return &job, nil
	case http.StatusTooManyRequests:
		return nil, errJobBusy
	case http.StatusNotFound:
		if method == http.MethodGet {
			return nil, errJobLost
		}
	}
	return nil, aiStatusError(status, data)
}

// aiCall sends one request to marketflow-ai with the service key and returns
// the body and status.
func (r *Renderer) aiCall(ctx context.Context, method, path string, body any, timeout time.Duration) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		rd = bytes.NewReader(buf)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, r.aiBaseURL+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, r.serviceKey)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return data, resp.StatusCode, nil
}

// aiStatusError turns a failed marketflow-ai reply into an error: 402 is the
// credits error, 400 and 422 carry marketflow-ai's message for the owner.
func aiStatusError(status int, data []byte) error {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(data, &e)
	switch {
	case status == http.StatusPaymentRequired:
		return errNoCredits
	case (status == http.StatusBadRequest || status == http.StatusUnprocessableEntity) && e.Error != "":
		return &aiError{msg: e.Error}
	}
	if e.Error == "" {
		e.Error = http.StatusText(status)
	}
	return fmt.Errorf("marketflow-ai %d: %s", status, e.Error)
}

// failJob records why a job did not start. marketflow-ai charges only once a
// job has started, so nothing was charged.
func (r *Renderer) failJob(ctx context.Context, id uuid.UUID, err error) {
	msg := "the video could not be made right now; nothing was charged"
	var ae *aiError
	switch {
	case errors.Is(err, errNoCredits):
		msg = "not enough AI credits for this video"
	case errors.As(err, &ae):
		msg = excerpt(ae.msg, 480)
	}
	r.logger.Warn("video job did not start", zap.String("version_id", id.String()), zap.Error(err))
	_ = r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusRendering)).
		SetStatus(StatusFailed).SetError(msg).Exec(ctx)
}
