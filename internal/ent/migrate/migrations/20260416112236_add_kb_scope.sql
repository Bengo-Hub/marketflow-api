-- Modify "knowledge_base_entries" table
ALTER TABLE "knowledge_base_entries" ADD COLUMN "scope" character varying NOT NULL DEFAULT 'tenant';
-- Create index "knowledgebaseentry_scope_category" to table: "knowledge_base_entries"
CREATE INDEX "knowledgebaseentry_scope_category" ON "knowledge_base_entries" ("scope", "category");
