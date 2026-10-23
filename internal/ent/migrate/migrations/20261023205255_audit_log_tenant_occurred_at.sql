-- Create index "auditlog_tenant_id_occurred_at" to table: "audit_logs"
CREATE INDEX "auditlog_tenant_id_occurred_at" ON "audit_logs" ("tenant_id", "occurred_at");
