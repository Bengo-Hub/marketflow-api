package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/cache"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/database"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/outbox"
	"github.com/bengobox/marketflow-api/internal/modules/shortlinks"
	platformevents "github.com/bengobox/marketflow-api/internal/platform/events"
	"github.com/bengobox/marketflow-api/internal/providers"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// App wires core dependencies and exposes server lifecycle controls.
type App struct {
	cfg             *config.Config
	logger          *zap.Logger
	entClient       *ent.Client
	db              *sql.DB
	redis           *redis.Client
	natsConn        *nats.Conn
	outboxPublisher *outbox.Publisher
	providerManager *providers.Manager
	httpServer      *http.Server
}

// New constructs the application.
func New(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*App, error) {
	// ── Database ──────────────────────────────────────────────────────────
	entClient, sqlDB, err := database.NewClient(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	if cfg.Database.RunMigrations {
		if err := database.RunMigrations(ctx, entClient); err != nil {
			return nil, err
		}
	}

	// ── Redis ─────────────────────────────────────────────────────────────
	redisClient, err := cache.New(cfg.Redis)
	if err != nil {
		return nil, err
	}

	// ── NATS Event Publishing (optional) ──────────────────────────────────
	var natsConn *nats.Conn
	var outboxPub *outbox.Publisher
	if cfg.Events.Enabled {
		natsConn, err = platformevents.Connect(cfg.Events)
		if err != nil {
			return nil, fmt.Errorf("connect to NATS: %w", err)
		}
		logger.Info("NATS connection established", zap.String("url", cfg.Events.NATSURL))

		outboxRepo := eventslib.NewSQLOutboxRepository(sqlDB)
		outboxNatsPublisher := platformevents.NewOutboxPublisher(natsConn, logger)
		outboxPub = outbox.NewPublisher(outboxRepo, outboxNatsPublisher, logger, outbox.PublisherConfig{
			BatchSize:  cfg.Events.OutboxBatchSize,
			PollPeriod: cfg.Events.OutboxPollPeriod,
		})
		outboxPub.Start(ctx)
	}

	// ── Provider Config Manager ───────────────────────────────────────────
	encKey := encryption.KeyFromEnv(cfg.Security.EncryptionKey)
	providerMgr := providers.NewManager(entClient, encKey, cfg.App.Environment, cfg.Security.PlatformTenant, logger)

	// ── Auth Middleware (JWT via shared auth-client JWKS) ──────────────────
	var authMiddleware *authclient.AuthMiddleware
	if cfg.Security.RequireJWT {
		authConfig := authclient.DefaultConfig(
			cfg.Security.JWKSUrl,
			cfg.Security.JWTIssuer,
			cfg.Security.JWTAudience,
		)
		authConfig.RedisClient = redisClient
		validator, err := authclient.NewValidator(authConfig)
		if err != nil {
			return nil, fmt.Errorf("create JWT validator: %w", err)
		}
		authMiddleware = authclient.NewAuthMiddleware(validator)
		logger.Info("JWT auth middleware initialized",
			zap.String("jwks_url", cfg.Security.JWKSUrl),
			zap.String("issuer", cfg.Security.JWTIssuer),
		)
	} else {
		logger.Warn("JWT auth disabled — all routes are unprotected (development mode only)")
	}

	// ── Module Services ───────────────────────────────────────────────────
	leadService := leads.NewService(entClient, logger)
	leadHandler := leads.NewHandler(leadService, logger)

	contactService := contacts.NewService(entClient, logger)
	contactHandler := contacts.NewHandler(contactService, logger)

	shortLinkService := shortlinks.NewService(entClient, logger, "")
	shortLinkHandler := shortlinks.NewHandler(shortLinkService, logger)

	kbService := knowledge.NewService(entClient, logger)
	kbHandler := knowledge.NewHandler(kbService, logger)

	// Placeholder for modules not yet implemented (Sprint 4+)
	placeholder := func(w http.ResponseWriter, r *http.Request) {
		httpapi.JSON(w, http.StatusNotImplemented, map[string]any{"error": "not implemented yet"})
	}

	// ── Router ────────────────────────────────────────────────────────────
	var requireAuth func(http.Handler) http.Handler
	if authMiddleware != nil {
		requireAuth = authMiddleware.RequireAuth
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		HealthHandler:  handlers.Health,
		MetricsHandler: promhttp.Handler(),
		LeadHandlers: httpapi.LeadHandlers{
			Create: leadHandler.Create,
			List:   leadHandler.List,
			Get:    leadHandler.Get,
			Update: leadHandler.Update,
			Delete: leadHandler.Delete,
		},
		ContactHandlers: httpapi.ContactHandlers{
			Create: contactHandler.Create,
			List:   contactHandler.List,
			Get:    contactHandler.Get,
			Update: contactHandler.Update,
			Delete: contactHandler.Delete,
		},
		KnowledgeHandlers: httpapi.KnowledgeHandlers{
			CreateEntry: kbHandler.CreateEntry,
			ListEntries: kbHandler.ListEntries,
			GetEntry:    kbHandler.GetEntry,
			UpdateEntry: kbHandler.UpdateEntry,
			DeleteEntry: kbHandler.DeleteEntry,
		},
		CampaignHandlers: httpapi.CampaignHandlers{
			Create: placeholder,
			List:   placeholder,
			Get:    placeholder,
			Update: placeholder,
			Delete: placeholder,
			Pause:  placeholder,
			Resume: placeholder,
		},
		FunnelHandlers: httpapi.FunnelHandlers{
			Create:  placeholder,
			List:    placeholder,
			Get:     placeholder,
			Update:  placeholder,
			Delete:  placeholder,
			Publish: placeholder,
		},
		ShortLinkHandlers: httpapi.ShortLinkHandlers{
			Create:   shortLinkHandler.Create,
			List:     shortLinkHandler.List,
			Redirect: shortLinkHandler.Redirect,
		},
		RequireAuthHandler: requireAuth,
		AllowedOrigins:     cfg.HTTP.AllowedOrigins,
	})

	// ── HTTP Server ───────────────────────────────────────────────────────
	server := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", cfg.HTTP.Host, cfg.HTTP.Port),
		Handler:           router,
		ReadTimeout:       cfg.HTTP.ReadTimeout,
		ReadHeaderTimeout: cfg.HTTP.ReadHeaderTimeout,
		WriteTimeout:      cfg.HTTP.WriteTimeout,
		IdleTimeout:       cfg.HTTP.IdleTimeout,
	}

	return &App{
		cfg:             cfg,
		logger:          logger,
		entClient:       entClient,
		db:              sqlDB,
		redis:           redisClient,
		natsConn:        natsConn,
		outboxPublisher: outboxPub,
		providerManager: providerMgr,
		httpServer:      server,
	}, nil
}

// Run starts the HTTP server.
func (a *App) Run() error {
	a.logger.Info("starting HTTP server", zap.String("addr", a.httpServer.Addr))
	return a.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server and closes resources.
func (a *App) Shutdown(ctx context.Context) error {
	shutdownErr := a.httpServer.Shutdown(ctx)

	if a.outboxPublisher != nil {
		a.outboxPublisher.Stop()
	}
	if a.natsConn != nil {
		a.natsConn.Drain()
		a.natsConn.Close()
	}
	if err := a.entClient.Close(); err != nil {
		a.logger.Warn("failed to close ent client", zap.Error(err))
		if shutdownErr == nil {
			shutdownErr = err
		}
	}
	if a.db != nil {
		if err := a.db.Close(); err != nil {
			a.logger.Warn("failed to close database", zap.Error(err))
			if shutdownErr == nil {
				shutdownErr = err
			}
		}
	}
	if err := a.redis.Close(); err != nil {
		a.logger.Warn("failed to close redis client", zap.Error(err))
		if shutdownErr == nil {
			shutdownErr = err
		}
	}
	return shutdownErr
}
