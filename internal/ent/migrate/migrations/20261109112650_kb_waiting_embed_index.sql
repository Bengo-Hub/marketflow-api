-- Modify "activities" table
ALTER TABLE "activities" ADD COLUMN "metadata" jsonb NULL;
-- Create index "activities_inbox_pending" to table: "activities"
CREATE INDEX "activities_inbox_pending" ON "activities" ("performed_at") WHERE (((activity_type)::text = ANY ((ARRAY['comment'::character varying, 'dm'::character varying])::text[])) AND ((metadata ->> 'intent_status'::text) = 'pending'::text));
-- Create index "activity_tenant_id_activity_type_performed_at" to table: "activities"
CREATE INDEX "activity_tenant_id_activity_type_performed_at" ON "activities" ("tenant_id", "activity_type", "performed_at");
-- Modify "campaigns" table
ALTER TABLE "campaigns" ADD COLUMN "connected_account_id" uuid NULL, ADD COLUMN "creative_version_id" uuid NULL;
-- Create index "campaign_tenant_id_creative_version_id" to table: "campaigns"
CREATE INDEX "campaign_tenant_id_creative_version_id" ON "campaigns" ("tenant_id", "creative_version_id");
-- Create index "campaigns_launching" to table: "campaigns"
CREATE INDEX "campaigns_launching" ON "campaigns" ("updated_at") WHERE ((status)::text = 'launching'::text);
-- Create index "contact_metadata" to table: "contacts"
CREATE INDEX "contact_metadata" ON "contacts" USING gin ("metadata");
-- Drop index "creativeversion_render_queue" from table: "creative_versions"
DROP INDEX "creativeversion_render_queue";
-- Create index "creativeversion_render_queue" to table: "creative_versions"
CREATE INDEX "creativeversion_render_queue" ON "creative_versions" ("created_at") WHERE ((status)::text = ANY ((ARRAY['queued'::character varying, 'rendering'::character varying])::text[]));
-- Create index "knowledgebaseentry_waiting_embed" to table: "knowledge_base_entries"
CREATE INDEX "knowledgebaseentry_waiting_embed" ON "knowledge_base_entries" ("updated_at") WHERE (embedded = false);
-- Create "publish_jobs" table
CREATE TABLE "publish_jobs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "asset_id" uuid NOT NULL, "connected_account_id" uuid NOT NULL, "platform" character varying NOT NULL, "kind" character varying NOT NULL DEFAULT 'organic', "caption" text NULL, "scheduled_at" timestamptz NOT NULL, "status" character varying NOT NULL DEFAULT 'pending', "dedupe_key" character varying NOT NULL, "external_id" character varying NULL, "permalink" character varying NULL, "error" text NULL, "attempts" bigint NOT NULL DEFAULT 0, "created_by" uuid NULL, "metadata" jsonb NULL, "published_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "publish_jobs_dedupe_key_key" to table: "publish_jobs"
CREATE UNIQUE INDEX "publish_jobs_dedupe_key_key" ON "publish_jobs" ("dedupe_key");
-- Create index "publishjob_status_scheduled_at" to table: "publish_jobs"
CREATE INDEX "publishjob_status_scheduled_at" ON "publish_jobs" ("status", "scheduled_at") WHERE ((status)::text = ANY ((ARRAY['pending'::character varying, 'uploading'::character varying, 'processing'::character varying])::text[]));
-- Create index "publishjob_tenant_id_asset_id" to table: "publish_jobs"
CREATE INDEX "publishjob_tenant_id_asset_id" ON "publish_jobs" ("tenant_id", "asset_id");
-- Create index "publishjob_tenant_id_scheduled_at_id" to table: "publish_jobs"
CREATE INDEX "publishjob_tenant_id_scheduled_at_id" ON "publish_jobs" ("tenant_id", "scheduled_at", "id");
