-- Create index "activity_tenant_id_performed_at" to table: "activities"
CREATE INDEX "activity_tenant_id_performed_at" ON "activities" ("tenant_id", "performed_at");
