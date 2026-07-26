package config

import (
	"fmt"
	"os"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config aggregates all runtime settings.
type Config struct {
	App           AppConfig           `envPrefix:""`
	HTTP          HTTPConfig          `envPrefix:"HTTP_"`
	Database      DatabaseConfig      `envPrefix:""`
	Redis         RedisConfig         `envPrefix:"REDIS_"`
	Security      SecurityConfig      `envPrefix:"MF_SECURITY_"`
	Events        EventsConfig        `envPrefix:"MF_EVENTS_"`
	Meta          MetaConfig          `envPrefix:"MF_META_"`
	TikTok        TikTokConfig        `envPrefix:"MF_TIKTOK_"`
	AI            AIConfig            `envPrefix:"MF_AI_"`
	Subscriptions SubscriptionsConfig `envPrefix:"MF_SUBSCRIPTIONS_"`
	CrossService  CrossServiceConfig  `envPrefix:"MF_CROSS_"`
	Backup        BackupConfig        `envPrefix:"BACKUP_"`
}

// BackupConfig controls the tenant-scoped backup scheduler + retention churn. Artifacts are
// written to a local directory (Dir) — typically a PVC.
type BackupConfig struct {
	Dir             string `env:"DIR" envDefault:"/app/backups/marketflow"`
	ScheduleEnabled bool   `env:"SCHEDULE_ENABLED" envDefault:"true"`
	ScheduleHour    int    `env:"SCHEDULE_HOUR" envDefault:"2"`
	RetentionDays   int    `env:"RETENTION_DAYS" envDefault:"4"`
}

type SubscriptionsConfig struct {
	URL    string `env:"URL" envDefault:"http://subscriptions-api.infra.svc.cluster.local:4000"`
	APIKey string `env:"API_KEY"`
}

type AppConfig struct {
	Environment string `env:"APP_ENV" envDefault:"development"`
	ServiceName string `env:"APP_NAME" envDefault:"marketflow-api"`
}

type HTTPConfig struct {
	Host              string        `env:"HOST" envDefault:"0.0.0.0"`
	Port              int           `env:"PORT" envDefault:"4000"`
	AllowedOrigins    string        `env:"ALLOWED_ORIGINS" envDefault:"http://localhost:3000"`
	ReadTimeout       time.Duration `env:"READ_TIMEOUT" envDefault:"5s"`
	WriteTimeout      time.Duration `env:"WRITE_TIMEOUT" envDefault:"10s"`
	IdleTimeout       time.Duration `env:"IDLE_TIMEOUT" envDefault:"120s"`
	ReadHeaderTimeout time.Duration `env:"READ_HEADER_TIMEOUT" envDefault:"5s"`
	ShutdownTimeout   time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"25s"`
}

type DatabaseConfig struct {
	URL             string        `env:"POSTGRES_URL"`
	MigrateURL      string        `env:"POSTGRES_MIGRATE_URL"` // direct PG for migrate/seed; bypasses PgBouncer transaction mode
	MaxOpenConns    int           `env:"POSTGRES_MAX_OPEN_CONNS" envDefault:"8"`
	MaxIdleConns    int           `env:"POSTGRES_MAX_IDLE_CONNS" envDefault:"3"`
	ConnMaxLifetime time.Duration `env:"POSTGRES_CONN_MAX_LIFETIME" envDefault:"15m"`
	RunMigrations   bool          `env:"POSTGRES_RUN_MIGRATIONS" envDefault:"true"`
}

type RedisConfig struct {
	Addr      string `env:"ADDR" envDefault:"127.0.0.1:6379"`
	Password  string `env:"PASSWORD"`
	DB        int    `env:"DB" envDefault:"0"`
	EnableTLS bool   `env:"ENABLE_TLS" envDefault:"false"`
	Namespace string `env:"NAMESPACE" envDefault:"marketflow"`
}

type SecurityConfig struct {
	JWKSUrl        string `env:"JWKS_URL" envDefault:"https://sso.codevertexafrica.com/api/v1/.well-known/jwks.json"`
	JWTIssuer      string `env:"JWT_ISSUER" envDefault:"https://sso.codevertexafrica.com"`
	JWTAudience    string `env:"JWT_AUDIENCE" envDefault:"codevertex"`
	RequireJWT     bool   `env:"REQUIRE_JWT" envDefault:"true"`
	EncryptionKey  string `env:"ENCRYPTION_KEY" comment:"32-byte base64-encoded key for AES-256-GCM encryption of provider secrets"`
	PlatformTenant string `env:"PLATFORM_TENANT" envDefault:"codevertex" comment:"Platform owner tenant slug for provider config hierarchy"`
	AuthServiceURL string `env:"AUTH_SERVICE_URL" envDefault:"https://sso.codevertexafrica.com" comment:"Used by shared-auth-client APIKeyValidator"`
}

type EventsConfig struct {
	NATSURL          string        `env:"NATS_URL" envDefault:"nats://127.0.0.1:4222"`
	Enabled          bool          `env:"ENABLED" envDefault:"false"`
	OutboxPollPeriod time.Duration `env:"OUTBOX_POLL_PERIOD" envDefault:"5s"`
	OutboxBatchSize  int           `env:"OUTBOX_BATCH_SIZE" envDefault:"100"`
}

// MetaConfig holds Facebook/Instagram Marketing API settings.
// Platform-level credentials shared across tenants; per-tenant link is
// Tenant.meta_page_id only.
type MetaConfig struct {
	AppID       string `env:"APP_ID"`
	AppSecret   string `env:"APP_SECRET"`
	VerifyToken string `env:"VERIFY_TOKEN"` // GET subscription verification
	// GraphAPIVersion is the version of the Graph API to use.
	GraphAPIVersion string `env:"GRAPH_API_VERSION" envDefault:"v21.0"`
}

// TikTokConfig holds TikTok Business API settings.
type TikTokConfig struct {
	WebhookSecret string `env:"WEBHOOK_SECRET"` // optional shared secret for webhook verification
}

// CrossServiceConfig holds internal URLs for S2S calls to sibling services.
type CrossServiceConfig struct {
	InternalServiceKey string `env:"INTERNAL_SERVICE_KEY"`
	OrderingURL        string `env:"ORDERING_URL" envDefault:"http://ordering-backend.ordering.svc.cluster.local:4000"`
	POSURL             string `env:"POS_URL" envDefault:"http://pos-api.pos.svc.cluster.local:4000"`
	TreasuryURL        string `env:"TREASURY_URL" envDefault:"http://treasury-api.finance.svc.cluster.local:4000"`
}

// AIConfig holds AI/LLM integration settings.
type AIConfig struct {
	OllamaURL       string `env:"OLLAMA_URL" envDefault:"http://ollama.marketflow.svc.cluster.local:11434"`
	OllamaModel     string `env:"OLLAMA_MODEL" envDefault:"llama3.1:8b"`
	ClaudeAPIKey    string `env:"CLAUDE_API_KEY"`
	ClaudeModel     string `env:"CLAUDE_MODEL" envDefault:"claude-haiku-4-5-20251001"`
	EmbeddingModel  string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`
	EmbeddingDim    int    `env:"EMBEDDING_DIM" envDefault:"768"`
}

// Load parses environment variables into Config and performs validation.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("parse env: %w", err)
	}

	if cfg.Database.URL == "" {
		return nil, fmt.Errorf("POSTGRES_URL is required")
	}
	// Standardized NATS URL: accept EVENTS_NATS_URL when MF_EVENTS_NATS_URL is not set
	if cfg.Events.NATSURL == "" {
		cfg.Events.NATSURL = os.Getenv("EVENTS_NATS_URL")
	}
	// Standardized S2S key: MF_SUBSCRIPTIONS_API_KEY → INTERNAL_SERVICE_KEY (uniform across all services)
	if cfg.Subscriptions.APIKey == "" {
		cfg.Subscriptions.APIKey = os.Getenv("INTERNAL_SERVICE_KEY")
	}
	return cfg, nil
}

// LoadDatabaseOnly parses only database-related environment variables.
func LoadDatabaseOnly() (DatabaseConfig, error) {
	type dbOnlyConfig struct {
		Database DatabaseConfig `envPrefix:""`
	}
	cfg := &dbOnlyConfig{}
	if err := env.Parse(cfg); err != nil {
		return DatabaseConfig{}, fmt.Errorf("parse env: %w", err)
	}
	if cfg.Database.URL == "" {
		return DatabaseConfig{}, fmt.Errorf("POSTGRES_URL is required")
	}
	return cfg.Database, nil
}
