package publishing

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
)

// Delete from platform (owner decision 2026-11-14): a post MarketFlow published can be taken
// down from the platform where the platform's API lets the app delete its own post. Platform
// contracts were checked against the official docs on 2026-11-14:
//   - Facebook Page: DELETE /{object-id} with the Page token answers {"success": true}
//     (Pages API, Posts guide "Delete a post"); the reel's stored id is its video id.
//   - Instagram: DELETE /{ig-media-id} deletes non-ad posts, stories and reels, Instagram API with
//     Facebook Login only, with instagram_basic and instagram_manage_contents (IG Media reference,
//     "Deleting"). The answer is {"success": true, "deleted_id": "..."}.
//   - YouTube: DELETE /youtube/v3/videos?id= answers 204, and needs youtube, youtube.force-ssl or
//     youtubepartner; youtube.upload alone is not enough (videos.delete reference).
//   - TikTok: the Content Posting API offers creator info, init, upload and status only; there is
//     no delete, so the creator deletes the video in the TikTok app.

// StatusDeletedOnPlatform is a published job whose post was deleted from the platform through
// MarketFlow. deleted_at and deleted_by are kept in the job's metadata.
const StatusDeletedOnPlatform = "deleted_on_platform"

// ErrDeleteUnsupported means the platform's API cannot delete the post for us; the tenant deletes
// it on the platform itself. DeleteUnsupportedError carries the platform's own steps.
var ErrDeleteUnsupported = errors.New("this platform does not let apps delete a post")

// ErrAlreadyGone means the platform no longer has the post (deleted by hand, or removed by the
// platform). A delete that finds it gone has done its job.
var ErrAlreadyGone = errors.New("the platform no longer has this post")

// DeleteUnsupportedError says why a post cannot be deleted from MarketFlow and how to delete it
// on the platform.
type DeleteUnsupportedError struct {
	Platform string
	Reason   string
	Steps    []string
	HelpURL  string
}

func (e *DeleteUnsupportedError) Error() string { return e.Reason }

func (e *DeleteUnsupportedError) Is(target error) bool { return target == ErrDeleteUnsupported }

// Deleter is an adapter that can delete a post it published. externalID is the job's stored
// platform id (video id, media id).
type Deleter interface {
	Platform() string
	Delete(ctx context.Context, acct Account, externalID string) error
}

// deleteSpec is what a platform needs before MarketFlow may delete one of its posts.
type deleteSpec struct {
	Supported bool
	// AnyScopes lists permission sets; the account must hold every scope of at least one set.
	AnyScopes [][]string
	// Steps and HelpURL are the platform's own way to delete a post by hand.
	Steps   []string
	HelpURL string
}

var deleteSpecs = map[string]deleteSpec{
	Facebook: {Supported: true,
		AnyScopes: [][]string{{"pages_manage_posts"}},
		Steps: []string{
			"Open the reel on your Facebook Page.",
			"Select the three dots at the top right of the post.",
			"Select Delete and confirm.",
		},
		HelpURL: "https://www.facebook.com/help"},
	Instagram: {Supported: true,
		AnyScopes: [][]string{{"instagram_basic", "instagram_manage_contents"}},
		Steps: []string{
			"Open the reel in the Instagram app.",
			"Tap the three dots on the reel.",
			"Tap Delete and confirm.",
		},
		HelpURL: "https://help.instagram.com"},
	YouTube: {Supported: true,
		AnyScopes: [][]string{
			{"https://www.googleapis.com/auth/youtube"},
			{"https://www.googleapis.com/auth/youtube.force-ssl"},
			{"https://www.googleapis.com/auth/youtubepartner"},
		},
		Steps: []string{
			"Sign in to YouTube Studio.",
			"Select Content and find the Short.",
			"Point to the video, select the three dots (Options) and choose Delete forever.",
			"Tick the box that says the video is deleted for good, then select Delete forever.",
		},
		HelpURL: "https://support.google.com/youtube/answer/55770"},
	TikTok: {Supported: false,
		Steps: []string{
			"Open the video in the TikTok app.",
			"Tap the three dots (or the Share button) on the video.",
			"Tap Delete and confirm.",
		},
		HelpURL: "https://support.tiktok.com"},
}

