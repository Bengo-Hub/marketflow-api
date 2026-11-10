package publishing

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/integrations/meta"
)

// FacebookReels posts a reel to a Facebook Page (Graph v26.0, Reels publishing guide):
//  1. POST /{page-id}/video_reels upload_phase=start, which returns video_id and upload_url;
//  2. POST {upload_url} with the file_url header, so Meta pulls the public file and no bytes
//     pass through the pod (the offset and file_size headers say where to resume);
//  3. POST /{page-id}/video_reels upload_phase=finish video_state=PUBLISHED with is_ai_generated;
//  4. GET /{video-id}?fields=status until publishing completes or fails.
type FacebookReels struct {
	Graph string // https://graph.facebook.com/v26.0
	HTTP  *http.Client
}

// NewFacebookReels uses the platform Graph version (meta.GraphVersion is v26.0).
func NewFacebookReels(graphBase string) *FacebookReels {
	return &FacebookReels{Graph: strings.TrimSuffix(graphBase, "/"), HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// WithProof signs every Graph call with appsecret_proof (needed once "Require app secret" is on
// in the Meta app); secret is meta.PlatformSecret in production.
func (f *FacebookReels) WithProof(secret meta.SecretSource) *FacebookReels {
	f.HTTP.Transport = meta.NewProofTransport(f.HTTP.Transport, secret)
	return f
}

func (*FacebookReels) Platform() string { return Facebook }

func (f *FacebookReels) Start(ctx context.Context, a Account, _ Video, _ Post) (UploadState, error) {
	var out struct {
		VideoID   string `json:"video_id"`
		UploadURL string `json:"upload_url"`
	}
	form := url.Values{"upload_phase": {"start"}, "access_token": {a.Token}}
	if err := f.postForm(ctx, f.Graph+"/"+url.PathEscape(a.ExternalID)+"/video_reels", form, &out); err != nil {
		return UploadState{}, fmt.Errorf("facebook start: %w", err)
	}
	if out.VideoID == "" || out.UploadURL == "" {
		return UploadState{}, fmt.Errorf("facebook start: no video_id or upload_url in the answer")
	}
	return UploadState{SessionURI: out.UploadURL, ExternalID: out.VideoID}, nil
}

func (f *FacebookReels) Upload(ctx context.Context, a Account, v Video, st UploadState) (UploadState, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, st.SessionURI, nil)
	if err != nil {
		return st, err
	}
	req.Header.Set("Authorization", "OAuth "+a.Token)
	req.Header.Set("file_url", v.URL)
	req.Header.Set("offset", strconv.FormatInt(st.Offset, 10))
	req.Header.Set("file_size", strconv.FormatInt(v.Bytes, 10))
	var out struct {
		Success bool `json:"success"`
	}
	if err := f.do(req, &out); err != nil {
		return st, fmt.Errorf("facebook upload: %w", err)
	}
	if !out.Success {
		return st, fmt.Errorf("facebook upload: not accepted")
	}
	st.Offset = v.Bytes
	return st, nil
}

func (f *FacebookReels) Finish(ctx context.Context, a Account, p Post, st UploadState) (UploadState, error) {
	form := url.Values{
		"upload_phase":    {"finish"},
		"video_id":        {st.ExternalID},
		"video_state":     {"PUBLISHED"},
		"description":     {p.Caption},
		"is_ai_generated": {strconv.FormatBool(p.AIGenerated)},
		"access_token":    {a.Token},
	}
	var out struct {
		Success bool `json:"success"`
	}
	if err := f.postForm(ctx, f.Graph+"/"+url.PathEscape(a.ExternalID)+"/video_reels", form, &out); err != nil {
		return st, fmt.Errorf("facebook finish: %w", err)
	}
	if !out.Success {
		return st, fmt.Errorf("facebook finish: not accepted")
	}
	return st, nil
}

func (f *FacebookReels) Status(ctx context.Context, a Account, st UploadState) (Result, error) {
	u := f.Graph + "/" + url.PathEscape(st.ExternalID) + "?fields=status,permalink_url&access_token=" + url.QueryEscape(a.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Status struct {
			VideoStatus     string `json:"video_status"`
			PublishingPhase struct {
				Status        string `json:"status"`
				PublishStatus string `json:"publish_status"`
			} `json:"publishing_phase"`
			ProcessingPhase struct {
				Status string `json:"status"`
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			} `json:"processing_phase"`
		} `json:"status"`
		PermalinkURL string `json:"permalink_url"`
	}
	if err := f.do(req, &out); err != nil {
		return Result{}, fmt.Errorf("facebook status: %w", err)
	}
	s := out.Status
	switch {
	case s.VideoStatus == "error" || s.VideoStatus == "upload_failed" || s.VideoStatus == "expired" ||
		s.ProcessingPhase.Status == "error" || s.PublishingPhase.Status == "error":
		msg := "Facebook could not process the video"
		if len(s.ProcessingPhase.Errors) > 0 {
			msg = s.ProcessingPhase.Errors[0].Message
		}
		return Result{Done: true, Error: msg, ExternalID: st.ExternalID}, nil
	case s.PublishingPhase.PublishStatus == "published" || s.PublishingPhase.Status == "completed":
		link := out.PermalinkURL
		if link != "" && !strings.HasPrefix(link, "http") {
			link = "https://www.facebook.com" + link
		}
		return Result{Done: true, Published: true, ExternalID: st.ExternalID, Permalink: link}, nil
	}
	return Result{ExternalID: st.ExternalID}, nil
}

func (f *FacebookReels) postForm(ctx context.Context, u string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.do(req, out)
}

// do sends the request and decodes the JSON answer; a Graph error becomes the error text.
func (f *FacebookReels) do(req *http.Request, out any) error {
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var ge struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if json.Unmarshal(body, &ge) == nil && ge.Error.Message != "" {
			return fmt.Errorf("%d %s: %s", resp.StatusCode, ge.Error.Type, ge.Error.Message)
		}
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}
