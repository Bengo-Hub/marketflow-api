package publishing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// YouTubeShorts uploads a vertical video to the tenant's channel with the Data API v3 resumable
// protocol:
//  1. POST {upload}/videos?uploadType=resumable&part=snippet,status with the metadata, which
//     returns the session URI in Location;
//  2. PUT one ChunkSize chunk per Upload call with Content-Range; 308 means more is wanted and
//     its Range header says how much arrived, 200 or 201 carries the video resource;
//  3. after a lost answer, PUT an empty body with "bytes */total" to learn the offset; a 404
//     means the session expired and the job starts over.
//
// Finish has nothing to do (the last chunk publishes); Status reads uploadStatus. The insert
// docs now say unverified projects are not forced private, while older guidance said they were,
// so Privacy is configurable (MF_PUBLISHING_YOUTUBE_PRIVACY) until the first real upload shows
// which holds.
type YouTubeShorts struct {
	UploadBase string // https://www.googleapis.com/upload/youtube/v3
	APIBase    string // https://www.googleapis.com/youtube/v3
	Privacy    string // public, unlisted or private
	HTTP       *http.Client
}

// ErrSessionExpired means the resumable session is gone (404); the runner clears the state and
// calls Start again.
var ErrSessionExpired = errors.New("youtube upload session expired")

func NewYouTubeShorts(uploadBase, apiBase, privacy string) *YouTubeShorts {
	if privacy == "" {
		privacy = "public"
	}
	return &YouTubeShorts{
		UploadBase: strings.TrimSuffix(uploadBase, "/"),
		APIBase:    strings.TrimSuffix(apiBase, "/"),
		Privacy:    privacy,
		HTTP:       noRedirectClient(5 * time.Minute),
	}
}

func (*YouTubeShorts) Platform() string { return YouTube }

func (y *YouTubeShorts) Start(ctx context.Context, a Account, v Video, p Post) (UploadState, error) {
	meta := map[string]any{
		"snippet": map[string]any{
			"title":       youtubeTitle(p.Caption),
			"description": p.Caption,
		},
		"status": map[string]any{
			"privacyStatus":           y.Privacy,
			"selfDeclaredMadeForKids": p.MadeForKids,
			"containsSyntheticMedia":  p.AIGenerated,
		},
	}
	body, _ := json.Marshal(meta)
	u := y.UploadBase + "/videos?uploadType=resumable&part=snippet,status"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return UploadState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("X-Upload-Content-Length", strconv.FormatInt(v.Bytes, 10))
	req.Header.Set("X-Upload-Content-Type", "video/mp4")
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return UploadState{}, fmt.Errorf("youtube start: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return UploadState{}, fmt.Errorf("youtube start: %w", googleError(resp))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return UploadState{}, fmt.Errorf("youtube start: no session uri in the answer")
	}
	return UploadState{SessionURI: loc}, nil
}

// Upload sends the chunk at st.Offset, read from the asset URL with a Range request so only one
// chunk is ever in memory.
func (y *YouTubeShorts) Upload(ctx context.Context, a Account, v Video, st UploadState) (UploadState, error) {
	if v.Bytes <= 0 {
		return st, fmt.Errorf("youtube upload: unknown file size")
	}
	end := st.Offset + ChunkSize
	if end > v.Bytes {
		end = v.Bytes
	}
	chunk, err := readRange(ctx, y.HTTP, v.URL, st.Offset, end-1)
	if err != nil {
		return st, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, st.SessionURI, bytes.NewReader(chunk))
	if err != nil {
		return st, err
	}
	req.ContentLength = int64(len(chunk))
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "video/mp4")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", st.Offset, end-1, v.Bytes))
	return y.handleUploadAnswer(req, v, st)
}

// Resume asks the session how many bytes it holds, after a timeout or a 5xx.
func (y *YouTubeShorts) Resume(ctx context.Context, a Account, v Video, st UploadState) (UploadState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, st.SessionURI, nil)
	if err != nil {
		return st, err
	}
	req.ContentLength = 0
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", v.Bytes))
	return y.handleUploadAnswer(req, v, st)
}

