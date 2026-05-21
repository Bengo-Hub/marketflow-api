-- Modify "leads" table
ALTER TABLE "leads" ADD COLUMN "owner_user_id" uuid NULL, ADD COLUMN "ai_score_breakdown" jsonb NULL, ADD COLUMN "enrichment_data" jsonb NULL;
-- Create "accounts" table
CREATE TABLE "accounts" ("id" uuid NOT NULL, "name" character varying NOT NULL, "industry" character varying NULL, "website" character varying NULL, "phone" character varying NULL, "address" jsonb NULL, "employee_count" bigint NULL, "annual_revenue" double precision NULL, "owner_user_id" uuid NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "accounts_tenants_accounts" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "account_owner_user_id" to table: "accounts"
CREATE INDEX "account_owner_user_id" ON "accounts" ("owner_user_id");
-- Create index "account_tenant_id_industry" to table: "accounts"
CREATE INDEX "account_tenant_id_industry" ON "accounts" ("tenant_id", "industry");
-- Create index "account_tenant_id_name" to table: "accounts"
CREATE INDEX "account_tenant_id_name" ON "accounts" ("tenant_id", "name");
-- Create "activities" table
CREATE TABLE "activities" ("id" uuid NOT NULL, "entity_type" character varying NOT NULL, "entity_id" uuid NOT NULL, "activity_type" character varying NOT NULL, "direction" character varying NULL, "subject" character varying NULL, "body" character varying NULL, "outcome" character varying NULL, "duration_minutes" bigint NULL, "performed_by" uuid NULL, "source" character varying NULL, "external_ref" character varying NULL, "performed_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "activities_tenants_activities" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "activity_external_ref" to table: "activities"
CREATE INDEX "activity_external_ref" ON "activities" ("external_ref");
-- Create index "activity_performed_at" to table: "activities"
CREATE INDEX "activity_performed_at" ON "activities" ("performed_at");
-- Create index "activity_tenant_id_activity_type" to table: "activities"
CREATE INDEX "activity_tenant_id_activity_type" ON "activities" ("tenant_id", "activity_type");
-- Create index "activity_tenant_id_entity_type_entity_id" to table: "activities"
CREATE INDEX "activity_tenant_id_entity_type_entity_id" ON "activities" ("tenant_id", "entity_type", "entity_id");
-- Create index "activity_tenant_id_entity_type_entity_id_performed_at" to table: "activities"
CREATE INDEX "activity_tenant_id_entity_type_entity_id_performed_at" ON "activities" ("tenant_id", "entity_type", "entity_id", "performed_at");
-- Create "custom_agents" table
CREATE TABLE "custom_agents" ("id" uuid NOT NULL, "name" character varying NOT NULL, "description" character varying NULL, "trigger_type" character varying NOT NULL DEFAULT 'manual', "trigger_config" jsonb NULL, "system_prompt" character varying NULL, "allowed_tools" jsonb NULL, "max_iterations" bigint NOT NULL DEFAULT 5, "is_active" boolean NOT NULL DEFAULT true, "run_count" bigint NOT NULL DEFAULT 0, "last_run_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "custom_agents_tenants_custom_agents" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "customagent_tenant_id_is_active" to table: "custom_agents"
CREATE INDEX "customagent_tenant_id_is_active" ON "custom_agents" ("tenant_id", "is_active");
-- Create index "customagent_tenant_id_trigger_type" to table: "custom_agents"
CREATE INDEX "customagent_tenant_id_trigger_type" ON "custom_agents" ("tenant_id", "trigger_type");
-- Create "agent_runs" table
CREATE TABLE "agent_runs" ("id" uuid NOT NULL, "triggered_by" character varying NOT NULL, "trigger_payload" jsonb NULL, "status" character varying NOT NULL DEFAULT 'running', "trace" jsonb NULL, "total_tokens" bigint NOT NULL DEFAULT 0, "tool_calls_n" bigint NOT NULL DEFAULT 0, "started_at" timestamptz NOT NULL, "finished_at" timestamptz NULL, "error_message" character varying NULL, "custom_agent_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "agent_runs_custom_agents_runs" FOREIGN KEY ("custom_agent_id") REFERENCES "custom_agents" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "agent_runs_tenants_agent_runs" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "agentrun_started_at" to table: "agent_runs"
CREATE INDEX "agentrun_started_at" ON "agent_runs" ("started_at");
-- Create index "agentrun_tenant_id_custom_agent_id" to table: "agent_runs"
CREATE INDEX "agentrun_tenant_id_custom_agent_id" ON "agent_runs" ("tenant_id", "custom_agent_id");
-- Create index "agentrun_tenant_id_status" to table: "agent_runs"
CREATE INDEX "agentrun_tenant_id_status" ON "agent_runs" ("tenant_id", "status");
-- Create "behavioral_triggers" table
CREATE TABLE "behavioral_triggers" ("id" uuid NOT NULL, "name" character varying NOT NULL, "event_type" character varying NOT NULL, "conditions" jsonb NULL, "action" jsonb NOT NULL, "is_active" boolean NOT NULL DEFAULT true, "fire_count" bigint NOT NULL DEFAULT 0, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "behavioral_triggers_tenants_behavioral_triggers" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "behavioraltrigger_tenant_id_event_type_is_active" to table: "behavioral_triggers"
CREATE INDEX "behavioraltrigger_tenant_id_event_type_is_active" ON "behavioral_triggers" ("tenant_id", "event_type", "is_active");
-- Create index "behavioraltrigger_tenant_id_is_active" to table: "behavioral_triggers"
CREATE INDEX "behavioraltrigger_tenant_id_is_active" ON "behavioral_triggers" ("tenant_id", "is_active");
-- Modify "contacts" table
ALTER TABLE "contacts" ADD COLUMN "owner_user_id" uuid NULL, ADD COLUMN "lifecycle_stage" character varying NOT NULL DEFAULT 'lead', ADD COLUMN "enrichment_data" jsonb NULL, ADD COLUMN "account_id" uuid NULL, ADD CONSTRAINT "contacts_accounts_contacts" FOREIGN KEY ("account_id") REFERENCES "accounts" ("id") ON UPDATE NO ACTION ON DELETE SET NULL;
-- Create index "contact_account_id" to table: "contacts"
CREATE INDEX "contact_account_id" ON "contacts" ("account_id");
-- Create index "contact_tenant_id_lifecycle_stage" to table: "contacts"
CREATE INDEX "contact_tenant_id_lifecycle_stage" ON "contacts" ("tenant_id", "lifecycle_stage");
-- Create index "contact_tenant_id_owner_user_id" to table: "contacts"
CREATE INDEX "contact_tenant_id_owner_user_id" ON "contacts" ("tenant_id", "owner_user_id");
-- Create "custom_field_defs" table
CREATE TABLE "custom_field_defs" ("id" uuid NOT NULL, "entity_type" character varying NOT NULL, "field_key" character varying NOT NULL, "field_label" character varying NOT NULL, "field_type" character varying NOT NULL DEFAULT 'text', "options" jsonb NULL, "is_required" boolean NOT NULL DEFAULT false, "sort_order" bigint NOT NULL DEFAULT 0, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "custom_field_defs_tenants_custom_field_defs" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "customfielddef_tenant_id_entity_type" to table: "custom_field_defs"
CREATE INDEX "customfielddef_tenant_id_entity_type" ON "custom_field_defs" ("tenant_id", "entity_type");
-- Create index "customfielddef_tenant_id_entity_type_field_key" to table: "custom_field_defs"
CREATE UNIQUE INDEX "customfielddef_tenant_id_entity_type_field_key" ON "custom_field_defs" ("tenant_id", "entity_type", "field_key");
-- Create "custom_field_values" table
CREATE TABLE "custom_field_values" ("id" uuid NOT NULL, "entity_type" character varying NOT NULL, "entity_id" uuid NOT NULL, "value" character varying NULL, "updated_at" timestamptz NOT NULL, "field_def_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "custom_field_values_custom_field_defs_values" FOREIGN KEY ("field_def_id") REFERENCES "custom_field_defs" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "custom_field_values_tenants_custom_field_values" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "customfieldvalue_entity_type_entity_id_field_def_id" to table: "custom_field_values"
CREATE UNIQUE INDEX "customfieldvalue_entity_type_entity_id_field_def_id" ON "custom_field_values" ("entity_type", "entity_id", "field_def_id");
-- Create index "customfieldvalue_field_def_id" to table: "custom_field_values"
CREATE INDEX "customfieldvalue_field_def_id" ON "custom_field_values" ("field_def_id");
-- Create index "customfieldvalue_tenant_id_entity_type_entity_id" to table: "custom_field_values"
CREATE INDEX "customfieldvalue_tenant_id_entity_type_entity_id" ON "custom_field_values" ("tenant_id", "entity_type", "entity_id");
-- Create "pipelines" table
CREATE TABLE "pipelines" ("id" uuid NOT NULL, "name" character varying NOT NULL, "is_default" boolean NOT NULL DEFAULT false, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "pipelines_tenants_pipelines" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "pipeline_tenant_id" to table: "pipelines"
CREATE INDEX "pipeline_tenant_id" ON "pipelines" ("tenant_id");
-- Create index "pipeline_tenant_id_is_default" to table: "pipelines"
CREATE INDEX "pipeline_tenant_id_is_default" ON "pipelines" ("tenant_id", "is_default");
-- Create "pipeline_stages" table
CREATE TABLE "pipeline_stages" ("id" uuid NOT NULL, "tenant_id" uuid NOT NULL, "name" character varying NOT NULL, "stage_order" bigint NOT NULL DEFAULT 0, "win_probability" double precision NOT NULL DEFAULT 0, "is_won_stage" boolean NOT NULL DEFAULT false, "is_lost_stage" boolean NOT NULL DEFAULT false, "color" character varying NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "pipeline_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "pipeline_stages_pipelines_stages" FOREIGN KEY ("pipeline_id") REFERENCES "pipelines" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "pipelinestage_pipeline_id_stage_order" to table: "pipeline_stages"
CREATE INDEX "pipelinestage_pipeline_id_stage_order" ON "pipeline_stages" ("pipeline_id", "stage_order");
-- Create index "pipelinestage_tenant_id" to table: "pipeline_stages"
CREATE INDEX "pipelinestage_tenant_id" ON "pipeline_stages" ("tenant_id");
-- Create "deals" table
CREATE TABLE "deals" ("id" uuid NOT NULL, "title" character varying NOT NULL, "lead_id" uuid NULL, "value" double precision NULL, "currency" character varying NOT NULL DEFAULT 'USD', "probability" double precision NULL, "expected_close_date" timestamptz NULL, "status" character varying NOT NULL DEFAULT 'open', "lost_reason" character varying NULL, "owner_user_id" uuid NULL, "metadata" jsonb NULL, "won_at" timestamptz NULL, "lost_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "account_id" uuid NULL, "contact_id" uuid NULL, "pipeline_id" uuid NOT NULL, "stage_id" uuid NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "deals_accounts_deals" FOREIGN KEY ("account_id") REFERENCES "accounts" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "deals_contacts_deals" FOREIGN KEY ("contact_id") REFERENCES "contacts" ("id") ON UPDATE NO ACTION ON DELETE SET NULL, CONSTRAINT "deals_pipeline_stages_deals" FOREIGN KEY ("stage_id") REFERENCES "pipeline_stages" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "deals_pipelines_deals" FOREIGN KEY ("pipeline_id") REFERENCES "pipelines" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION, CONSTRAINT "deals_tenants_deals" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "deal_tenant_id_contact_id" to table: "deals"
CREATE INDEX "deal_tenant_id_contact_id" ON "deals" ("tenant_id", "contact_id");
-- Create index "deal_tenant_id_created_at" to table: "deals"
CREATE INDEX "deal_tenant_id_created_at" ON "deals" ("tenant_id", "created_at");
-- Create index "deal_tenant_id_expected_close_date" to table: "deals"
CREATE INDEX "deal_tenant_id_expected_close_date" ON "deals" ("tenant_id", "expected_close_date");
-- Create index "deal_tenant_id_owner_user_id" to table: "deals"
CREATE INDEX "deal_tenant_id_owner_user_id" ON "deals" ("tenant_id", "owner_user_id");
-- Create index "deal_tenant_id_pipeline_id" to table: "deals"
CREATE INDEX "deal_tenant_id_pipeline_id" ON "deals" ("tenant_id", "pipeline_id");
-- Create index "deal_tenant_id_stage_id" to table: "deals"
CREATE INDEX "deal_tenant_id_stage_id" ON "deals" ("tenant_id", "stage_id");
-- Create index "deal_tenant_id_status" to table: "deals"
CREATE INDEX "deal_tenant_id_status" ON "deals" ("tenant_id", "status");
-- Create "lead_scoring_rules" table
CREATE TABLE "lead_scoring_rules" ("id" uuid NOT NULL, "name" character varying NOT NULL, "conditions" jsonb NOT NULL, "score_delta" bigint NOT NULL, "is_active" boolean NOT NULL DEFAULT true, "sort_order" bigint NOT NULL DEFAULT 0, "apply_mode" character varying NOT NULL DEFAULT 'cumulative', "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "lead_scoring_rules_tenants_lead_scoring_rules" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "leadscoringrule_tenant_id_is_active" to table: "lead_scoring_rules"
CREATE INDEX "leadscoringrule_tenant_id_is_active" ON "lead_scoring_rules" ("tenant_id", "is_active");
-- Create index "leadscoringrule_tenant_id_sort_order" to table: "lead_scoring_rules"
CREATE INDEX "leadscoringrule_tenant_id_sort_order" ON "lead_scoring_rules" ("tenant_id", "sort_order");
-- Create "segments" table
CREATE TABLE "segments" ("id" uuid NOT NULL, "name" character varying NOT NULL, "entity_type" character varying NOT NULL DEFAULT 'contact', "filter_rules" jsonb NOT NULL, "is_dynamic" boolean NOT NULL DEFAULT true, "member_count" bigint NOT NULL DEFAULT 0, "last_calculated_at" timestamptz NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "segments_tenants_segments" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "segment_tenant_id_entity_type" to table: "segments"
CREATE INDEX "segment_tenant_id_entity_type" ON "segments" ("tenant_id", "entity_type");
-- Create index "segment_tenant_id_is_dynamic" to table: "segments"
CREATE INDEX "segment_tenant_id_is_dynamic" ON "segments" ("tenant_id", "is_dynamic");
-- Create "tasks" table
CREATE TABLE "tasks" ("id" uuid NOT NULL, "entity_type" character varying NOT NULL, "entity_id" uuid NOT NULL, "title" character varying NOT NULL, "description" character varying NULL, "due_at" timestamptz NULL, "priority" character varying NOT NULL DEFAULT 'normal', "status" character varying NOT NULL DEFAULT 'open', "assigned_to_user_id" uuid NULL, "completed_at" timestamptz NULL, "created_by" uuid NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "tasks_tenants_tasks" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "task_tenant_id_assigned_to_user_id" to table: "tasks"
CREATE INDEX "task_tenant_id_assigned_to_user_id" ON "tasks" ("tenant_id", "assigned_to_user_id");
-- Create index "task_tenant_id_due_at" to table: "tasks"
CREATE INDEX "task_tenant_id_due_at" ON "tasks" ("tenant_id", "due_at");
-- Create index "task_tenant_id_entity_type_entity_id" to table: "tasks"
CREATE INDEX "task_tenant_id_entity_type_entity_id" ON "tasks" ("tenant_id", "entity_type", "entity_id");
-- Create index "task_tenant_id_status" to table: "tasks"
CREATE INDEX "task_tenant_id_status" ON "tasks" ("tenant_id", "status");
-- Create index "task_tenant_id_status_due_at" to table: "tasks"
CREATE INDEX "task_tenant_id_status_due_at" ON "tasks" ("tenant_id", "status", "due_at");
