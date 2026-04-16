-- Create "ad_metrics" table
CREATE TABLE "ad_metrics" ("id" uuid NOT NULL, "platform" character varying NOT NULL, "external_campaign_id" character varying NULL, "date" timestamptz NOT NULL, "impressions" bigint NOT NULL DEFAULT 0, "clicks" bigint NOT NULL DEFAULT 0, "leads" bigint NOT NULL DEFAULT 0, "conversions" bigint NOT NULL DEFAULT 0, "spend" double precision NOT NULL DEFAULT 0, "ctr" double precision NOT NULL DEFAULT 0, "cpl" double precision NOT NULL DEFAULT 0, "currency" character varying NOT NULL DEFAULT 'USD', "raw" jsonb NULL, "fetched_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, "campaign_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "ad_metrics_campaigns_ad_metrics" FOREIGN KEY ("campaign_id") REFERENCES "campaigns" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "ad_metrics_tenants_ad_metrics" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "admetrics_campaign_id_date" to table: "ad_metrics"
CREATE UNIQUE INDEX "admetrics_campaign_id_date" ON "ad_metrics" ("campaign_id", "date");
-- Create index "admetrics_tenant_id_date" to table: "ad_metrics"
CREATE INDEX "admetrics_tenant_id_date" ON "ad_metrics" ("tenant_id", "date");
-- Create index "admetrics_tenant_id_platform_date" to table: "ad_metrics"
CREATE INDEX "admetrics_tenant_id_platform_date" ON "ad_metrics" ("tenant_id", "platform", "date");
-- Create "nurture_delivery_events" table
CREATE TABLE "nurture_delivery_events" ("id" uuid NOT NULL, "step_index" bigint NOT NULL DEFAULT 0, "channel" character varying NOT NULL, "event_type" character varying NOT NULL, "provider_message_id" character varying NULL, "link_url" character varying NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "enrollment_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "nurture_delivery_events_nurture_enrollments_delivery_events" FOREIGN KEY ("enrollment_id") REFERENCES "nurture_enrollments" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "nurture_delivery_events_tenants_nurture_delivery_events" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "nurturedeliveryevent_enrollment_id_step_index" to table: "nurture_delivery_events"
CREATE INDEX "nurturedeliveryevent_enrollment_id_step_index" ON "nurture_delivery_events" ("enrollment_id", "step_index");
-- Create index "nurturedeliveryevent_provider_message_id" to table: "nurture_delivery_events"
CREATE INDEX "nurturedeliveryevent_provider_message_id" ON "nurture_delivery_events" ("provider_message_id");
-- Create index "nurturedeliveryevent_tenant_id_event_type_created_at" to table: "nurture_delivery_events"
CREATE INDEX "nurturedeliveryevent_tenant_id_event_type_created_at" ON "nurture_delivery_events" ("tenant_id", "event_type", "created_at");
-- Create "profile_events" table
CREATE TABLE "profile_events" ("id" uuid NOT NULL, "slug" character varying NOT NULL, "event_type" character varying NOT NULL, "referrer" character varying NULL, "user_agent" character varying NULL, "ip_hash" character varying NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "profile_events_tenants_profile_events" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "profileevent_slug_created_at" to table: "profile_events"
CREATE INDEX "profileevent_slug_created_at" ON "profile_events" ("slug", "created_at");
-- Create index "profileevent_tenant_id_event_type_created_at" to table: "profile_events"
CREATE INDEX "profileevent_tenant_id_event_type_created_at" ON "profile_events" ("tenant_id", "event_type", "created_at");
-- Create "testimonials" table
CREATE TABLE "testimonials" ("id" uuid NOT NULL, "author_name" character varying NOT NULL, "author_role" character varying NULL, "body" character varying NOT NULL, "avatar_url" character varying NULL, "rating" bigint NOT NULL DEFAULT 5, "display_order" bigint NOT NULL DEFAULT 0, "is_published" boolean NOT NULL DEFAULT true, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "testimonials_tenants_testimonials" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "testimonial_tenant_id_created_at" to table: "testimonials"
CREATE INDEX "testimonial_tenant_id_created_at" ON "testimonials" ("tenant_id", "created_at");
-- Create index "testimonial_tenant_id_is_published_display_order" to table: "testimonials"
CREATE INDEX "testimonial_tenant_id_is_published_display_order" ON "testimonials" ("tenant_id", "is_published", "display_order");
