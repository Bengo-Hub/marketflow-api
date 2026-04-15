package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// chunkText splits content on paragraph boundaries, grouping into chunks of
// ~maxChars each. Returns at least one chunk so trivial entries still embed.
func chunkText(content string, maxChars int) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return []string{""}
	}
	paragraphs := strings.Split(content, "\n\n")
	var out []string
	var buf strings.Builder
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if buf.Len()+len(p)+2 > maxChars && buf.Len() > 0 {
			out = append(out, strings.TrimSpace(buf.String()))
			buf.Reset()
		}
		if buf.Len() > 0 {
			buf.WriteString("\n\n")
		}
		buf.WriteString(p)
	}
	if buf.Len() > 0 {
		out = append(out, strings.TrimSpace(buf.String()))
	}
	if len(out) == 0 {
		out = append(out, content)
	}
	return out
}

// ollamaEmbed calls Ollama's /api/embeddings endpoint. Uses the configured
// shared-infra Ollama service; same model as the RAG pipeline.
func ollamaEmbed(ctx context.Context, baseURL, model, text string) ([]float32, error) {
	if baseURL == "" {
		return nil, fmt.Errorf("ollama URL not configured")
	}
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama call: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ollama %d: %s", resp.StatusCode, string(b))
	}
	var r struct {
		Embedding []float32 `json:"embedding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("ollama decode: %w", err)
	}
	if len(r.Embedding) == 0 {
		return nil, fmt.Errorf("ollama returned empty embedding")
	}
	return r.Embedding, nil
}

// upsertEmbedding writes a single (entry_id, chunk_index) row into
// knowledge_base_embeddings. Uses raw *sql.DB because the table has a pgvector
// column that Ent doesn't know about; see migrations/20260409183000_add_kb_embeddings.sql.
func upsertEmbedding(ctx context.Context, raw *sql.DB, tenantID, entryID uuid.UUID, chunkIdx int, chunkText, category, title string, vec []float32) error {
	if raw == nil {
		return fmt.Errorf("upsertEmbedding: no raw *sql.DB configured")
	}
	parts := make([]string, len(vec))
	for i, v := range vec {
		parts[i] = fmt.Sprintf("%f", v)
	}
	vectorStr := "[" + strings.Join(parts, ",") + "]"

	_, err := raw.ExecContext(ctx, `
		INSERT INTO knowledge_base_embeddings
			(id, entry_id, tenant_id, chunk_text, chunk_index, category, title, embedding, created_at)
		VALUES
			(gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7::vector, now())
		ON CONFLICT (entry_id, chunk_index) DO UPDATE SET
			chunk_text = EXCLUDED.chunk_text,
			category   = EXCLUDED.category,
			title      = EXCLUDED.title,
			embedding  = EXCLUDED.embedding
	`, entryID, tenantID, chunkText, chunkIdx, category, title, vectorStr)
	if err != nil {
		return fmt.Errorf("pgvector upsert: %w", err)
	}
	return nil
}
