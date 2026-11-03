package publishing

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// TikTokDirect posts to the creator's account with the Content Posting API (Direct Post):
//  1. POST /v2/post/publish/creator_info/query/ for the allowed privacy levels, the interaction
//     switches and the longest video the creator may post;
//  2. POST /v2/post/publish/video/init/ with post_info (is_aigc set) and source_info, either
//     PULL_FROM_URL (the media domain must be verified in the TikTok developer portal) or
//     FILE_UPLOAD, which returns an upload_url;
//  3. FILE_UPLOAD only: PUT each chunk with Content-Range (206 means more, 201 means done);
//  4. POST /v2/post/publish/status/fetch/ until PUBLISH_COMPLETE or FAILED.
//
// Until the video.publish audit passes TikTok only allows SELF_ONLY, so Privacy defaults to it.
type TikTokDirect struct {
	API     string // https://open.tiktokapis.com
	Privacy string // a privacy_level the creator allows; SELF_ONLY until audited
	Pull    bool   // PULL_FROM_URL instead of FILE_UPLOAD
	HTTP    *http.Client
}

func NewTikTokDirect(api, privacy string, pull bool) *TikTokDirect {
	if privacy == "" {
		privacy = "SELF_ONLY"
	}
	return &TikTokDirect{API: strings.TrimSuffix(api, "/"), Privacy: privacy, Pull: pull, HTTP: noRedirectClient(5 * time.Minute)}
}

func (*TikTokDirect) Platform() string { return TikTok }

// TikTok chunk rules: at least 5 MB and at most 64 MB a chunk, the last one may carry the
// remainder (up to 128 MB), and a file under 5 MB goes as one chunk.
const tiktokMinChunk = 5 * 1000 * 1000

// tiktokChunks gives the chunk size and count for a file.
func tiktokChunks(size int64) (chunk int64, count int) {
	if size < ChunkSize {
		return size, 1
	}
	return ChunkSize, int(size / ChunkSize) // the remainder rides on the last chunk
}

type tiktokCreator struct {
	PrivacyLevelOptions     []string `json:"privacy_level_options"`
	CommentDisabled         bool     `json:"comment_disabled"`
	DuetDisabled            bool     `json:"duet_disabled"`
	StitchDisabled          bool     `json:"stitch_disabled"`
	MaxVideoPostDurationSec int      `json:"max_video_post_duration_sec"`
}

func (t *TikTokDirect) Start(ctx context.Context, a Account, v Video, p Post) (UploadState, error) {
	var ci tiktokCreator
	if err := t.call(ctx, a, "/v2/post/publish/creator_info/query/", map[string]any{}, &ci); err != nil {
		return UploadState{}, fmt.Errorf("tiktok creator info: %w", err)
	}
	if !slices.Contains(ci.PrivacyLevelOptions, t.Privacy) {
		return UploadState{}, fmt.Errorf("this TikTok account does not allow the %s privacy level", t.Privacy)
	}
	if ci.MaxVideoPostDurationSec > 0 && v.DurationMS > ci.MaxVideoPostDurationSec*1000 {
		return UploadState{}, fmt.Errorf("this TikTok account can post videos up to %d seconds", ci.MaxVideoPostDurationSec)
	}
	postInfo := map[string]any{
		"title":           p.Caption,
		"privacy_level":   t.Privacy,
		"disable_comment": ci.CommentDisabled,
		"disable_duet":    ci.DuetDisabled,
		"disable_stitch":  ci.StitchDisabled,
		"is_aigc":         p.AIGenerated,
	}
	source := map[string]any{"source": "PULL_FROM_URL", "video_url": v.URL}
	if !t.Pull {
		if v.Bytes <= 0 {
			return UploadState{}, fmt.Errorf("tiktok init: unknown file size")
		}
		chunk, count := tiktokChunks(v.Bytes)
		source = map[string]any{"source": "FILE_UPLOAD", "video_size": v.Bytes, "chunk_size": chunk, "total_chunk_count": count}
	}
	var out struct {
		PublishID string `json:"publish_id"`
		UploadURL string `json:"upload_url"`
	}
	if err := t.call(ctx, a, "/v2/post/publish/video/init/", map[string]any{"post_info": postInfo, "source_info": source}, &out); err != nil {
		return UploadState{}, fmt.Errorf("tiktok init: %w", err)
	}
	if out.PublishID == "" || (!t.Pull && out.UploadURL == "") {
		return UploadState{}, fmt.Errorf("tiktok init: no publish_id or upload_url in the answer")
	}
	st := UploadState{SessionURI: out.UploadURL, ExternalID: out.PublishID}
	if t.Pull {
		st.Offset = v.Bytes // TikTok pulls the file itself
	}
	return st, nil
}

