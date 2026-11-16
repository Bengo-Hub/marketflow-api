package config

import (
	"fmt"
	"os"
	"time"

	"github.com/bengobox/marketflow-api/internal/platform/legaldocs"
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
	Autopilot     AutopilotConfig     `envPrefix:"MF_AUTOPILOT_"`
}

// AutopilotConfig is the ad autopilot's veto-window mode (Sprint 20, owner 2026-11-14), read by the
// API and the worker alike (MF_AUTOPILOT_*).
type AutopilotConfig struct {
	// DefaultVetoHours is the window an auto_publish agent gets when it sets none (1 to 72).
	DefaultVetoHours int `env:"DEFAULT_VETO_HOURS" envDefault:"4"`
	// TermsVersion is the Autopilot terms version tenants accept; a new one asks everyone again.
	TermsVersion string `env:"TERMS_VERSION" envDefault:"2026-11-14"`
	// A new terms version applies from its effective date (owner 2026-11-14). Until then a tenant
	// that accepted an earlier version keeps auto-publishing; from then on it pauses until it
	// accepts the new one. TermsEffectiveAt (RFC 3339) sets the date outright; otherwise it is
	// TermsAnnouncedAt (RFC 3339) plus TermsNotice. With neither set the version applies at once.
	TermsEffectiveAt string        `env:"TERMS_EFFECTIVE_AT"`
	TermsAnnouncedAt string        `env:"TERMS_ANNOUNCED_AT"`
	TermsNotice      time.Duration `env:"TERMS_NOTICE" envDefault:"336h"`
	// The terms are a versioned auth-api legal document (owner 2026-11-14). Once a version is
	// published there, its version and effective date replace TermsVersion and the dates above,
	// and acceptances are recorded in auth-api; the metadata acceptance is still read as a
	// fallback. TermsFromAuth false keeps config and metadata only.
	TermsFromAuth bool `env:"TERMS_FROM_AUTH" envDefault:"true"`
	// TermsDocType is the auth-api doc_type.
	TermsDocType string `env:"TERMS_DOC_TYPE" envDefault:"MARKETFLOW_AUTOPUBLISH_TERMS"`
	// TermsAuthURL is auth-api for the terms; empty uses the service's auth-api URL
	// (MF_SECURITY_AUTH_SERVICE_URL).
	TermsAuthURL string `env:"TERMS_AUTH_URL"`
	// TermsCacheTTL is how long the document and each tenant's acceptances are cached.
	TermsCacheTTL time.Duration `env:"TERMS_CACHE_TTL" envDefault:"5m"`
	// TermsPageURL is the terms page on the accounts site (a version's page is <url>/<version>).
	TermsPageURL string `env:"TERMS_PAGE_URL" envDefault:"https://accounts.codevertexafrica.com/legal/marketflow-autopublish-terms"`
	// PipelineInterval is how often the worker carries veto-window videos on (the Renderer's
	// reports carry them on at once; this sweep catches anything a report missed).
	PipelineInterval time.Duration `env:"PIPELINE_INTERVAL" envDefault:"1m"`
	// PipelineSettle is how long a version stays still before the sweep acts on it.
	PipelineSettle time.Duration `env:"PIPELINE_SETTLE" envDefault:"2m"`
	// VetoLinkGrace keeps the emailed cancel link answering after the post time, so a late click
	// is told the post already went out instead of "invalid link".
	VetoLinkGrace time.Duration `env:"VETO_LINK_GRACE" envDefault:"168h"`
	// QuoteRetries is how many failed render quotes a video tolerates before it waits for a person.
	QuoteRetries int `env:"QUOTE_RETRIES" envDefault:"5"`
	// OutageHandover is how long a scheduled post keeps waiting while the plan cannot be checked,
	// from the first failed check for that post; then it is handed to the owner to approve by
	// hand and its jobs are cancelled (owner 2026-11-14).
	OutageHandover time.Duration `env:"OUTAGE_HANDOVER" envDefault:"24h"`
}

// TermsSource builds the auth-api terms client (nil when TermsFromAuth is off). authURL is the
// service's auth-api URL, used when TermsAuthURL is empty; serviceKey is the S2S key. Build one per
// process and share it, so the API and every gate use one cache.
func (c AutopilotConfig) TermsSource(authURL, serviceKey string) *legaldocs.Client {
	if !c.TermsFromAuth {
		return nil
	}
	base := c.TermsAuthURL
	if base == "" {
		base = authURL
	}
	return legaldocs.New(legaldocs.Config{BaseURL: base, APIKey: serviceKey, DocType: c.TermsDocType, TTL: c.TermsCacheTTL})
}

// TermsEffective is when the current terms version applies: TermsEffectiveAt, else
// TermsAnnouncedAt plus TermsNotice (14 days by default), else the zero time (at once). A value
// that does not parse counts as unset.
func (c AutopilotConfig) TermsEffective() time.Time {
	if t, err := time.Parse(time.RFC3339, c.TermsEffectiveAt); err == nil {
		return t.UTC()
	}
	if t, err := time.Parse(time.RFC3339, c.TermsAnnouncedAt); err == nil {
		notice := c.TermsNotice
		if notice <= 0 {
			notice = 14 * 24 * time.Hour
		}
		return t.UTC().Add(notice)
	}
	return time.Time{}
}

type SubscriptionsConfig struct {
	URL    string `env:"URL" envDefault:"http://subscriptions-api.infra.svc.cluster.local:4000"`
	APIKey string `env:"API_KEY"`
}

