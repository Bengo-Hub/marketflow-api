-- Modify "chat_sessions" table
ALTER TABLE "chat_sessions" ADD COLUMN "last_activity_at" timestamptz NULL;
-- Create index "chatsession_last_activity_at" to table: "chat_sessions"
CREATE INDEX "chatsession_last_activity_at" ON "chat_sessions" ("last_activity_at");
-- Create index "chatsession_started_at" to table: "chat_sessions"
CREATE INDEX "chatsession_started_at" ON "chat_sessions" ("started_at");