// YouTubeConnectScopes are the Google scopes a YouTube channel connection must request (owner
// 2026-11-14): youtube.upload to post and youtube.force-ssl to delete what MarketFlow posted.
var YouTubeConnectScopes = []string{
	"https://www.googleapis.com/auth/youtube.upload",
	"https://www.googleapis.com/auth/youtube.force-ssl",
}

// NeedsReconnectToDelete is true for an account on a platform that can delete, whose stored
// scopes lack the delete permission: the tenant connected before it was asked for and must
// reconnect to grant it. Unknown scopes (none stored) give no hint.
func NeedsReconnectToDelete(platform string, scopes []string) bool {
	sp, ok := deleteSpecs[platform]
	if !ok || !sp.Supported || len(scopes) == 0 {
		return false
	}
	return !DeleteSupport(platform, scopes).CanDelete
}

// DeleteInfo says whether a post on this platform, made with an account holding these scopes,
// can be deleted from MarketFlow, and if not, why and how to delete it on the platform.
type DeleteInfo struct {
	CanDelete bool
	Reason    string
	Steps     []string
	HelpURL   string
}

// DeleteSupport reads the platform's delete rules for an account's granted scopes.
func DeleteSupport(platform string, scopes []string) DeleteInfo {
	sp, ok := deleteSpecs[platform]
	if !ok {
		return DeleteInfo{Reason: fmt.Sprintf("%s is not a publishing platform", platform)}
	}
	info := DeleteInfo{Steps: sp.Steps, HelpURL: sp.HelpURL}
	if !sp.Supported {
		info.Reason = fmt.Sprintf("%s does not let apps delete a post. Delete it on %s.", title(platform), title(platform))
		return info
	}
	for _, set := range sp.AnyScopes {
		if hasAll(scopes, set) {
			info.CanDelete = true
			return info
		}
	}
	info.Reason = fmt.Sprintf("the connected %s account has not given MarketFlow permission to delete posts. Delete it on %s, or reconnect the account once that permission is offered.",
		title(platform), title(platform))
	return info
}

// Unsupported turns a refusal into the error the handler shows.
func (i DeleteInfo) Unsupported(platform string) *DeleteUnsupportedError {
	return &DeleteUnsupportedError{Platform: platform, Reason: i.Reason, Steps: i.Steps, HelpURL: i.HelpURL}
}

func hasAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// Delete removes the Page reel: DELETE /{video-id} with the Page token.
func (f *FacebookReels) Delete(ctx context.Context, a Account, externalID string) error {
	if err := f.graphDelete(ctx, a, externalID); err != nil {
		return fmt.Errorf("facebook delete: %w", err)
	}
	return nil
}

// Delete removes the published reel: DELETE /{ig-media-id}.
func (g *InstagramReels) Delete(ctx context.Context, a Account, externalID string) error {
	if err := g.fb.graphDelete(ctx, a, externalID); err != nil {
		return fmt.Errorf("instagram delete: %w", err)
	}
	return nil
}

func (f *FacebookReels) graphDelete(ctx context.Context, a Account, id string) error {
	if id == "" {
		return errors.New("no platform id stored for this post")
	}
	u := f.Graph + "/" + url.PathEscape(id) + "?access_token=" + url.QueryEscape(a.Token)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, u, nil)
	if err != nil {
		return err
	}
	var out struct {
		Success bool `json:"success"`
	}
	// Any Graph error comes back with Meta's message. Graph's "does not exist" answer (code 100,
	// subcode 33) also covers "cannot be loaded due to missing permissions", so it is never read
	// as already gone: marking a live post deleted would be worse than asking again.
	if err := f.do(req, &out); err != nil {
		return err
	}
	if !out.Success {
		return errors.New("not accepted")
	}
	return nil
}

// Delete removes the video: DELETE {api}/videos?id=. 204 is done, 404 means it is already gone.
func (y *YouTubeShorts) Delete(ctx context.Context, a Account, externalID string) error {
	if externalID == "" {
		return errors.New("youtube delete: no platform id stored for this post")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, y.APIBase+"/videos?id="+url.QueryEscape(externalID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.Token)
	resp, err := y.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("youtube delete: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return ErrAlreadyGone
	}
	return fmt.Errorf("youtube delete: %w", googleError(resp))
}

// Delete always refuses: the Content Posting API has no delete.
func (*TikTokDirect) Delete(context.Context, Account, string) error {
	return DeleteSupport(TikTok, nil).Unsupported(TikTok)
}