func (y *YouTubeShorts) handleUploadAnswer(req *http.Request, v Video, st UploadState) (UploadState, error) {
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return st, fmt.Errorf("youtube upload: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		var out struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.ID == "" {
			return st, fmt.Errorf("youtube upload: no video id in the answer")
		}
		st.ExternalID, st.Offset = out.ID, v.Bytes
		st.ChunkIndex++
		return st, nil
	case http.StatusPermanentRedirect: // 308 Resume Incomplete
		st.Offset = rangeEnd(resp.Header.Get("Range"))
		st.ChunkIndex++
		return st, nil
	case http.StatusNotFound:
		return UploadState{}, ErrSessionExpired
	}
	return st, fmt.Errorf("youtube upload: %w", googleError(resp))
}

// rangeEnd turns "bytes=0-8388607" into the next offset (8388608); no header means nothing
// arrived yet.
func rangeEnd(h string) int64 {
	i := strings.LastIndexByte(h, '-')
	if i < 0 {
		return 0
	}
	n, err := strconv.ParseInt(h[i+1:], 10, 64)
	if err != nil {
		return 0
	}
	return n + 1
}

// Finish has nothing left to send: the last chunk created the video.
func (*YouTubeShorts) Finish(_ context.Context, _ Account, _ Post, st UploadState) (UploadState, error) {
	if st.ExternalID == "" {
		return st, fmt.Errorf("youtube finish: the upload has not completed")
	}
	return st, nil
}

func (y *YouTubeShorts) Status(ctx context.Context, a Account, st UploadState) (Result, error) {
	u := y.APIBase + "/videos?part=status&id=" + url.QueryEscape(st.ExternalID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("youtube status: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("youtube status: %w", googleError(resp))
	}
	var out struct {
		Items []struct {
			Status struct {
				UploadStatus    string `json:"uploadStatus"`
				FailureReason   string `json:"failureReason"`
				RejectionReason string `json:"rejectionReason"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return Result{}, fmt.Errorf("youtube status: %w", err)
	}
	if len(out.Items) == 0 {
		return Result{Done: true, ExternalID: st.ExternalID, Error: "YouTube no longer has this video"}, nil
	}
	s := out.Items[0].Status
	switch s.UploadStatus {
	case "processed":
		return Result{Done: true, Published: true, ExternalID: st.ExternalID, Permalink: "https://www.youtube.com/shorts/" + st.ExternalID}, nil
	case "failed", "rejected", "deleted":
		msg := s.FailureReason
		if msg == "" {
			msg = s.RejectionReason
		}
		if msg == "" {
			msg = s.UploadStatus
		}
		return Result{Done: true, ExternalID: st.ExternalID, Error: "YouTube " + s.UploadStatus + " the video: " + msg}, nil
	}
	return Result{ExternalID: st.ExternalID}, nil // uploaded, still processing
}

// readRange reads bytes from..to of the stored video, so only one chunk is in memory.
func readRange(ctx context.Context, hc *http.Client, src string, from, to int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read video: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		// A store that ignores Range would hand back the whole file; refuse it rather than
		// upload the wrong bytes.
		return nil, fmt.Errorf("read video: storage answered %d to a range request", resp.StatusCode)
	}
	want := to - from + 1
	b, err := io.ReadAll(io.LimitReader(resp.Body, want+1))
	if err != nil {
		return nil, fmt.Errorf("read video: %w", err)
	}
	if int64(len(b)) != want {
		return nil, fmt.Errorf("read video: got %d bytes, wanted %d", len(b), want)
	}
	return b, nil
}

// noRedirectClient never follows a redirect: YouTube's 308 Resume Incomplete is an answer, not a
// redirect.
func noRedirectClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// youtubeTitle is the caption's first line, without the characters YouTube rejects, cut to 100
// characters.
func youtubeTitle(caption string) string {
	t := strings.TrimSpace(strings.SplitN(caption, "\n", 2)[0])
	t = strings.NewReplacer("<", "", ">", "").Replace(t)
	if utf8.RuneCountInString(t) > 100 {
		t = string([]rune(t)[:100])
	}
	if t == "" {
		t = "New video"
	}
	return t
}

// googleError reads the Google API error message.
func googleError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	var ge struct {
		Error struct {
			Message string `json:"message"`
			Errors  []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &ge) == nil && ge.Error.Message != "" {
		if len(ge.Error.Errors) > 0 && ge.Error.Errors[0].Reason != "" {
			return fmt.Errorf("%d %s: %s", resp.StatusCode, ge.Error.Errors[0].Reason, ge.Error.Message)
		}
		return fmt.Errorf("%d: %s", resp.StatusCode, ge.Error.Message)
	}
	return fmt.Errorf("status %d", resp.StatusCode)
}
