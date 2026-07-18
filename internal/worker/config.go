// Package worker hosts the marketflow background consumer (NATS JetStream).
// It is built as a separate binary (cmd/worker) but ships in the same Docker image
// as the API and shares the same config / Ent client.
package worker

import (
	"fmt"

	"github.com/caarlos0/env/v11"
)

// Config is the worker's runtime configuration.
// Intentionally distinct from marketflow-api's main Config because the worker
// has no HTTP API, no pgvector requirement, and a different port.
type Config struct {
	NATS          NATSConfig          `envPrefix:"NATS_"`
	HTTP          HTTPConfig          `envPrefix:"WORKER_HTTP_"`
	API           APIConfig           `envPrefix:"MF_WORKER_"`
	Notifications NotificationsConfig `envPrefix:"NOTIFICATIONS_"`
	Database      DatabaseConfig      `envPrefix:""`
	Scheduler     SchedulerConfig     `envPrefix:"SCHEDULER_"`
}

type DatabaseConfig struct {
	URL string `env:"POSTGRES_URL"`
}

type SchedulerConfig struct {
	IntervalSeconds int `env:"INTERVAL_SECONDS" envDefault:"60"`
	BatchSize       int `env:"BATCH_SIZE" envDefault:"50"`
	// Re-engagement cron
	ReengagementIntervalHours int `env:"REENGAGEMENT_INTERVAL_HOURS" envDefault:"24"`
	ReengagementDormantDays   int `env:"REENGAGEMENT_DORMANT_DAYS" envDefault:"60"`
}

type NATSConfig struct {
	URL string `env:"URL" envDefault:"nats://nats.messaging.svc.cluster.local:4222"`
	// Stream is the worker's OWN stream (marketflow-internal work signals). Stream
	// names are case-sensitive and prod's stream is lowercase "marketflow" — the old
	// "MARKETFLOW" default named a stream that never existed, so consumer
	// registration could never succeed.
	Stream string `env:"STREAM" envDefault:"marketflow"`
}

type HTTPConfig struct {
	Port int `env:"PORT" envDefault:"4002"`
}

type APIConfig struct {
	MarketFlowAPIURL   string `env:"API_URL" envDefault:"http://marketflow-api.marketflow.svc.cluster.local:4000"`
	MarketFlowAIURL    string `env:"AI_URL" envDefault:"http://marketflow-ai.marketflow.svc.cluster.local:4001"`
	TicketingAPIURL    string `env:"TICKETING_URL" envDefault:"http://ticketing-api.ticketing.svc.cluster.local:4009"`
	OllamaURL          string `env:"OLLAMA_URL" envDefault:"http://ollama.shared-infra.svc.cluster.local:11434"`
	EmbeddingModel     string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`
	InternalServiceKey string `env:"INTERNAL_SERVICE_KEY"`
}

// NotificationsConfig carries the platform notifications-api coordinates.
// Service-to-service auth uses X-API-Key (platform API key).
type NotificationsConfig struct {
	URL    string `env:"URL" envDefault:"http://notifications-api.notifications.svc.cluster.local:4000"`
	APIKey string `env:"API_KEY"`
	Token  string `env:"TOKEN"` // optional Bearer fallback
}

// LoadConfig parses env vars into a worker Config.
func LoadConfig() (*Config, error) {
	c := &Config{}
	if err := env.Parse(c); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}
	return c, nil
}
