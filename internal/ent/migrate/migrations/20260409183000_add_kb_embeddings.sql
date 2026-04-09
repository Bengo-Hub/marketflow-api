-- Create knowledge_base_embeddings table for pgvector RAG
-- This table stores vector embeddings for KB entry chunks, used by marketflow-ai for similarity search.
-- Uses pgvector's vector(768) type for nomic-embed-text embeddings.

CREATE TABLE IF NOT EXISTS "knowledge_base_embeddings" (
    "id" uuid NOT NULL DEFAULT gen_random_uuid(),
    "entry_id" uuid NOT NULL REFERENCES "knowledge_base_entries" ("id") ON DELETE CASCADE,
    "tenant_id" uuid NOT NULL,
    "chunk_text" text NOT NULL,
    "chunk_index" integer NOT NULL DEFAULT 0,
    "category" character varying NOT NULL DEFAULT 'product',
    "title" character varying NOT NULL DEFAULT '',
    "embedding" vector(768) NOT NULL,
    "created_at" timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY ("id")
);

-- Index for vector similarity search (IVFFlat for approximate nearest neighbor)
-- Use cosine distance operator (<=>)
CREATE INDEX IF NOT EXISTS "kb_embeddings_vector_idx"
    ON "knowledge_base_embeddings"
    USING ivfflat ("embedding" vector_cosine_ops)
    WITH (lists = 100);

-- Index for filtering by tenant before vector search
CREATE INDEX IF NOT EXISTS "kb_embeddings_tenant_id" ON "knowledge_base_embeddings" ("tenant_id");

-- Index for joining back to KB entries
CREATE INDEX IF NOT EXISTS "kb_embeddings_entry_id" ON "knowledge_base_embeddings" ("entry_id");

-- Composite index for tenant-scoped similarity search
CREATE INDEX IF NOT EXISTS "kb_embeddings_tenant_category" ON "knowledge_base_embeddings" ("tenant_id", "category");
