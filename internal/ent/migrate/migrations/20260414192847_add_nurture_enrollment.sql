-- Create "nurture_enrollments" table
CREATE TABLE "nurture_enrollments" ("id" uuid NOT NULL, "sequence_id" uuid NOT NULL, "lead_id" uuid NULL, "contact_id" uuid NULL, "current_step" bigint NOT NULL DEFAULT 0, "status" character varying NOT NULL DEFAULT 'active', "next_step_at" timestamptz NULL, "last_step_at" timestamptz NULL, "started_at" timestamptz NOT NULL, "completed_at" timestamptz NULL, "exit_reason" character varying NULL, "metadata" jsonb NULL, "created_at" timestamptz NOT NULL, "updated_at" timestamptz NOT NULL, "tenant_id" uuid NOT NULL, PRIMARY KEY ("id"), CONSTRAINT "nurture_enrollments_tenants_nurture_enrollments" FOREIGN KEY ("tenant_id") REFERENCES "tenants" ("id") ON UPDATE NO ACTION ON DELETE NO ACTION);
-- Create index "nurtureenrollment_status_next_step_at" to table: "nurture_enrollments"
CREATE INDEX "nurtureenrollment_status_next_step_at" ON "nurture_enrollments" ("status", "next_step_at");
-- Create index "nurtureenrollment_tenant_id_lead_id_sequence_id" to table: "nurture_enrollments"
CREATE INDEX "nurtureenrollment_tenant_id_lead_id_sequence_id" ON "nurture_enrollments" ("tenant_id", "lead_id", "sequence_id");
-- Create index "nurtureenrollment_tenant_id_status" to table: "nurture_enrollments"
CREATE INDEX "nurtureenrollment_tenant_id_status" ON "nurture_enrollments" ("tenant_id", "status");
