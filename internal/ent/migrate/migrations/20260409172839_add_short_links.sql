-- Create "short_links" table
CREATE TABLE "short_links" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "code" character varying NOT NULL, "target_url" text NOT NULL, "link_type" character varying NOT NULL DEFAULT 'product', "title" character varying NULL, "campaign_id" uuid NULL, "click_count" bigint NOT NULL DEFAULT 0, "last_clicked_at" timestamptz NULL, "expires_at" timestamptz NULL, "is_active" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "short_links_code_key" to table: "short_links"
CREATE UNIQUE INDEX "short_links_code_key" ON "short_links" ("code");
-- Create index "shortlink_code" to table: "short_links"
CREATE UNIQUE INDEX "shortlink_code" ON "short_links" ("code");
-- Create index "shortlink_tenant_id_campaign_id" to table: "short_links"
CREATE INDEX "shortlink_tenant_id_campaign_id" ON "short_links" ("tenant_id", "campaign_id");
-- Create index "shortlink_tenant_id_created_at" to table: "short_links"
CREATE INDEX "shortlink_tenant_id_created_at" ON "short_links" ("tenant_id", "created_at");
-- Create index "shortlink_tenant_id_link_type" to table: "short_links"
CREATE INDEX "shortlink_tenant_id_link_type" ON "short_links" ("tenant_id", "link_type");
