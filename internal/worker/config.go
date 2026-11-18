// Package worker hosts the marketflow background consumer (NATS JetStream).
// It is built as a separate binary (cmd/worker) but ships in the same Docker image
// as the API and shares the same config / Ent client.
package worker

import (
	"fmt"
	"os"
	"time"

	"github.com/bengobox/marketflow-api/internal/config"
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
	// Redis (same REDIS_* env as the API; both run in one container) backs the
	// once-per-period claims that stop every pod's worker repeating scheduled work.
	Redis config.RedisConfig `envPrefix:"REDIS_"`
	// AuthServiceURL is auth-api, for S2S lookups (the WhatsApp assistant's by-phone check).
	AuthServiceURL string `env:"MF_SECURITY_AUTH_SERVICE_URL" envDefault:"https://sso.codevertexafrica.com"`
	// CrossServiceKey is the S2S key the API pods use for auth-api and the other services.
	CrossServiceKey string `env:"MF_CROSS_INTERNAL_SERVICE_KEY"`
	// PlatformTenant is the platform owner's tenant slug (it sends platform-number replies).
	PlatformTenant string `env:"MF_SECURITY_PLATFORM_TENANT" envDefault:"codevertex"`
	// Autopilot is the ad autopilot's veto-window mode, the same MF_AUTOPILOT_* env as the API.
	Autopilot config.AutopilotConfig `envPrefix:"MF_AUTOPILOT_"`
	// Subscriptions is subscriptions-api (same MF_SUBSCRIPTIONS_* env as the API), read strictly
	// for the autopilot_auto_publish plan feature at each veto-window step.
	Subscriptions config.SubscriptionsConfig `envPrefix:"MF_SUBSCRIPTIONS_"`
	// MediaStorage is the R2 lifecycle backstop and the live-bytes alert (media_lifecycle.go).
	MediaStorage MediaStorageConfig `envPrefix:"MF_MEDIA_"`
}

// MediaStorageConfig drives the per-tenant R2 lifecycle rules and the live-bytes alert.
type MediaStorageConfig struct {
	// R2AccountID and R2LifecycleToken reach Cloudflare's R2 lifecycle API (an account API token
	// with R2 bucket edit, the same API the devops-k8s object-storage workflow uses). Without both
	// the rules are not synced; the alert still runs.
	R2AccountID      string `env:"R2_ACCOUNT_ID"`
	R2LifecycleToken string `env:"R2_LIFECYCLE_TOKEN"`
	R2APIBase        string `env:"R2_API_BASE" envDefault:"https://api.cloudflare.com/client/v4"`
	// Retention is the backstop, in days, after which R2 deletes a tenant's drafts and temporary
	// renders, as JSON: {"default_days":10,"plans":{"PLAN_CODE":30}}. A tenant can carry its own
	// media_retention_days in Tenant.metadata; every value is held between MinDays and MaxDays.
	Retention string `env:"RETENTION" envDefault:"{\"default_days\":10}"`
	MinDays   int    `env:"RETENTION_MIN_DAYS" envDefault:"4"`
	MaxDays   int    `env:"RETENTION_MAX_DAYS" envDefault:"90"`
	// MaxRules caps the rules this job keeps on one bucket (R2 allows 1,000 in all, and the
	// bucket's own rules stay); tenants with the most live bytes come first.
	MaxRules int `env:"LIFECYCLE_MAX_RULES" envDefault:"900"`
	// LifecycleVideoDrafts lets the sync manage video-drafts. Off by default: R2 deletes by age,
	// and a finished video can still be in use by a running ad or post after its retention.
	LifecycleVideoDrafts bool `env:"LIFECYCLE_VIDEO_DRAFTS" envDefault:"false"`
	// LiveBytesAlert is the live-bytes level that alerts the platform (default 8 GiB).
	LiveBytesAlert int64 `env:"LIVE_BYTES_ALERT" envDefault:"8589934592"`
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
	// Chat transcript retention after last activity; 0 disables the daily prune.
	ChatRetentionDays int `env:"CHAT_RETENTION_DAYS" envDefault:"30"`
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
	MarketFlowAPIURL string `env:"API_URL" envDefault:"http://marketflow-api.marketflow.svc.cluster.local:4000"`
	MarketFlowAIURL  string `env:"AI_URL" envDefault:"http://marketflow-ai.marketflow.svc.cluster.local:4001"`
	TicketingAPIURL  string `env:"TICKETING_URL" envDefault:"http://ticketing-api.ticketing.svc.cluster.local:4009"`
	OllamaURL        string `env:"OLLAMA_URL" envDefault:"http://ollama.shared-infra.svc.cluster.local:11434"`
	EmbeddingModel   string `env:"EMBEDDING_MODEL" envDefault:"nomic-embed-text"`
	// InventoryURL and WebsiteURL are read by the knowledge base sync's nightly walk.
	InventoryURL       string `env:"INVENTORY_URL" envDefault:"http://inventory-api.inventory.svc.cluster.local:4000"`
	WebsiteURL         string `env:"WEBSITE_URL" envDefault:"http://codevertex-website.codevertex.svc.cluster.local"`
	InternalServiceKey string `env:"INTERNAL_SERVICE_KEY"`
	// PortalURL is the tenant portal, for links in notices (reconnect an integration).
	PortalURL string `env:"PORTAL_URL" envDefault:"https://marketflow.codevertexafrica.com"`
	// CRMLinkServices lists the services asked "do you still link this CRM contact" before an
	// erased person's contact is deleted, as "name=base_url,..." (MF_WORKER_CRM_LINK_SERVICES).
	// Empty uses the in-cluster defaults for pos, treasury, library, hospital, ordering,
	// ticketing and projects.
	CRMLinkServices string `env:"CRM_LINK_SERVICES"`
	// SignupURL is where the WhatsApp assistant's Sign up button sends a number no MarketFlow
	// user has verified (MF_WORKER_SIGNUP_URL). Empty uses PortalURL, whose home page signs up.
	SignupURL string `env:"SIGNUP_URL"`
	// SignupReplyEvery is how often one unknown number may get the sign-up reply
	// (MF_WORKER_SIGNUP_REPLY_EVERY).
	SignupReplyEvery time.Duration `env:"SIGNUP_REPLY_EVERY" envDefault:"720h"`
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
	// The worker deployment never set MF_WORKER_INTERNAL_SERVICE_KEY, so every S2S call it made
	// (ticketing SLA checks, marketflow-ai jobs) went out without a key and got 401. It carries
	// the same key as MF_CROSS_INTERNAL_SERVICE_KEY and INTERNAL_SERVICE_KEY, so fall back to them.
	if c.API.InternalServiceKey == "" {
		c.API.InternalServiceKey = firstSet(c.CrossServiceKey, os.Getenv("INTERNAL_SERVICE_KEY"))
	}
	if c.CrossServiceKey == "" {
		c.CrossServiceKey = c.API.InternalServiceKey
	}
	// The same fallback as the API's: the S2S key is uniform across services.
	if c.Subscriptions.APIKey == "" {
		c.Subscriptions.APIKey = firstSet(os.Getenv("INTERNAL_SERVICE_KEY"), c.API.InternalServiceKey)
	}
	return c, nil
}

func firstSet(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
