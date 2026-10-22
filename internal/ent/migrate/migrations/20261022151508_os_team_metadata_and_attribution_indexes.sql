-- Modify "custom_agents" table
ALTER TABLE "custom_agents" ADD COLUMN "metadata" jsonb NULL;
-- Create index "lead_tenant_id_campaign_id" to table: "leads"
CREATE INDEX "lead_tenant_id_campaign_id" ON "leads" ("tenant_id", "campaign_id");
-- Create index "scheduledmeeting_lead_id" to table: "scheduled_meetings"
CREATE INDEX "scheduledmeeting_lead_id" ON "scheduled_meetings" ("lead_id");
