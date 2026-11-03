package publishing

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Job statuses (PublishJob.status).
const (
	StatusPending    = "pending"
	StatusUploading  = "uploading"
	StatusProcessing = "processing"
	StatusPublished  = "published"
	StatusFailed     = "failed"
	StatusCancelled  = "cancelled"
)

// Job is the runner's view of a PublishJob row.
type Job struct {
	ID          string
	TenantID    string
	AccountID   string
	Platform    string
	Status      string
	Attempts    int
	Post        Post
	Video       Video
	State       UploadState
	FinishedAt  time.Time // when Finish succeeded; bounds the processing wait
	ExternalID  string
	Permalink   string
	Error       string
	ScheduledAt time.Time
}

// Store persists job progress. Save must write status, attempts, state, error, external id,
// permalink and scheduled_at in one statement, so a crash between chunks loses at most the chunk
// in flight.
type Store interface {
	Save(ctx context.Context, j *Job) error
}

// Accounts loads a connected account with its decrypted token.
type Accounts interface {
	Account(ctx context.Context, tenantID, accountID string) (Account, error)
}

// Switches says whether a platform is switched on (ProviderSetting publishing/<platform>).
type Switches interface {
	Enabled(ctx context.Context, platform string) bool
}

// Resumer is an adapter that can ask the platform how much of an interrupted upload arrived
// (YouTube). Adapters without it restart the chunk at the stored offset.
type Resumer interface {
	Resume(ctx context.Context, acct Account, v Video, st UploadState) (UploadState, error)
}

// Runner limits.
const (
	MaxAttempts      = 5
	ProcessingPoll   = 30 * time.Second
	ProcessingMaxAge = 2 * time.Hour
)

// Runner moves one job forward each time Step is called. It never posts twice: the dedupe_key
// stops a second job for the same asset and account, and a job that already has an external id
// from Finish only polls Status.
type Runner struct {
	Adapters map[string]Adapter
	Store    Store
	Accounts Accounts
	Switches Switches
	Now      func() time.Time
}

func NewRunner(store Store, accounts Accounts, switches Switches, adapters ...Adapter) *Runner {
	m := make(map[string]Adapter, len(adapters))
	for _, a := range adapters {
		m[a.Platform()] = a
	}
	return &Runner{Adapters: m, Store: store, Accounts: accounts, Switches: switches, Now: time.Now}
}

// Step runs the job until it is published, failed, or has to wait (processing poll or a retry
// backoff, written to ScheduledAt). The returned error is only a Store failure; platform errors
// are recorded on the job.
func (r *Runner) Step(ctx context.Context, j *Job) error {
	switch j.Status {
	case StatusPublished, StatusFailed, StatusCancelled:
		return nil
	}
	ad, ok := r.Adapters[j.Platform]
	if !ok {
		return r.fail(ctx, j, fmt.Sprintf("%s is not a publishing platform", j.Platform))
	}
	if r.Switches != nil && !r.Switches.Enabled(ctx, j.Platform) {
		return r.fail(ctx, j, ErrDisabled.Error())
	}
	acct, err := r.Accounts.Account(ctx, j.TenantID, j.AccountID)
	if err != nil {
		return r.fail(ctx, j, "the connected account is gone or was disconnected; reconnect it and post again")
	}

	if j.Status == StatusPending {
		if j.Video.Bytes <= 0 || j.Video.URL == "" {
			return r.fail(ctx, j, "the video file is missing or its size is unknown; render it again")
		}
		if problems := Preflight(j.Platform, j.Video, j.Post, acct.Scopes, 0); len(problems) > 0 {
			return r.fail(ctx, j, strings.Join(problems, "; "))
		}
		st, err := ad.Start(ctx, acct, j.Video, j.Post)
		if err != nil {
			return r.retry(ctx, j, err)
		}
		j.State, j.Status = st, StatusUploading
		if err := r.Store.Save(ctx, j); err != nil {
			return err
		}
	}

	if j.Status == StatusUploading {
		if j.Attempts > 0 {
			if rs, ok := ad.(Resumer); ok && j.State.SessionURI != "" {
				st, err := rs.Resume(ctx, acct, j.Video, j.State)
				if err != nil {
					return r.retry(ctx, j, err)
				}
				j.State = st
			}
		}
		for j.State.Offset < j.Video.Bytes {
			st, err := ad.Upload(ctx, acct, j.Video, j.State)
			if err != nil {
				return r.retry(ctx, j, err)
			}
			if st.Offset <= j.State.Offset {
				return r.retry(ctx, j, errors.New("the upload made no progress"))
			}
			j.State = st
			if err := r.Store.Save(ctx, j); err != nil {
				return err
			}
		}
		st, err := ad.Finish(ctx, acct, j.Post, j.State)
		if err != nil {
			return r.retry(ctx, j, err)
		}
		j.State, j.Status, j.FinishedAt = st, StatusProcessing, r.Now()
		j.Attempts, j.Error = 0, ""
		if err := r.Store.Save(ctx, j); err != nil {
			return err
		}
	}

	// StatusProcessing: poll once; wait and come back if the platform is still working.
	res, err := ad.Status(ctx, acct, j.State)
	if err != nil {
		return r.retry(ctx, j, err)
	}
	switch {
	case res.Done && res.Published:
		j.Status, j.ExternalID, j.Permalink, j.Error = StatusPublished, res.ExternalID, res.Permalink, ""
		return r.Store.Save(ctx, j)
	case res.Done:
		return r.fail(ctx, j, res.Error)
	}
	if !j.FinishedAt.IsZero() && r.Now().Sub(j.FinishedAt) > ProcessingMaxAge {
		return r.fail(ctx, j, "the platform was still processing the video after 2 hours; check the account and post again")
	}
	j.ScheduledAt = r.Now().Add(ProcessingPoll)
	return r.Store.Save(ctx, j)
}

// retry records a platform error and backs off (1, 2, 4, 8 minutes); the fifth error fails the
// job. An expired YouTube session clears the state so the next attempt starts over.
func (r *Runner) retry(ctx context.Context, j *Job, cause error) error {
	j.Attempts++
	j.Error = cause.Error()
	if errors.Is(cause, ErrSessionExpired) {
		j.State, j.Status = UploadState{}, StatusPending
	}
	if j.Attempts >= MaxAttempts {
		return r.fail(ctx, j, fmt.Sprintf("gave up after %d tries: %s", j.Attempts, cause))
	}
	j.ScheduledAt = r.Now().Add(time.Minute << (j.Attempts - 1))
	return r.Store.Save(ctx, j)
}

func (r *Runner) fail(ctx context.Context, j *Job, msg string) error {
	j.Status, j.Error = StatusFailed, msg
	return r.Store.Save(ctx, j)
}