type AppConfig struct {
	// A missing APP_ENV means production: development relaxes webhook signatures (audit MF #9).
	Environment string `env:"APP_ENV" envDefault:"production"`
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
	// DemoPlanCode is the plan a demo tenant (exempt from subscriptions, no plan of its own) gets
	// in the AI work: the public demo password must not unlock paid features (owner, 2026-11-12).
	DemoPlanCode string `env:"DEMO_PLAN_CODE" envDefault:"MARKETFLOW_STARTER" comment:"Free plan a demo tenant's AI features and chat limits follow"`
	AuthServiceURL string `env:"AUTH_SERVICE_URL" envDefault:"https://sso.codevertexafrica.com" comment:"Used by shared-auth-client APIKeyValidator"`
	// EnforceTenantHeader refuses a tenant header that names another tenant than the token.
	// On by default (production enforces it); a values file that drops the variable must not
	// quietly return to report-only. Set false only to diagnose, and the mismatch is then only
	// logged ("tenant in request does not match the token").
	EnforceTenantHeader bool `env:"ENFORCE_TENANT_HEADER" envDefault:"true"`
	// Per-IP limits on the public write routes (Sprint 7 #61), on top of the global 300 a
	// minute: lead forms and bookings are a few a minute from a real visitor; page events are
	// frequent but cheap. 0 turns that limit off.
	PublicLeadsPerMinute    int `env:"PUBLIC_LEADS_PER_MIN" envDefault:"10"`
	PublicBookingsPerMinute int `env:"PUBLIC_BOOKINGS_PER_MIN" envDefault:"10"`
	PublicEventsPerMinute   int `env:"PUBLIC_EVENTS_PER_MIN" envDefault:"120"`
	// The public reads (profile, funnel and booking pages, short links, review links, OAuth
	// callbacks) and the provider webhooks get their own per-IP counters too. A visitor loads a
	// page and a few of its parts; Meta and TikTok send webhooks from a shared pool of addresses,
	// so theirs is set well above any real burst.
	PublicReadsPerMinute int `env:"PUBLIC_READS_PER_MIN" envDefault:"120"`
	WebhooksPerMinute    int `env:"WEBHOOKS_PER_MIN" envDefault:"600"`
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
	// The Graph API version is not configurable: meta.GraphVersion is the one source.
	// DeletionStatusURL is the public page a person lands on to check a data
	// deletion request (Meta's data deletion callback returns it with the code).
	DeletionStatusURL string `env:"DELETION_STATUS_URL" envDefault:"https://marketflow.codevertexafrica.com/data-deletion"`
}

// TikTokConfig holds TikTok Business API settings.
type TikTokConfig struct {
	WebhookSecret string `env:"WEBHOOK_SECRET"` // optional shared secret for webhook verification
	// SigningSecret verifies the TikTok-Signature header (the TikTok app's client secret); the
	// query secret above stays the fallback for events without the header.
	SigningSecret string `env:"SIGNING_SECRET"`
}

// CrossServiceConfig holds internal URLs for S2S calls to sibling services.
type CrossServiceConfig struct {
	InternalServiceKey string `env:"INTERNAL_SERVICE_KEY"`
	// AIURL is marketflow-ai, for the internal credit charge of a paid ad launch.
	AIURL string `env:"AI_URL" envDefault:"http://marketflow-ai.marketflow.svc.cluster.local:4001"`
	// CrawlerURL is marketflow-crawler, the internal page reader; empty turns its use off.
	CrawlerURL  string `env:"CRAWLER_URL" envDefault:"http://marketflow-crawler.marketflow.svc.cluster.local:4010"`
	// PortalURL is the tenant portal, for links in emails (same variable as the worker's).
	PortalURL string `env:"PORTAL_URL" envDefault:"https://marketflow.codevertexafrica.com"`
	OrderingURL string `env:"ORDERING_URL" envDefault:"http://ordering-backend.ordering.svc.cluster.local:4000"`
	POSURL      string `env:"POS_URL" envDefault:"http://pos-api.pos.svc.cluster.local:4000"`
	TreasuryURL string `env:"TREASURY_URL" envDefault:"http://treasury-api.treasury.svc.cluster.local:4000"`
	// InventoryURL: the creative studio's offering picker reads sellable items (S2S).
	InventoryURL string `env:"INVENTORY_URL" envDefault:"http://inventory-api.inventory.svc.cluster.local:4000"`
	// NotificationsURL: campaign sends are handed to notifications-api's broadcast engine.
	NotificationsURL string `env:"NOTIFICATIONS_URL" envDefault:"http://notifications-api.notifications.svc.cluster.local:4000"`
	// WebsiteURL: codevertex-website's public catalogue and courses, read to pre-fill the
	// platform tenant's brand context during onboarding.
	WebsiteURL string `env:"CODEVERTEX_WEBSITE_URL" envDefault:"http://codevertex-website.codevertex.svc.cluster.local"`
}

// AIConfig holds AI/LLM integration settings.
type AIConfig struct {
	OllamaURL      string `env:"OLLAMA_URL" envDefault:"http://ollama.marketflow.svc.cluster.local:11434"`
	OllamaModel    string `env:"OLLAMA_MODEL" envDefault:"llama3.1:8b"`
	ClaudeAPIKey   string `env:"CLAUDE_API_KEY"`
	ClaudeModel    string `env:"CLAUDE_MODEL" envDefault:"claude-haiku-4-5-20251001"`
	EmbeddingModel string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`
	EmbeddingDim   int    `env:"EMBEDDING_DIM" envDefault:"768"`
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
	if err := checkFailClosed(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// checkFailClosed refuses settings that would open the API: JWT auth may be off only in
// development (audit MF #9).
func checkFailClosed(cfg *Config) error {
	if !cfg.Security.RequireJWT && cfg.App.Environment != "development" {
		return fmt.Errorf("MF_SECURITY_REQUIRE_JWT=false is only allowed with APP_ENV=development (APP_ENV=%q)", cfg.App.Environment)
	}
	return nil
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
