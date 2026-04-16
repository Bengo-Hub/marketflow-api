// cmd/embed-backfill — one-shot CLI that generates pgvector embeddings for
// every KnowledgeBaseEntry where `embedded=false`. Use after a fresh seed
// (or when Ollama has been upgraded) to populate knowledge_base_embeddings
// so the RAG pipeline's searchPgVector call returns results.
//
// Usage:
//   POSTGRES_URL=... OLLAMA_URL=http://ollama.shared-infra.svc.cluster.local:11434 \
//     go run ./cmd/embed-backfill [-only-unembedded=true] [-tenant-id UUID]
//
// Idempotent: each chunk is UPSERT on (entry_id, chunk_index).
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	onlyUnembedded := flag.Bool("only-unembedded", true, "Skip entries that already have embedded=true")
	tenantFilter := flag.String("tenant-id", "", "Optional tenant UUID to limit the backfill")
	scopeFilter := flag.String("scope", "", "Optional scope filter: platform | tenant")
	model := flag.String("model", "nomic-embed-text", "Ollama embedding model")
	maxChunkChars := flag.Int("max-chunk", 900, "Max characters per chunk")
	flag.Parse()

	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		log.Fatal("POSTGRES_URL required")
	}
	ollamaURL := os.Getenv("OLLAMA_URL")
	if ollamaURL == "" {
		ollamaURL = "http://ollama.shared-infra.svc.cluster.local:11434"
	}

	raw, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer raw.Close()
	ctx := context.Background()
	if err := raw.PingContext(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}
	drv := entsql.OpenDB(dialect.Postgres, raw)
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	q := client.KnowledgeBaseEntry.Query()
	if *onlyUnembedded {
		q = q.Where(knowledgebaseentry.EmbeddedEQ(false))
	}
	if *tenantFilter != "" {
		tid, err := uuid.Parse(*tenantFilter)
		if err != nil {
			log.Fatalf("invalid tenant-id: %v", err)
		}
		q = q.Where(knowledgebaseentry.TenantIDEQ(tid))
	}
	if *scopeFilter != "" {
		q = q.Where(knowledgebaseentry.ScopeEQ(*scopeFilter))
	}
	entries, err := q.All(ctx)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	log.Printf("Embedding %d entries (model=%s)…", len(entries), *model)

	total, failed := 0, 0
	for i, e := range entries {
		chunks := chunkText(e.Content, *maxChunkChars)
		embedded := 0
		for idx, chunk := range chunks {
			vec, err := ollamaEmbed(ctx, ollamaURL, *model, chunk)
			if err != nil {
				failed++
				log.Printf("  [%d/%d] %q chunk %d: %v", i+1, len(entries), e.Title, idx, err)
				continue
			}
			if err := upsertEmbedding(ctx, raw, e.TenantID, e.ID, idx, chunk, e.Category, e.Title, vec); err != nil {
				failed++
				log.Printf("  [%d/%d] %q chunk %d upsert: %v", i+1, len(entries), e.Title, idx, err)
				continue
			}
			embedded++
			total++
		}
		if embedded > 0 {
			_, _ = client.KnowledgeBaseEntry.UpdateOneID(e.ID).
				SetEmbedded(true).
				SetChunkCount(embedded).
				Save(ctx)
		}
		if (i+1)%10 == 0 {
			log.Printf("  progress: %d/%d entries", i+1, len(entries))
		}
	}
	log.Printf("Done. %d chunks upserted, %d failures.", total, failed)
}

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

func ollamaEmbed(ctx context.Context, baseURL, model, text string) ([]float32, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "prompt": text})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/api/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	if len(r.Embedding) == 0 {
		return nil, fmt.Errorf("empty embedding")
	}
	return r.Embedding, nil
}

func upsertEmbedding(ctx context.Context, raw *sql.DB, tenantID, entryID uuid.UUID, chunkIdx int, chunkText, category, title string, vec []float32) error {
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
	return err
}
