-- Create "outbox_events" table
CREATE TABLE "outbox_events" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "aggregate_type" character varying NOT NULL, "aggregate_id" uuid NOT NULL, "event_type" character varying NOT NULL, "payload" bytea NOT NULL, "status" character varying NOT NULL DEFAULT 'PENDING', "attempts" bigint NOT NULL DEFAULT 0, "last_attempt_at" timestamptz NULL, "published_at" timestamptz NULL, "error_message" character varying NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "outboxevent_status_created_at" to table: "outbox_events"
CREATE INDEX "outboxevent_status_created_at" ON "outbox_events" ("status", "created_at");
-- Create index "outboxevent_tenant_id_aggregate_type_aggregate_id" to table: "outbox_events"
CREATE INDEX "outboxevent_tenant_id_aggregate_type_aggregate_id" ON "outbox_events" ("tenant_id", "aggregate_type", "aggregate_id");
-- Create "provider_settings" table
CREATE TABLE "provider_settings" ("id" uuid NOT NULL, "tenant_id" character varying NOT NULL, "provider_type" character varying NOT NULL, "provider_name" character varying NOT NULL, "environment" character varying NOT NULL DEFAULT 'production', "key" character varying NOT NULL, "value" text NULL, "description" text NULL, "is_encrypted" boolean NOT NULL DEFAULT false, "is_secret" boolean NOT NULL DEFAULT false, "is_platform" boolean NOT NULL DEFAULT false, "is_platform_managed" boolean NOT NULL DEFAULT false, "is_active" boolean NOT NULL DEFAULT true, "status" character varying NOT NULL DEFAULT 'active', "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" to table: "provider_settings"
CREATE INDEX "providersetting_tenant_id_envi_4174d78779cdd3eabfc186faa75339aa" ON "provider_settings" ("tenant_id", "environment", "provider_type", "provider_name", "key");
-- Create index "providersetting_tenant_id_environment_provider_type" to table: "provider_settings"
CREATE INDEX "providersetting_tenant_id_environment_provider_type" ON "provider_settings" ("tenant_id", "environment", "provider_type");
-- Create index "providersetting_tenant_id_provider_type_is_active" to table: "provider_settings"
CREATE INDEX "providersetting_tenant_id_provider_type_is_active" ON "provider_settings" ("tenant_id", "provider_type", "is_active");
-- Create "service_configs" table
CREATE TABLE "service_configs" ("id" uuid NOT NULL, "tenant_id" uuid NULL, "config_key" character varying NOT NULL, "config_value" text NOT NULL, "config_type" character varying NOT NULL DEFAULT 'string', "description" character varying NULL, "is_secret" boolean NOT NULL DEFAULT false, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "serviceconfig_config_key" to table: "service_configs"
CREATE INDEX "serviceconfig_config_key" ON "service_configs" ("config_key");
-- Create index "serviceconfig_tenant_id_config_key" to table: "service_configs"
CREATE UNIQUE INDEX "serviceconfig_tenant_id_config_key" ON "service_configs" ("tenant_id", "config_key");
-- Create "tenants" table
CREATE TABLE "tenants" ("id" uuid NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "fbo_distrib_id" character varying NULL, "whatsapp_number" character varying NULL, "flp_store_code" character varying NULL DEFAULT 'KEN', "flp_language" character varying NULL DEFAULT 'en', "flp_profile_uuid" character varying NULL, "business_type" character varying NOT NULL DEFAULT 'flp', "country" character varying NULL DEFAULT 'KE', "timezone" character varying NULL DEFAULT 'Africa/Nairobi', "meta_access_token" character varying NULL, "meta_ad_account_id" character varying NULL, "meta_page_id" character varying NULL, "subscription_plan" character varying NULL, "subscription_status" character varying NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "tenant_business_type" to table: "tenants"
CREATE INDEX "tenant_business_type" ON "tenants" ("business_type");
-- Create index "tenant_slug" to table: "tenants"
CREATE UNIQUE INDEX "tenant_slug" ON "tenants" ("slug");
-- Create index "tenant_status" to table: "tenants"
CREATE INDEX "tenant_status" ON "tenants" ("status");
-- Create index "tenants_slug_key" to table: "tenants"
CREATE UNIQUE INDEX "tenants_slug_key" ON "tenants" ("slug");
-- Create "campaigns" table
CREATE TABLE "campaigns" ("id" uuid NOT NULL, "name" character varying NOT NULL, "campaign_type" character varying NOT NULL DEFAULT 'lead_gen', "platform" character varying NOT NULL DEFAULT 'meta', "status" character varying NOT NULL DEFAULT 'draft', "meta_campaign_id" character varying NULL, "meta_adset_id" character varying NULL, "meta_ad_id" character varying NULL, "budget_daily" double precision NULL, "budget_total" double precision NULL, "currency" character varying NOT NULL DEFAULT 'USD', "target_audience" jsonb NULL, "funnel_id" uuid NULL, "start_date" timestamptz NULL, "end_date" timestamptz NULL, "metrics" jsonb NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "campaigns_tenants_campaigns" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "campaign_meta_campaign_id" to table: "campaigns"
CREATE INDEX "campaign_meta_campaign_id" ON "campaigns" ("meta_campaign_id");
-- Create index "campaign_tenant_id_campaign_type" to table: "campaigns"
CREATE INDEX "campaign_tenant_id_campaign_type" ON "campaigns" ("tenant_id", "campaign_type");
-- Create index "campaign_tenant_id_platform" to table: "campaigns"
CREATE INDEX "campaign_tenant_id_platform" ON "campaigns" ("tenant_id", "platform");
-- Create index "campaign_tenant_id_status" to table: "campaigns"
CREATE INDEX "campaign_tenant_id_status" ON "campaigns" ("tenant_id", "status");
-- Create "contacts" table
CREATE TABLE "contacts" ("id" uuid NOT NULL, "email" character varying NULL, "phone" character varying NULL, "first_name" character varying NULL, "last_name" character varying NULL, "contact_type" character varying NOT NULL DEFAULT 'customer', "lead_id" uuid NULL, "flp_customer_id" character varying NULL, "total_cc" double precision NOT NULL DEFAULT 0, "total_orders" bigint NOT NULL DEFAULT 0, "last_order_at" timestamptz NULL, "product_interests" jsonb NULL, "subscribed_email" boolean NOT NULL DEFAULT true, "subscribed_sms" boolean NOT NULL DEFAULT true, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "contacts_tenants_contacts" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "contact_tenant_id_contact_type" to table: "contacts"
CREATE INDEX "contact_tenant_id_contact_type" ON "contacts" ("tenant_id", "contact_type");
-- Create index "contact_tenant_id_email" to table: "contacts"
CREATE INDEX "contact_tenant_id_email" ON "contacts" ("tenant_id", "email");
-- Create index "contact_tenant_id_last_order_at" to table: "contacts"
CREATE INDEX "contact_tenant_id_last_order_at" ON "contacts" ("tenant_id", "last_order_at");
-- Create index "contact_tenant_id_phone" to table: "contacts"
CREATE INDEX "contact_tenant_id_phone" ON "contacts" ("tenant_id", "phone");
-- Create "funnels" table
CREATE TABLE "funnels" ("id" uuid NOT NULL, "name" character varying NOT NULL, "slug" character varying NOT NULL, "funnel_type" character varying NOT NULL DEFAULT 'product_inquiry', "status" character varying NOT NULL DEFAULT 'draft', "steps" jsonb NULL, "landing_page_html" text NULL, "landing_page_css" jsonb NULL, "total_views" bigint NOT NULL DEFAULT 0, "total_submissions" bigint NOT NULL DEFAULT 0, "conversion_rate" double precision NOT NULL DEFAULT 0, "metadata" jsonb NULL, "published_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "funnels_tenants_funnels" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "funnel_tenant_id_funnel_type" to table: "funnels"
CREATE INDEX "funnel_tenant_id_funnel_type" ON "funnels" ("tenant_id", "funnel_type");
-- Create index "funnel_tenant_id_slug" to table: "funnels"
CREATE UNIQUE INDEX "funnel_tenant_id_slug" ON "funnels" ("tenant_id", "slug");
-- Create index "funnel_tenant_id_status" to table: "funnels"
CREATE INDEX "funnel_tenant_id_status" ON "funnels" ("tenant_id", "status");
-- Create "leads" table
CREATE TABLE "leads" ("id" uuid NOT NULL, "email" character varying NULL, "phone" character varying NULL, "first_name" character varying NULL, "last_name" character varying NULL, "source" character varying NOT NULL DEFAULT 'organic', "utm_source" character varying NULL, "utm_medium" character varying NULL, "utm_campaign" character varying NULL, "status" character varying NOT NULL DEFAULT 'new', "lead_type" character varying NOT NULL DEFAULT 'customer', "score" bigint NOT NULL DEFAULT 0, "interest" character varying NULL, "qualification_answers" jsonb NULL, "metadata" jsonb NULL, "contacted_at" timestamptz NULL, "qualified_at" timestamptz NULL, "converted_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "campaign_id" uuid NULL, "funnel_id" uuid NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "leads_campaigns_leads" FOREIGN KEY ("campaign_id") REFERENCES "campaigns" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "leads_funnels_leads" FOREIGN KEY ("funnel_id") REFERENCES "funnels" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "leads_tenants_leads" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "lead_tenant_id_created_at" to table: "leads"
CREATE INDEX "lead_tenant_id_created_at" ON "leads" ("tenant_id", "created_at");
-- Create index "lead_tenant_id_email" to table: "leads"
CREATE INDEX "lead_tenant_id_email" ON "leads" ("tenant_id", "email");
-- Create index "lead_tenant_id_lead_type" to table: "leads"
CREATE INDEX "lead_tenant_id_lead_type" ON "leads" ("tenant_id", "lead_type");
-- Create index "lead_tenant_id_phone" to table: "leads"
CREATE INDEX "lead_tenant_id_phone" ON "leads" ("tenant_id", "phone");
-- Create index "lead_tenant_id_source" to table: "leads"
CREATE INDEX "lead_tenant_id_source" ON "leads" ("tenant_id", "source");
-- Create index "lead_tenant_id_status" to table: "leads"
CREATE INDEX "lead_tenant_id_status" ON "leads" ("tenant_id", "status");
-- Create "chat_sessions" table
CREATE TABLE "chat_sessions" ("id" uuid NOT NULL, "status" character varying NOT NULL DEFAULT 'active', "channel" character varying NOT NULL DEFAULT 'web_widget', "messages" jsonb NULL, "message_count" bigint NOT NULL DEFAULT 0, "ai_responses" bigint NOT NULL DEFAULT 0, "escalations" bigint NOT NULL DEFAULT 0, "avg_confidence" double precision NULL, "metadata" jsonb NULL, "started_at" timestamptz NOT NULL, "ended_at" timestamptz NULL, "contact_id" uuid NULL, "lead_id" uuid NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "chat_sessions_contacts_chat_sessions" FOREIGN KEY ("contact_id") REFERENCES "contacts" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "chat_sessions_leads_chat_sessions" FOREIGN KEY ("lead_id") REFERENCES "leads" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "chat_sessions_tenants_chat_sessions" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "chatsession_tenant_id_channel" to table: "chat_sessions"
CREATE INDEX "chatsession_tenant_id_channel" ON "chat_sessions" ("tenant_id", "channel");
-- Create index "chatsession_tenant_id_contact_id" to table: "chat_sessions"
CREATE INDEX "chatsession_tenant_id_contact_id" ON "chat_sessions" ("tenant_id", "contact_id");
-- Create index "chatsession_tenant_id_lead_id" to table: "chat_sessions"
CREATE INDEX "chatsession_tenant_id_lead_id" ON "chat_sessions" ("tenant_id", "lead_id");
-- Create index "chatsession_tenant_id_status" to table: "chat_sessions"
CREATE INDEX "chatsession_tenant_id_status" ON "chat_sessions" ("tenant_id", "status");
-- Create "knowledge_base_entries" table
CREATE TABLE "knowledge_base_entries" ("id" uuid NOT NULL, "category" character varying NOT NULL DEFAULT 'product', "title" character varying NOT NULL, "content" text NOT NULL, "product_code" character varying NULL, "product_price" double precision NULL, "product_cc" double precision NULL, "product_image_url" character varying NULL, "embedded" boolean NOT NULL DEFAULT false, "chunk_count" bigint NOT NULL DEFAULT 0, "tags" jsonb NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "knowledge_base_entries_tenants_knowledge_base_entries" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "knowledgebaseentry_tenant_id_category" to table: "knowledge_base_entries"
CREATE INDEX "knowledgebaseentry_tenant_id_category" ON "knowledge_base_entries" ("tenant_id", "category");
-- Create index "knowledgebaseentry_tenant_id_embedded" to table: "knowledge_base_entries"
CREATE INDEX "knowledgebaseentry_tenant_id_embedded" ON "knowledge_base_entries" ("tenant_id", "embedded");
-- Create index "knowledgebaseentry_tenant_id_product_code" to table: "knowledge_base_entries"
CREATE INDEX "knowledgebaseentry_tenant_id_product_code" ON "knowledge_base_entries" ("tenant_id", "product_code");
-- Create "nurture_sequences" table
CREATE TABLE "nurture_sequences" ("id" uuid NOT NULL, "name" character varying NOT NULL, "trigger_event" character varying NOT NULL, "lead_type_filter" character varying NULL, "active" boolean NOT NULL DEFAULT true, "steps" jsonb NOT NULL, "total_enrolled" bigint NOT NULL DEFAULT 0, "total_completed" bigint NOT NULL DEFAULT 0, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "nurture_sequences_tenants_nurture_sequences" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "nurturesequence_tenant_id_active" to table: "nurture_sequences"
CREATE INDEX "nurturesequence_tenant_id_active" ON "nurture_sequences" ("tenant_id", "active");
-- Create index "nurturesequence_tenant_id_trigger_event" to table: "nurture_sequences"
CREATE INDEX "nurturesequence_tenant_id_trigger_event" ON "nurture_sequences" ("tenant_id", "trigger_event");
-- Create "scheduled_meetings" table
CREATE TABLE "scheduled_meetings" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "cal_booking_id" character varying NULL, "meeting_url" character varying NULL, "scheduled_at" timestamptz NOT NULL, "duration_minutes" bigint NOT NULL DEFAULT 30, "meeting_type" character varying NOT NULL DEFAULT 'product_demo', "status" character varying NOT NULL DEFAULT 'scheduled', "notes" text NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "contact_id" uuid NULL, "lead_id" uuid NULL, PRIMARY KEY ("id"), CONSTRAINT "scheduled_meetings_contacts_scheduled_meetings" FOREIGN KEY ("contact_id") REFERENCES "contacts" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "scheduled_meetings_leads_scheduled_meetings" FOREIGN KEY ("lead_id") REFERENCES "leads" ("id") ON UPDATE NO ACTION ON DELETE SET NULL);
-- Create index "scheduledmeeting_cal_booking_id" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_cal_booking_id" ON "scheduled_meetings" ("cal_booking_id");
-- Create index "scheduledmeeting_tenant_id_meeting_type" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_tenant_id_meeting_type" ON "scheduled_meetings" ("tenant_id", "meeting_type");
-- Create index "scheduledmeeting_tenant_id_scheduled_at" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_tenant_id_scheduled_at" ON "scheduled_meetings" ("tenant_id", "scheduled_at");
-- Create index "scheduledmeeting_tenant_id_status" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_tenant_id_status" ON "scheduled_meetings" ("tenant_id", "status");
