-- Create "audit_logs" table
CREATE TABLE "audit_logs" ("id" uuid NOT NULL, "tenant_id" uuid NULL, "scope" character varying NOT NULL DEFAULT 'tenant', "event" character varying NOT NULL, "actor" character varying NULL, "subject_type" character varying NULL, "subject_id" uuid NULL, "payload" jsonb NULL, "occurred_at" timestamptz NOT NULL, "created_at" timestamptz NOT NULL, PRIMARY KEY ("id"));
-- Create index "auditlog_occurred_at" to table: "audit_logs"
CREATE INDEX "auditlog_occurred_at" ON "audit_logs" ("occurred_at");
-- Create index "auditlog_scope_event" to table: "audit_logs"
CREATE INDEX "auditlog_scope_event" ON "audit_logs" ("scope", "event");
-- Create index "auditlog_tenant_id_event" to table: "audit_logs"
CREATE INDEX "auditlog_tenant_id_event" ON "audit_logs" ("tenant_id", "event");
