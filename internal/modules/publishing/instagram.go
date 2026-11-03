package publishing

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// InstagramReels posts a reel to an Instagram professional account (Graph v26.0, content
// publishing guide):
//  1. POST /{ig-user-id}/media media_type=REELS upload_type=resumable (caption, is_ai_generated),
//     which returns the container id and its rupload uri;
//  2. POST {uri} with the file_url header, so Meta pulls the public file;
//  3. GET /{container}?fields=status_code until FINISHED (or ERROR / EXPIRED);
//  4. POST /{ig-user-id}/media_publish creation_id={container}.
//
// The account's daily limit is read from content_publishing_limit before Start (the docs give
// both 50 and 100 a day), so a full quota fails before any upload.
type InstagramReels struct {
	fb *FacebookReels // the same Graph transport and error decoding
}

// NewInstagramReels uses the platform Graph version (v26.0).
func NewInstagramReels(graphBase string) *InstagramReels {
	return &InstagramReels{fb: NewFacebookReels(graphBase)}
}

func (*InstagramReels) Platform() string { return Instagram }

// QuotaLeft is how many more posts the account may publish in the current 24 hours.
func (g *InstagramReels) QuotaLeft(ctx context.Context, a Account) (int, error) {
	u := g.fb.Graph + "/" + url.PathEscape(a.ExternalID) + "/content_publishing_limit?fields=config,quota_usage&access_token=" + url.QueryEscape(a.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	var out struct {
		Data []struct {
			QuotaUsage int `json:"quota_usage"`
			Config     struct {
				QuotaTotal int `json:"quota_total"`
			} `json:"config"`
		} `json:"data"`
	}
	if err := g.fb.do(req, &out); err != nil {
		return 0, fmt.Errorf("instagram publishing limit: %w", err)
	}
	if len(out.Data) == 0 {
		return 0, fmt.Errorf("instagram publishing limit: empty answer")
	}
	return out.Data[0].Config.QuotaTotal - out.Data[0].QuotaUsage, nil
}

func (g *InstagramReels) Start(ctx context.Context, a Account, _ Video, p Post) (UploadState, error) {
	left, err := g.QuotaLeft(ctx, a)
	if err != nil {
		return UploadState{}, err
	}
	if left <= 0 {
		return UploadState{}, fmt.Errorf("this Instagram account has used its posts for the last 24 hours; try again later")
	}
	form := url.Values{
		"media_type":      {"REELS"},
		"upload_type":     {"resumable"},
		"caption":         {p.Caption},
		"share_to_feed":   {"true"},
		"is_ai_generated": {strconv.FormatBool(p.AIGenerated)},
		"access_token":    {a.Token},
	}
	var out struct {
		ID  string `json:"id"`
		URI string `json:"uri"`
	}
	if err := g.fb.postForm(ctx, g.fb.Graph+"/"+url.PathEscape(a.ExternalID)+"/media", form, &out); err != nil {
		return UploadState{}, fmt.Errorf("instagram container: %w", err)
	}
	if out.ID == "" || out.URI == "" {
		return UploadState{}, fmt.Errorf("instagram container: no id or uri in the answer")
	}
	return UploadState{SessionURI: out.URI, ExternalID: out.ID}, nil
}

// Upload hands Meta the public file URL; the container then processes on its own.
func (g *InstagramReels) Upload(ctx context.Context, a Account, v Video, st UploadState) (UploadState, error) {
	return g.fb.Upload(ctx, a, v, st)
}

// Finish waits for the container to be FINISHED, then publishes it. It polls once every
// pollEvery for at most pollFor, the guide's advice (once a minute, up to 5 minutes).
func (g *InstagramReels) Finish(ctx context.Context, a Account, _ Post, st UploadState) (UploadState, error) {
	deadline := time.Now().Add(g.pollFor())
	for {
		code, err := g.containerStatus(ctx, a, st.ExternalID)
		if err != nil {
			return st, err
		}
		switch code {
		case "FINISHED":
			var out struct {
				ID string `json:"id"`
			}
			form := url.Values{"creation_id": {st.ExternalID}, "access_token": {a.Token}}
			if err := g.fb.postForm(ctx, g.fb.Graph+"/"+url.PathEscape(a.ExternalID)+"/media_publish", form, &out); err != nil {
				return st, fmt.Errorf("instagram publish: %w", err)
			}
			if out.ID == "" {
				return st, fmt.Errorf("instagram publish: no media id in the answer")
			}
			st.ExternalID = out.ID
			return st, nil
		case "ERROR", "EXPIRED":
			return st, fmt.Errorf("instagram could not process the video (%s)", strings.ToLower(code))
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("instagram is still processing the video; the job will try again")
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(g.pollEvery()):
		}
	}
}

// Status reads the published media's permalink.
func (g *InstagramReels) Status(ctx context.Context, a Account, st UploadState) (Result, error) {
	u := g.fb.Graph + "/" + url.PathEscape(st.ExternalID) + "?fields=permalink,media_product_type&access_token=" + url.QueryEscape(a.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return Result{}, err
	}
	var out struct {
		Permalink string `json:"permalink"`
	}
	if err := g.fb.do(req, &out); err != nil {
		return Result{}, fmt.Errorf("instagram status: %w", err)
	}
	return Result{Done: true, Published: true, ExternalID: st.ExternalID, Permalink: out.Permalink}, nil
}

func (g *InstagramReels) containerStatus(ctx context.Context, a Account, container string) (string, error) {
	u := g.fb.Graph + "/" + url.PathEscape(container) + "?fields=status_code&access_token=" + url.QueryEscape(a.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	var out struct {
		StatusCode string `json:"status_code"`
	}
	if err := g.fb.do(req, &out); err != nil {
		return "", fmt.Errorf("instagram container status: %w", err)
	}
	return out.StatusCode, nil
}

// Poll timing, shortened by tests.
var (
	igPollEvery = time.Minute
	igPollFor   = 5 * time.Minute
)

func (*InstagramReels) pollEvery() time.Duration { return igPollEvery }
func (*InstagramReels) pollFor() time.Duration   { return igPollFor }
