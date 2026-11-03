package publishing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeAdapter scripts each call so the runner's state machine can be walked step by step.
type fakeAdapter struct {
	platform  string
	startErr  error
	uploadErr []error // popped per Upload call
	statuses  []Result
	starts    int
	uploads   int
	finishes  int
	resumes   int
	chunk     int64
}

func (f *fakeAdapter) Platform() string { return f.platform }
func (f *fakeAdapter) Start(context.Context, Account, Video, Post) (UploadState, error) {
	f.starts++
	if f.startErr != nil {
		return UploadState{}, f.startErr
	}
	return UploadState{SessionURI: "s", ExternalID: "x"}, nil
}
func (f *fakeAdapter) Upload(_ context.Context, _ Account, v Video, st UploadState) (UploadState, error) {
	f.uploads++
	if len(f.uploadErr) > 0 {
		err := f.uploadErr[0]
		f.uploadErr = f.uploadErr[1:]
		if err != nil {
			return st, err
		}
	}
	st.Offset = min(st.Offset+f.chunk, v.Bytes)
	st.ChunkIndex++
	return st, nil
}
func (f *fakeAdapter) Finish(_ context.Context, _ Account, _ Post, st UploadState) (UploadState, error) {
	f.finishes++
	return st, nil
}
func (f *fakeAdapter) Status(context.Context, Account, UploadState) (Result, error) {
	r := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return r, nil
}

type resumingAdapter struct{ *fakeAdapter }

func (r resumingAdapter) Resume(_ context.Context, _ Account, _ Video, st UploadState) (UploadState, error) {
	r.resumes++
	st.Offset = 20 // the platform says it has 20 bytes
	return st, nil
}

type memStore struct{ saves []Job }

func (m *memStore) Save(_ context.Context, j *Job) error { m.saves = append(m.saves, *j); return nil }

type fixedAccounts struct{ err error }

func (f fixedAccounts) Account(context.Context, string, string) (Account, error) {
	return Account{Token: "t", Scopes: []string{"https://www.googleapis.com/auth/youtube.upload"}}, f.err
}

type switches map[string]bool

func (s switches) Enabled(_ context.Context, p string) bool { return s[p] }

func newJob() *Job {
	return &Job{ID: "j", Platform: YouTube, Status: StatusPending,
		Video: Video{URL: "https://m/v.mp4", Bytes: 30, DurationMS: 20000, Width: 720, Height: 1280},
		Post:  Post{Caption: "hi", AIGenerated: true}}
}