// Upload PUTs the chunk at st.ChunkIndex.
func (t *TikTokDirect) Upload(ctx context.Context, _ Account, v Video, st UploadState) (UploadState, error) {
	if t.Pull {
		st.Offset = v.Bytes
		return st, nil
	}
	chunk, count := tiktokChunks(v.Bytes)
	if st.ChunkIndex >= count {
		st.Offset = v.Bytes
		return st, nil
	}
	from := int64(st.ChunkIndex) * chunk
	to := from + chunk - 1
	if st.ChunkIndex == count-1 {
		to = v.Bytes - 1
	}
	b, err := readRange(ctx, t.HTTP, v.URL, from, to)
	if err != nil {
		return st, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, st.SessionURI, bytes.NewReader(b))
	if err != nil {
		return st, err
	}
	req.ContentLength = int64(len(b))
	req.Header.Set("Content-Type", "video/mp4")
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, to, v.Bytes))
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return st, fmt.Errorf("tiktok upload: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusCreated {
		return st, fmt.Errorf("tiktok upload: chunk %d answered %d", st.ChunkIndex, resp.StatusCode)
	}
	st.ChunkIndex++
	st.Offset = to + 1
	return st, nil
}

// Finish has nothing to send: Direct Post publishes once TikTok has the whole file.
func (*TikTokDirect) Finish(_ context.Context, _ Account, _ Post, st UploadState) (UploadState, error) {
	return st, nil
}

func (t *TikTokDirect) Status(ctx context.Context, a Account, st UploadState) (Result, error) {
	var out struct {
		Status     string   `json:"status"`
		FailReason string   `json:"fail_reason"`
		PostIDs    []string `json:"publicaly_available_post_id"` // TikTok's spelling
	}
	if err := t.call(ctx, a, "/v2/post/publish/status/fetch/", map[string]any{"publish_id": st.ExternalID}, &out); err != nil {
		return Result{}, fmt.Errorf("tiktok status: %w", err)
	}
	switch out.Status {
	case "PUBLISH_COMPLETE":
		r := Result{Done: true, Published: true, ExternalID: st.ExternalID}
		if len(out.PostIDs) > 0 {
			r.ExternalID = out.PostIDs[0]
			r.Permalink = "https://www.tiktok.com/video/" + out.PostIDs[0]
		}
		return r, nil
	case "FAILED":
		msg := out.FailReason
		if msg == "" {
			msg = "unknown reason"
		}
		return Result{Done: true, ExternalID: st.ExternalID, Error: "TikTok could not post the video: " + msg}, nil
	}
	return Result{ExternalID: st.ExternalID}, nil // PROCESSING_UPLOAD, PROCESSING_DOWNLOAD
}

// call POSTs JSON and unwraps TikTok's {data, error} envelope; error.code "ok" means success.
func (t *TikTokDirect) call(ctx context.Context, a Account, path string, in, data any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.API+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&env); err != nil {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if env.Error.Code != "" && env.Error.Code != "ok" {
		return fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if len(env.Data) == 0 {
		return nil
	}
	return json.Unmarshal(env.Data, data)
}
