// Package publishing posts approved creative videos to tenants' own social accounts (Sprint 16).
//
// One Adapter per platform behind one port, so the job runner, the resume logic, preflight and
// the calendar never branch on the platform. Platform contracts were checked against the
// official docs on 2026-11-03: docs/integrations/organic-publishing-apis.md (marketflow docs).
//
// Every platform is off until its approval lands (owner rule, 2026-11-03: build first, switch on
// later): Meta App Review (pages_manage_posts, instagram_content_publish), Google OAuth
// verification (youtube.upload) and the TikTok video.publish audit. The switches are the
// platform ProviderSetting publishing/<platform> enabled flag, read through Enabled.
package publishing

import (
	"context"
	"errors"
)

// Platforms.
const (
	Facebook  = "facebook"
	Instagram = "instagram"
	YouTube   = "youtube"
	TikTok    = "tiktok"
)

// ChunkSize is the upload chunk: 8 MiB, a multiple of 256 KiB (YouTube's rule) and inside
// TikTok's 5 to 64 MB range. One chunk is held in memory at a time.
const ChunkSize = 8 << 20

// Video describes the file to post, read from the approved creative asset.
type Video struct {
	URL        string // public object-storage URL (Facebook and Instagram pull it with file_url)
	Bytes      int64
	DurationMS int
	Width      int
	Height     int
	FPS        float64
}

// Post is what one job publishes.
type Post struct {
	Caption string
	// AIGenerated sets every platform's AI label: is_ai_generated (Facebook, Instagram),
	// containsSyntheticMedia (YouTube), is_aigc (TikTok). Creative Studio videos always set it.
	AIGenerated bool
	// MadeForKids is YouTube's selfDeclaredMadeForKids, always sent explicitly.
	MadeForKids bool
}

// UploadState is the resumable position, stored in PublishJob.metadata after every
// acknowledged chunk.
type UploadState struct {
	SessionURI string `json:"session_uri,omitempty"` // YouTube session, rupload URL, TikTok upload_url
	ExternalID string `json:"external_id,omitempty"` // video_id, container id or publish_id
	Offset     int64  `json:"offset"`
	ChunkIndex int    `json:"chunk_index"`
}

// Result is a platform's answer to a status check.
type Result struct {
	Done       bool // published or failed for good
	Published  bool
	ExternalID string
	Permalink  string
	Error      string
}

// Adapter is one platform's publishing protocol.
type Adapter interface {
	Platform() string
	// Start opens the upload (a session, a container, an init) and returns the first state.
	Start(ctx context.Context, acct Account, v Video, p Post) (UploadState, error)
	// Upload sends from st.Offset and returns the new state; a platform that pulls the file by
	// URL returns the final state at once.
	Upload(ctx context.Context, acct Account, v Video, st UploadState) (UploadState, error)
	// Finish publishes the uploaded video (Facebook finish, Instagram media_publish).
	Finish(ctx context.Context, acct Account, p Post, st UploadState) (UploadState, error)
	// Status reports processing on the platform after Finish.
	Status(ctx context.Context, acct Account, st UploadState) (Result, error)
}

// Account is the connected account a job posts to, with a decrypted token.
type Account struct {
	ID         string
	ExternalID string // Page id, IG user id, YouTube channel, TikTok open_id
	Token      string
	Scopes     []string
}

// ErrDisabled means the platform's switch is off (its approval has not landed).
var ErrDisabled = errors.New("publishing to this platform is not switched on yet")
