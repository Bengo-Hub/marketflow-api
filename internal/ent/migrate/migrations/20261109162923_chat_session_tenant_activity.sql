-- Create index "chatsession_tenant_id_last_activity_at" to table: "chat_sessions"
CREATE INDEX "chatsession_tenant_id_last_activity_at" ON "chat_sessions" ("tenant_id", "last_activity_at");
