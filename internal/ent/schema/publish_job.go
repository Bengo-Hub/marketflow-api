package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/google/uuid"
)

// PublishJob is one post of an approved creative asset to one connected account (Sprint 16,
// organic publishing): a Facebook Page reel, an Instagram reel, a YouTube Short or a TikTok
// video. Platform contracts: docs/integrations/organic-publishing-apis.md.
//
// The upload state (session URI or upload URL, byte offset, chunk index, attempt) lives in
// metadata and is written after every acknowledged chunk, so a redelivered job resumes where it
// stopped. dedupe_key (asset + account) makes a retried publish never post twice.
type PublishJob struct {
	ent.Schema
}

// Fields of the PublishJob.
func (PublishJob) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).
			Default(uuid.New).
			Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.UUID("asset_id", uuid.UUID{}).
			Comment("creative_assets.id of the approved video"),
		field.UUID("connected_account_id", uuid.UUID{}).
			Comment("connected_accounts.id the post goes to"),
		field.String("platform").
			Comment("facebook | instagram | youtube | tiktok"),
		field.String("kind").
			Default("organic").
			Comment("organic | ad_video"),
		field.Text("caption").
			Optional(),
		field.Time("scheduled_at").
			Comment("when to publish; now for an immediate post"),
		field.String("status").
			Default("pending").
			Comment("pending | uploading | processing | published | failed | cancelled"),
		field.String("dedupe_key").
			Unique().
			Comment("asset_id:connected_account_id, so one asset posts once per account"),
		field.String("external_id").
			Optional().
			Comment("the platform's post, media or video id"),
		field.String("permalink").
			Optional(),
		field.Text("error").
			Optional(),
		field.Int("attempts").
			Default(0),
		field.UUID("created_by", uuid.UUID{}).
			Optional().
			Nillable(),
		field.JSON("metadata", map[string]any{}).
			Optional().
			Comment("upload state: session_uri | upload_url, offset, chunk_index, platform status; AI label flags"),
		field.Time("published_at").
			Optional().
			Nillable(),
		field.Time("created_at").
			Default(time.Now).
			Immutable(),
		field.Time("updated_at").
			Default(time.Now).
			UpdateDefault(time.Now),
	}
}

// Indexes of the PublishJob.
func (PublishJob) Indexes() []ent.Index {
	return []ent.Index{
		// The scheduler reads only jobs that still have to run, oldest first.
		index.Fields("status", "scheduled_at").
			Annotations(entsql.IndexWhere("status IN ('pending', 'uploading', 'processing')")),
		// The calendar lists a tenant's jobs by time, with keyset pagination.
		index.Fields("tenant_id", "scheduled_at", "id"),
		index.Fields("tenant_id", "asset_id"),
	}
}