func TestRunnerHappyPathSavesEveryChunk(t *testing.T) {
	ad := &fakeAdapter{platform: YouTube, chunk: 10, statuses: []Result{{}, {Done: true, Published: true, ExternalID: "v1", Permalink: "p"}}}
	st := &memStore{}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	r := NewRunner(st, fixedAccounts{}, switches{YouTube: true}, ad)
	r.Now = func() time.Time { return now }
	j := newJob()

	if err := r.Step(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	// start + 3 chunks + finish + the still-processing poll
	if ad.uploads != 3 || ad.finishes != 1 || j.Status != StatusProcessing || len(st.saves) != 6 {
		t.Fatalf("after step 1: uploads=%d finishes=%d status=%s saves=%d", ad.uploads, ad.finishes, j.Status, len(st.saves))
	}
	if !j.ScheduledAt.Equal(now.Add(ProcessingPoll)) {
		t.Fatalf("poll not rescheduled: %v", j.ScheduledAt)
	}
	for i, s := range st.saves[1:4] {
		if s.State.ChunkIndex != i+1 {
			t.Fatalf("chunk %d not saved on its own: %+v", i, s.State)
		}
	}
	if err := r.Step(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if j.Status != StatusPublished || j.ExternalID != "v1" || ad.starts != 1 || ad.uploads != 3 {
		t.Fatalf("second step must only poll: %+v starts=%d uploads=%d", j, ad.starts, ad.uploads)
	}
	if err := r.Step(context.Background(), j); err != nil || ad.starts != 1 {
		t.Fatal("a published job must never run again")
	}
}

func TestRunnerRetryResumesFromStoredOffset(t *testing.T) {
	ad := &fakeAdapter{platform: YouTube, chunk: 10, uploadErr: []error{nil, errors.New("timeout")}, statuses: []Result{{Done: true, Published: true}}}
	st := &memStore{}
	r := NewRunner(st, fixedAccounts{}, switches{YouTube: true}, resumingAdapter{ad})
	j := newJob()
	_ = r.Step(context.Background(), j)
	if j.Status != StatusUploading || j.Attempts != 1 || j.State.Offset != 10 || !strings.Contains(j.Error, "timeout") {
		t.Fatalf("after a chunk error: %+v", j)
	}
	_ = r.Step(context.Background(), j)
	// Resume moved the offset to 20, so only one more chunk is sent.
	if ad.resumes != 1 || ad.starts != 1 || ad.uploads != 3 || j.Status != StatusPublished {
		t.Fatalf("resume: resumes=%d starts=%d uploads=%d status=%s", ad.resumes, ad.starts, ad.uploads, j.Status)
	}
}

func TestRunnerBackoffThenGivesUp(t *testing.T) {
	ad := &fakeAdapter{platform: YouTube, startErr: errors.New("503")}
	st := &memStore{}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	r := NewRunner(st, fixedAccounts{}, switches{YouTube: true}, ad)
	r.Now = func() time.Time { return now }
	j := newJob()
	for i, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute} {
		_ = r.Step(context.Background(), j)
		if j.Status != StatusPending || !j.ScheduledAt.Equal(now.Add(wait)) {
			t.Fatalf("try %d: %s at %v", i+1, j.Status, j.ScheduledAt)
		}
	}
	_ = r.Step(context.Background(), j)
	if j.Status != StatusFailed || !strings.Contains(j.Error, "gave up after 5 tries") {
		t.Fatalf("fifth error must fail: %+v", j)
	}
}

func TestRunnerExpiredSessionStartsOver(t *testing.T) {
	ad := &fakeAdapter{platform: YouTube, chunk: 10, uploadErr: []error{ErrSessionExpired}, statuses: []Result{{Done: true, Published: true}}}
	r := NewRunner(&memStore{}, fixedAccounts{}, switches{YouTube: true}, ad)
	j := newJob()
	_ = r.Step(context.Background(), j)
	if j.Status != StatusPending || j.State.SessionURI != "" {
		t.Fatalf("expired session must reset: %+v", j)
	}
	_ = r.Step(context.Background(), j)
	if ad.starts != 2 || j.Status != StatusPublished {
		t.Fatalf("restart: starts=%d status=%s", ad.starts, j.Status)
	}
}

func TestRunnerGuards(t *testing.T) {
	ctx := context.Background()
	ad := &fakeAdapter{platform: YouTube, chunk: 10}

	j := newJob()
	_ = NewRunner(&memStore{}, fixedAccounts{}, switches{}, ad).Step(ctx, j)
	if j.Status != StatusFailed || j.Error != ErrDisabled.Error() || ad.starts != 0 {
		t.Fatalf("switched-off platform: %+v", j)
	}

	j = newJob()
	j.Video.Width, j.Video.Height = 1920, 1080
	_ = NewRunner(&memStore{}, fixedAccounts{}, switches{YouTube: true}, ad).Step(ctx, j)
	if j.Status != StatusFailed || !strings.Contains(j.Error, "vertical") || ad.starts != 0 {
		t.Fatalf("preflight must stop a landscape Short: %+v", j)
	}

	j = newJob()
	_ = NewRunner(&memStore{}, fixedAccounts{err: errors.New("gone")}, switches{YouTube: true}, ad).Step(ctx, j)
	if j.Status != StatusFailed || !strings.Contains(j.Error, "reconnect") {
		t.Fatalf("missing account: %+v", j)
	}

	stuck := &fakeAdapter{platform: YouTube, chunk: 30, statuses: []Result{{}}}
	now := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	r := NewRunner(&memStore{}, fixedAccounts{}, switches{YouTube: true}, stuck)
	r.Now = func() time.Time { return now }
	j = newJob()
	_ = r.Step(ctx, j)
	now = now.Add(ProcessingMaxAge + time.Minute)
	_ = r.Step(ctx, j)
	if j.Status != StatusFailed || !strings.Contains(j.Error, "2 hours") {
		t.Fatalf("endless processing must fail: %+v", j)
	}
}
