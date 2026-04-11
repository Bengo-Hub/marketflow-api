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
	"github.com/bengobox/marketflow-api/internal/modules/admin"
	"github.com/bengobox/marketflow-api/internal/modules/campaigns"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"github.com/bengobox/marketflow-api/internal/modules/outbox"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/modules/sequences"
	"github.com/bengobox/marketflow-api/internal/modules/shortlinks"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/webhooks"
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
	tenantConsumer  *platformevents.TenantConsumer
	providerManager *providers.Manager
	rbacService     *rbac.Service
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

	// ── Tenant Syncer ─────────────────────────────────────────────────────
	tenantSyncer := tenant.NewSyncer(entClient, logger)

	// ── RBAC ──────────────────────────────────────────────────────────────
	rbacRepo := rbac.NewEntRepository(entClient)
	rbacService := rbac.NewService(rbacRepo, logger)

	// ── NATS Tenant Consumer ──────────────────────────────────────────────
	var tenantConsumer *platformevents.TenantConsumer
	if natsConn != nil {
		tenantConsumer = platformevents.NewTenantConsumer(natsConn, tenantSyncer, logger)
		if err := tenantConsumer.Start(); err != nil {
			logger.Warn("failed to start tenant consumer", zap.Error(err))
		}
	}

	// ── Platform Provider Admin Handler ────────────────────────────────────
	platformProviderHandler := admin.NewPlatformProviderHandler(entClient, encKey, logger)

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

	campaignService := campaigns.NewService(entClient, logger)
	campaignHandler := campaigns.NewHandler(campaignService, logger)

	funnelService := funnels.NewService(entClient, logger)
	funnelHandler := funnels.NewHandler(funnelService, logger)

	sequenceService := sequences.NewService(entClient, logger)
	sequenceHandler := sequences.NewHandler(sequenceService, logger)

	meetingService := meetings.NewService(entClient, logger)
	meetingHandler := meetings.NewHandler(meetingService, logger)

	// Webhook handlers
	metaWebhookHandler := webhooks.NewMetaHandler(leadService, logger)
	calcomWebhookHandler := webhooks.NewCalcomHandler(meetingService, leadService, logger)

	// Public handlers
	publicHandler := webhooks.NewPublicHandler(entClient, funnelService, logger)

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
			Create: campaignHandler.Create,
			List:   campaignHandler.List,
			Get:    campaignHandler.Get,
			Update: campaignHandler.Update,
			Delete: campaignHandler.Delete,
			Pause:  campaignHandler.Pause,
			Resume: campaignHandler.Resume,
		},
		FunnelHandlers: httpapi.FunnelHandlers{
			Create:  funnelHandler.Create,
			List:    funnelHandler.List,
			Get:     funnelHandler.Get,
			Update:  funnelHandler.Update,
			Delete:  funnelHandler.Delete,
			Publish: funnelHandler.Publish,
		},
		SequenceHandlers: httpapi.SequenceHandlers{
			Create: sequenceHandler.Create,
			List:   sequenceHandler.List,
			Get:    sequenceHandler.Get,
			Update: sequenceHandler.Update,
			Delete: sequenceHandler.Delete,
		},
		MeetingHandlers: httpapi.MeetingHandlers{
			Create: meetingHandler.Create,
			List:   meetingHandler.List,
			Get:    meetingHandler.Get,
			Update: meetingHandler.Update,
		},
		WebhookHandlers: httpapi.WebhookHandlers{
			MetaLeadgen:   metaWebhookHandler.HandleLeadgen,
			CalcomBooking: calcomWebhookHandler.HandleBooking,
		},
		PublicHandlers: httpapi.PublicHandlers{
			GetProfile: publicHandler.GetProfile,
			GetFunnel:  publicHandler.GetFunnel,
		},
		ShortLinkHandlers: httpapi.ShortLinkHandlers{
			Create:   shortLinkHandler.Create,
			List:     shortLinkHandler.List,
			Redirect: shortLinkHandler.Redirect,
		},
		AdminProviderHandlers: httpapi.AdminProviderHandlers{
			Configure:   platformProviderHandler.ConfigureProvider,
			List:        platformProviderHandler.ListProviders,
			GetSettings: platformProviderHandler.GetProviderSettings,
			Deactivate:  platformProviderHandler.DeactivateProvider,
		},
		RequireAuthHandler: requireAuth,
		RequirePlatformOwner: requirePlatformOwner(authMiddleware),
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
		tenantConsumer:  tenantConsumer,
		providerManager: providerMgr,
		rbacService:     rbacService,
		httpServer:      server,
	}, nil
}

// requirePlatformOwner creates middleware that restricts access to platform owners (superuser role).
func requirePlatformOwner(authMw *authclient.AuthMiddleware) func(http.Handler) http.Handler {
	if authMw == nil {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := authclient.ClaimsFromContext(r.Context())
			if !ok || claims == nil {
				httpapi.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required", nil)
				return
			}

			// Check for superuser or platform owner
			isSuperuser := false
			for _, role := range claims.Roles {
				if role == "superuser" || role == "admin" {
					isSuperuser = true
					break
				}
			}
			if !isSuperuser && !claims.IsPlatformOwner {
				httpapi.Error(w, http.StatusForbidden, "forbidden", "platform owner access required", nil)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// Run starts the HTTP server.
func (a *App) Run() error {
	a.logger.Info("starting HTTP server", zap.String("addr", a.httpServer.Addr))
	return a.httpServer.ListenAndServe()
}

// Shutdown gracefully stops the HTTP server and closes resources.
func (a *App) Shutdown(ctx context.Context) error {
	shutdownErr := a.httpServer.Shutdown(ctx)

	if a.tenantConsumer != nil {
		a.tenantConsumer.Stop()
	}

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
