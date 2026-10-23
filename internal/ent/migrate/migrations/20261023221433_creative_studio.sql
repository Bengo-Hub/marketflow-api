-- Create "creative_briefs" table
CREATE TABLE "creative_briefs" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "title" character varying NOT NULL, "offering" jsonb NULL, "objective" character varying NOT NULL DEFAULT 'sales', "audience" character varying NULL, "language" character varying NOT NULL DEFAULT 'en', "status" character varying NOT NULL DEFAULT 'active', "created_by" uuid NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "creativebrief_tenant_id_status_created_at" to table: "creative_briefs"
CREATE INDEX "creativebrief_tenant_id_status_created_at" ON "creative_briefs" ("tenant_id", "status", "created_at");
-- Create "creative_versions" table
CREATE TABLE "creative_versions" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "parent_version_id" uuid NULL, "version_no" bigint NOT NULL, "kind" character varying NOT NULL DEFAULT 'poster', "tier" character varying NOT NULL DEFAULT 'standard', "format" character varying NOT NULL, "copy" jsonb NULL, "status" character varying NOT NULL DEFAULT 'queued', "credits" bigint NOT NULL DEFAULT 0, "error" character varying NULL, "approved_by" uuid NULL, "approved_at" timestamptz NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "brief_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "creative_versions_creative_briefs_versions" FOREIGN KEY ("brief_id") REFERENCES "creative_briefs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "creativeversion_brief_id_version_no" to table: "creative_versions"
CREATE UNIQUE INDEX "creativeversion_brief_id_version_no" ON "creative_versions" ("brief_id", "version_no");
-- Create index "creativeversion_render_queue" to table: "creative_versions"
CREATE INDEX "creativeversion_render_queue" ON "creative_versions" ("created_at") WHERE ((status)::text = ANY ((ARRAY['queued'::character varying, 'rendering'::character varying])::text[]));
-- Create index "creativeversion_tenant_id_status_created_at" to table: "creative_versions"
CREATE INDEX "creativeversion_tenant_id_status_created_at" ON "creative_versions" ("tenant_id", "status", "created_at");
-- Create "creative_assets" table
CREATE TABLE "creative_assets" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "kind" character varying NOT NULL DEFAULT 'image', "storage_key" character varying NOT NULL, "url" character varying NOT NULL, "bytes" bigint NOT NULL DEFAULT 0, "width" bigint NOT NULL DEFAULT 0, "height" bigint NOT NULL DEFAULT 0, "duration_ms" bigint NOT NULL DEFAULT 0, "sha256" character varying NULL, "expires_at" timestamptz NULL, "purged_at" timestamptz NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "version_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "creative_assets_creative_versions_assets" FOREIGN KEY ("version_id") REFERENCES "creative_versions" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "creativeasset_expires_at" to table: "creative_assets"
CREATE INDEX "creativeasset_expires_at" ON "creative_assets" ("expires_at") WHERE ((purged_at IS NULL) AND (expires_at IS NOT NULL));
-- Create index "creativeasset_tenant_id_version_id" to table: "creative_assets"
CREATE INDEX "creativeasset_tenant_id_version_id" ON "creative_assets" ("tenant_id", "version_id");
