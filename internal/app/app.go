package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/cache"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/database"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-api/internal/integrations/gcal"
	mfmiddleware "github.com/bengobox/marketflow-api/internal/middleware"
	"github.com/bengobox/marketflow-api/internal/modules/accounts"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
	"github.com/bengobox/marketflow-api/internal/modules/admin"
	"github.com/bengobox/marketflow-api/internal/modules/analytics"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	backupmod "github.com/bengobox/marketflow-api/internal/modules/backup"
	"github.com/bengobox/marketflow-api/internal/modules/campaigns"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	custom_agents "github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	custom_fields "github.com/bengobox/marketflow-api/internal/modules/custom_fields"
	"github.com/bengobox/marketflow-api/internal/modules/deals"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/modules/integrations"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"github.com/bengobox/marketflow-api/internal/modules/pipelines"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/modules/segments"
	"github.com/bengobox/marketflow-api/internal/modules/sequences"
	"github.com/bengobox/marketflow-api/internal/modules/shortlinks"
	"github.com/bengobox/marketflow-api/internal/modules/tasks"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/bengobox/marketflow-api/internal/modules/webhooks"
	platformevents "github.com/bengobox/marketflow-api/internal/platform/events"
	"github.com/bengobox/marketflow-api/internal/platform/subscriptions"
	"github.com/bengobox/marketflow-api/internal/providers"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// App wires core dependencies and exposes server lifecycle controls.
type App struct {
	cfg                  *config.Config
	logger               *zap.Logger
	entClient            *ent.Client
	db                   *sql.DB
	redis                *redis.Client
	natsConn             *nats.Conn
	outboxPublisher      *eventslib.OutboxPoller
	tenantConsumer       *platformevents.TenantConsumer
	subscriptionConsumer *platformevents.SubscriptionConsumer
	tenantPurgeConsumer  *platformevents.TenantPurgeConsumer
	providerManager      *providers.Manager
	rbacService          *rbac.Service
	usagePublisher       *mfmiddleware.UsagePublisher
	httpServer           *http.Server
}

// New constructs the application.
func New(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*App, error) {
	// ── Database ──────────────────────────────────────────────────────────
	entClient, sqlDB, err := database.NewClient(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	if cfg.Database.RunMigrations {
		// Use direct PostgreSQL connection for migrations to bypass PgBouncer
		// transaction mode (which blocks Atlas advisory locks and DDL transactions).
		migCfg := cfg.Database
		if migCfg.MigrateURL != "" {
			migCfg.URL = migCfg.MigrateURL
		}
		migClient, migDB, err := database.NewClient(ctx, migCfg)
		if err != nil {
			return nil, fmt.Errorf("migration client: %w", err)
		}
		defer migDB.Close()
		defer migClient.Close()
		if err := database.RunMigrations(ctx, migClient); err != nil {
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
	var outboxPub *eventslib.OutboxPoller
	if cfg.Events.Enabled {
		natsConn, err = platformevents.Connect(cfg.Events)
		if err != nil {
			return nil, fmt.Errorf("connect to NATS: %w", err)
		}
		logger.Info("NATS connection established", zap.String("url", cfg.Events.NATSURL))

		outboxRepo := eventslib.NewSQLOutboxRepository(sqlDB)
		outboxNatsPublisher := eventslib.NewNATSAdapter(natsConn, logger)
		outboxPub = eventslib.NewOutboxPoller(outboxRepo, outboxNatsPublisher, logger, eventslib.PollerConfig{
			BatchSize:  cfg.Events.OutboxBatchSize,
			PollPeriod: cfg.Events.OutboxPollPeriod,
		})
		outboxPub.Start(ctx)
	}

	// ── Provider-secret Encryption Key Provider ───────────────────────────
	// DB-first (platform ServiceConfig encryption_key), env fallback
	// (MF_SECURITY_ENCRYPTION_KEY). Empty candidate list ⇒ encryption disabled.
	keyProvider := encryption.NewKeyProvider(entClient, cfg.Security.EncryptionKey)

	// ── Provider Config Manager ───────────────────────────────────────────
	providerMgr := providers.NewManager(entClient, keyProvider, cfg.App.Environment, cfg.Security.PlatformTenant, logger)

	// ── Tenant Syncer ─────────────────────────────────────────────────────
	tenantSyncer := tenant.NewSyncer(entClient, logger)
	// Backfill the default Walk-in customer for all existing tenants (idempotent, one-shot at boot).
	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		tenantSyncer.EnsureWalkInForAllTenants(bgCtx)
	}()

	// ── RBAC ──────────────────────────────────────────────────────────────
	rbacRepo := rbac.NewEntRepository(entClient)
	rbacService := rbac.NewService(rbacRepo, logger)

	// ── Subscriptions S2S client (consumer entitlement gating) ─────────────
	// Used by NATS event consumers (tenant_id, no JWT) to gate OPTIONAL feature-
	// specific CRM projections. consumerFeatureGate mirrors the HTTP-layer gating
	// contract; cached per tenant; fails open on subscriptions-api outage. Core
	// customer/contact/tenant identity sync (which MarketFlow owns) is never gated.
	subsClient := subscriptions.NewClient(subscriptions.Config{
		ServiceURL: cfg.Subscriptions.URL,
		APIKey:     cfg.Subscriptions.APIKey,
	})
	consumerFeatureGate := func(ctx context.Context, tenantID, feature string) bool {
		return subsClient.ConsumerHasFeature(ctx, tenantID, feature)
	}

	// ── NATS Tenant Consumer ──────────────────────────────────────────────
	var tenantConsumer *platformevents.TenantConsumer
	var subscriptionConsumer *platformevents.SubscriptionConsumer
	var tenantPurgeConsumer *platformevents.TenantPurgeConsumer
	if natsConn != nil {
		tenantConsumer = platformevents.NewTenantConsumer(natsConn, tenantSyncer, logger)
		if err := tenantConsumer.Start(); err != nil {
			logger.Warn("failed to start tenant consumer", zap.Error(err))
		}

		subCacheSub := subscriptions.NewCacheSubscriber(redisClient, logger)
		if err := subCacheSub.Start(natsConn); err != nil {
			logger.Warn("app: failed to start subscription cache subscriber", zap.Error(err))
		}

		loyaltyConsumer := platformevents.NewLoyaltyConsumer(entClient, logger)
		loyaltyConsumer.SetFeatureGate(consumerFeatureGate)
		if err := loyaltyConsumer.Start(natsConn); err != nil {
			logger.Warn("app: failed to start loyalty tier consumer", zap.Error(err))
		}

		subscriptionConsumer = platformevents.NewSubscriptionConsumer(natsConn, entClient, logger)
		if err := subscriptionConsumer.Start(); err != nil {
			logger.Warn("app: failed to start subscription consumer", zap.Error(err))
		}

		// Tenant purge consumer: irreversibly deletes ALL of a tenant's MarketFlow
		// data on a platform-owner-confirmed dormancy purge (tenant.purge). Uses the
		// raw *sql.DB for ordered, transactional, FK-safe deletes.
		tenantPurgeConsumer = platformevents.NewTenantPurgeConsumer(sqlDB, logger)
		if err := tenantPurgeConsumer.Start(natsConn); err != nil {
			logger.Warn("app: failed to start tenant purge consumer", zap.Error(err))
		}
	}

	// ── Platform Provider Admin Handler ────────────────────────────────────
	platformProviderHandler := admin.NewPlatformProviderHandler(entClient, keyProvider, logger)
	encryptionKeyHandler := admin.NewEncryptionKeyHandler(entClient, keyProvider, logger)
	adminTenantsHandler := admin.NewTenantsHandler(entClient, logger)
	serviceConfigHandler := admin.NewServiceConfigHandler(entClient, logger)

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
		// Accept platform X-API-Key in addition to JWT so service-to-service
		// callers (marketflow-ai agent tools, notifications-api, marketflow-worker)
		// can reach tenant-scoped endpoints without a user token.
		apiKeyValidator := authclient.NewAPIKeyValidator(cfg.Security.AuthServiceURL, nil)
		authMiddleware = authclient.NewAuthMiddlewareWithAPIKey(validator, apiKeyValidator)
		logger.Info("auth middleware initialized (JWT + platform X-API-Key)",
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
	contactHandler := contacts.NewHandler(contactService, logger,
		contacts.WithView360Config(contacts.View360Config{
			OrderingURL: cfg.CrossService.OrderingURL,
			POSURL:      cfg.CrossService.POSURL,
			TreasuryURL: cfg.CrossService.TreasuryURL,
			ServiceKey:  cfg.CrossService.InternalServiceKey,
		}),
	)

	shortLinkService := shortlinks.NewService(entClient, logger, "")
	shortLinkHandler := shortlinks.NewHandler(shortLinkService, logger)

	kbService := knowledge.NewService(entClient, logger)
	kbHandler := knowledge.NewHandler(kbService, logger)

	campaignService := campaigns.NewService(entClient, logger)
	campaignHandler := campaigns.NewHandler(campaignService, logger)

	funnelService := funnels.NewService(entClient, logger)
	funnelHandler := funnels.NewHandler(funnelService, logger)
	funnelAnalyticsHandler := funnels.NewAnalyticsHandler(funnelService, redisClient, entClient, logger)

	sequenceService := sequences.NewService(entClient, logger)
	sequenceHandler := sequences.NewHandler(sequenceService, logger)

	meetingService := meetings.NewService(entClient, gcal.NewSyncer(entClient, logger), logger)
	meetingHandler := meetings.NewHandler(meetingService, logger)

	analyticsHandler := analytics.NewHandler(entClient, logger)
	auditHandler := audit.NewHandler(entClient, logger)

	// CRM Sprint 9 modules
	dealService := deals.NewService(entClient, logger)
	dealHandler := deals.NewHandler(dealService, logger)

	pipelineService := pipelines.NewService(entClient, logger)
	pipelineHandler := pipelines.NewHandler(pipelineService, logger)

	accountService := accounts.NewService(entClient, logger)
	accountHandler := accounts.NewHandler(accountService, logger)

	activityService := activities.NewService(entClient, logger)
	activityHandler := activities.NewHandler(activityService, logger)

	taskService := tasks.NewService(entClient, logger)
	taskHandler := tasks.NewHandler(taskService, logger)

	customFieldService := custom_fields.NewService(entClient, logger)
	customFieldHandler := custom_fields.NewHandler(customFieldService, logger)

	customAgentServiceOpts := []func(*custom_agents.Service){}
	if natsConn != nil {
		customAgentServiceOpts = append(customAgentServiceOpts, custom_agents.WithNATS(natsConn))
	}
	customAgentService := custom_agents.NewService(entClient, logger, customAgentServiceOpts...)
	customAgentHandler := custom_agents.NewHandler(customAgentService, logger)

	segmentService := segments.NewService(entClient, logger)
	segmentHandler := segments.NewHandler(segmentService, logger)

	// ── Subscription usage + feature gate ─────────────────────────────
	usagePub := mfmiddleware.NewUsagePublisher(cfg.Subscriptions.URL, cfg.Subscriptions.APIKey, logger)
	featureGate := mfmiddleware.NewFeatureGate(cfg.Subscriptions.URL, cfg.Subscriptions.APIKey, logger)

	// Integrations OAuth flow handler. Uses the env encryption key as the HMAC
	// key for signing `state`; no separate secret needed. This is the OAuth
	// state-signing key (must stay stable across the redirect round-trip) and is
	// deliberately kept on the env value rather than the rotatable DB key.
	integrationsHandler := integrations.NewHandler(entClient, encryption.KeyFromEnv(cfg.Security.EncryptionKey), logger)

	// Webhook handlers — Meta app credentials are platform-level (MF_META_*),
	// shared across all tenants; per-tenant link is Tenant.meta_page_id.
	metaWebhookHandler := webhooks.NewMetaHandler(leadService, webhooks.MetaConfig{
		DB:          sqlDB,
		AppSecret:   cfg.Meta.AppSecret,
		VerifyToken: cfg.Meta.VerifyToken,
	}, logger)
	calcomWebhookHandler := webhooks.NewCalcomHandler(meetingService, leadService, logger)
	tiktokWebhookHandler := webhooks.NewTikTokHandler(leadService, webhooks.TikTokConfig{
		DB:           sqlDB,
		SharedSecret: cfg.TikTok.WebhookSecret,
	}, logger)

	// Testimonial CRUD + public reader for /p/{slug}
	testimonialHandler := testimonials.NewHandler(entClient, logger)
	tenantProfileHandler := tenant.NewHandler(entClient, logger)
	tenantMembersHandler := tenant.NewMembersHandler(rbacService, cfg.Security.AuthServiceURL, cfg.CrossService.InternalServiceKey, logger)

	// Public handlers
	publicHandler := webhooks.NewPublicHandler(entClient, funnelService, leadService, testimonialHandler, tenantSyncer, logger)

	// Tenant-scoped backups + daily 02:00 auto-backup scheduler + retention churn.
	backupSvc := backupmod.NewService(sqlDB, entClient, cfg.Backup.Dir, logger)
	backupHandler := backupmod.NewHandler(backupSvc, cfg.Backup.RetentionDays, logger)
	backupmod.NewScheduler(backupSvc, backupmod.SchedulerConfig{
		Enabled:       cfg.Backup.ScheduleEnabled,
		Hour:          cfg.Backup.ScheduleHour,
		RetentionDays: cfg.Backup.RetentionDays,
	}, logger).Start(ctx)

	// ── Router ────────────────────────────────────────────────────────────
	var requireAuth func(http.Handler) http.Handler
	if authMiddleware != nil {
		requireAuth = authMiddleware.RequireAuth
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Log:            logger,
		HealthHandler:  handlers.Health,
		MetricsHandler: promhttp.Handler(),
		BackupHandlers: httpapi.BackupHandlers{
			List:           backupHandler.List,
			Create:         backupHandler.Create,
			GetSettings:    backupHandler.GetSettings,
			UpdateSettings: backupHandler.UpdateSettings,
			Churn:          backupHandler.Churn,
			Download:       backupHandler.Download,
			Delete:         backupHandler.Delete,
		},
		LeadHandlers: httpapi.LeadHandlers{
			Create: leadHandler.Create,
			List:   leadHandler.List,
			Get:    leadHandler.Get,
			Update: leadHandler.Update,
			Delete: leadHandler.Delete,
			Export: leadHandler.Export,
		},
		ContactHandlers: httpapi.ContactHandlers{
			Create:        contactHandler.Create,
			List:          contactHandler.List,
			Get:           contactHandler.Get,
			Update:        contactHandler.Update,
			Delete:        contactHandler.Delete,
			Get360:        contactHandler.Get360,
			GetByEmail:    contactHandler.GetByEmail,
			GetByPhone:    contactHandler.GetByPhone,
			UpsertByPhone: contactHandler.UpsertByPhone,
		},
		KnowledgeHandlers: httpapi.KnowledgeHandlers{
			CreateEntry: kbHandler.CreateEntry,
			ListEntries: kbHandler.ListEntries,
			GetEntry:    kbHandler.GetEntry,
			UpdateEntry: kbHandler.UpdateEntry,
			DeleteEntry: kbHandler.DeleteEntry,
			ImportCSV:   kbHandler.ImportCSV,
		},
		CampaignHandlers: httpapi.CampaignHandlers{
			Create:  campaignHandler.Create,
			List:    campaignHandler.List,
			Get:     campaignHandler.Get,
			Update:  campaignHandler.Update,
			Delete:  campaignHandler.Delete,
			Pause:   campaignHandler.Pause,
			Resume:  campaignHandler.Resume,
			SendSMS: campaignHandler.SendSMS,
		},
		FunnelHandlers: httpapi.FunnelHandlers{
			Create:    funnelHandler.Create,
			List:      funnelHandler.List,
			Get:       funnelHandler.Get,
			Update:    funnelHandler.Update,
			Delete:    funnelHandler.Delete,
			Publish:   funnelHandler.Publish,
			Analytics: funnelAnalyticsHandler.GetAnalytics,
		},
		SequenceHandlers: httpapi.SequenceHandlers{
			Create:    sequenceHandler.Create,
			List:      sequenceHandler.List,
			Get:       sequenceHandler.Get,
			Update:    sequenceHandler.Update,
			Delete:    sequenceHandler.Delete,
			Analytics: sequenceHandler.Analytics,
		},
		MeetingHandlers: httpapi.MeetingHandlers{
			Create: meetingHandler.Create,
			List:   meetingHandler.List,
			Get:    meetingHandler.Get,
			Update: meetingHandler.Update,
		},
		WebhookHandlers: httpapi.WebhookHandlers{
			MetaVerify:            metaWebhookHandler.Verify,
			MetaLeadgen:           metaWebhookHandler.HandleLeadgen,
			CalcomBooking:         calcomWebhookHandler.HandleBooking,
			NotificationsDelivery: publicHandler.NotificationsDelivery,
			TikTokLeads:           tiktokWebhookHandler.HandleLeads,
		},
		PublicHandlers: httpapi.PublicHandlers{
			GetProfile:         publicHandler.GetProfile,
			GetFunnel:          publicHandler.GetFunnel,
			RenderFunnel:       publicHandler.RenderFunnel,
			CreateLead:         publicHandler.PublicCreateLead,
			RecordProfileEvent: publicHandler.RecordProfileEvent,
			ListTestimonials:   publicHandler.ListTestimonials,
			FunnelStepView:     funnelAnalyticsHandler.RecordStepView,
		},
		TenantSyncer: tenantSyncer,
		TenantProfileHandlers: httpapi.TenantProfileHandlers{
			Get:    tenantProfileHandler.GetProfile,
			Update: tenantProfileHandler.UpdateProfile,
		},
		TenantMemberHandlers: httpapi.TenantMemberHandlers{
			List:       tenantMembersHandler.ListMembers,
			Invite:     tenantMembersHandler.InviteMember,
			UpdateRole: tenantMembersHandler.UpdateMemberRole,
			Remove:     tenantMembersHandler.RemoveMember,
		},
		TestimonialHandlers: httpapi.TestimonialHandlers{
			List:   testimonialHandler.List,
			Create: testimonialHandler.Create,
			Update: testimonialHandler.Update,
			Delete: testimonialHandler.Delete,
		},
		ShortLinkHandlers: httpapi.ShortLinkHandlers{
			Create:   shortLinkHandler.Create,
			List:     shortLinkHandler.List,
			Redirect: shortLinkHandler.Redirect,
		},
		AnalyticsHandlers: httpapi.AnalyticsHandlers{
			Dashboard:       analyticsHandler.Dashboard,
			TenantAudit:     auditHandler.ListTenant,
			PlatformUsage:   analyticsHandler.PlatformUsage,
			PlatformAudit:   auditHandler.ListPlatform,
			CampaignMetrics: analyticsHandler.CampaignMetrics,
			SequenceMetrics: analyticsHandler.SequenceMetrics,
			ProfileMetrics:  analyticsHandler.ProfileMetrics,
		},
		IntegrationsHandlers: httpapi.IntegrationsHandlers{
			AvailableIntegrations: integrationsHandler.AvailableIntegrations,
			GoogleAuthorize:       integrationsHandler.GoogleAuthorize,
			GoogleCallback:        integrationsHandler.GoogleCallback,
			MetaAuthorize:         integrationsHandler.MetaAuthorize,
			MetaCallback:          integrationsHandler.MetaCallback,
			TikTokAuthorize:       integrationsHandler.TikTokAuthorize,
			TikTokCallback:        integrationsHandler.TikTokCallback,
			ListTenant:            integrationsHandler.ListTenant,
			DisconnectTenant:      integrationsHandler.DisconnectTenant,
		},
		AdminTenantsHandlers: httpapi.AdminTenantsHandlers{
			List: adminTenantsHandler.List,
		},
		AdminProviderHandlers: httpapi.AdminProviderHandlers{
			Configure:   platformProviderHandler.ConfigureProvider,
			List:        platformProviderHandler.ListProviders,
			GetSettings: platformProviderHandler.GetProviderSettings,
			Resolve:     platformProviderHandler.ResolveProvider,
			SyncModels:  platformProviderHandler.SyncModels,
			Test:        platformProviderHandler.TestProvider,
			Deactivate:  platformProviderHandler.DeactivateProvider,
		},
		AdminServiceConfigHandlers: httpapi.AdminServiceConfigHandlers{
			ListPlatform:   serviceConfigHandler.ListPlatformSettings,
			UpsertPlatform: serviceConfigHandler.UpsertPlatformSetting,
			ListTenant:     serviceConfigHandler.ListTenantSettings,
			UpsertTenant:   serviceConfigHandler.UpsertTenantSetting,
		},
		AdminEncryptionKeyHandlers: httpapi.AdminEncryptionKeyHandlers{
			Get: encryptionKeyHandler.GetEncryptionKey,
			Set: encryptionKeyHandler.SetEncryptionKey,
		},
		InternalLeadHandlers: httpapi.InternalLeadHandlers{
			IngestFromService: leadHandler.IngestFromService,
		},
		InternalServiceKey: cfg.Subscriptions.APIKey,
		DealHandlers: httpapi.DealHandlers{
			Create:   dealHandler.Create,
			List:     dealHandler.List,
			Get:      dealHandler.Get,
			Update:   dealHandler.Update,
			Move:     dealHandler.Move,
			MarkWon:  dealHandler.MarkWon,
			MarkLost: dealHandler.MarkLost,
			Delete:   dealHandler.Delete,
		},
		PipelineHandlers: httpapi.PipelineHandlers{
			Create:      pipelineHandler.Create,
			List:        pipelineHandler.List,
			Get:         pipelineHandler.Get,
			Update:      pipelineHandler.Update,
			Delete:      pipelineHandler.Delete,
			CreateStage: pipelineHandler.CreateStage,
			UpdateStage: pipelineHandler.UpdateStage,
			DeleteStage: pipelineHandler.DeleteStage,
		},
		AccountHandlers: httpapi.AccountHandlers{
			Create:       accountHandler.Create,
			List:         accountHandler.List,
			Get:          accountHandler.Get,
			Update:       accountHandler.Update,
			Delete:       accountHandler.Delete,
			ListContacts: accountHandler.ListContacts,
		},
		ActivityHandlers: httpapi.ActivityHandlers{
			Log:             activityHandler.Log,
			GetTimeline:     activityHandler.GetTimeline,
			LeadTimeline:    activityHandler.ListForEntity("lead"),
			ContactTimeline: activityHandler.ListForEntity("contact"),
			DealTimeline:    activityHandler.ListForEntity("deal"),
		},
		SuggestionHandlers: httpapi.SuggestionHandlers{
			List:    activityHandler.ListSuggestions,
			Dismiss: activityHandler.DismissSuggestion,
		},
		TaskHandlers: httpapi.TaskHandlers{
			Create:   taskHandler.Create,
			List:     taskHandler.List,
			Get:      taskHandler.Get,
			Update:   taskHandler.Update,
			Complete: taskHandler.Complete,
			Delete:   taskHandler.Delete,
		},
		CustomFieldHandlers: httpapi.CustomFieldHandlers{
			CreateDef:        customFieldHandler.CreateDef,
			ListDefs:         customFieldHandler.ListDefs,
			UpdateDef:        customFieldHandler.UpdateDef,
			DeleteDef:        customFieldHandler.DeleteDef,
			SetLeadValues:    customFieldHandler.SetValues("lead"),
			GetLeadValues:    customFieldHandler.GetValues("lead"),
			SetContactValues: customFieldHandler.SetValues("contact"),
			GetContactValues: customFieldHandler.GetValues("contact"),
			SetDealValues:    customFieldHandler.SetValues("deal"),
			GetDealValues:    customFieldHandler.GetValues("deal"),
		},
		CustomAgentHandlers: httpapi.CustomAgentHandlers{
			Create:     customAgentHandler.Create,
			List:       customAgentHandler.List,
			Get:        customAgentHandler.Get,
			Update:     customAgentHandler.Update,
			Delete:     customAgentHandler.Delete,
			ListRuns:   customAgentHandler.ListRuns,
			TriggerRun: customAgentHandler.TriggerRun,
		},
		SegmentHandlers: httpapi.SegmentHandlers{
			Create:   segmentHandler.Create,
			List:     segmentHandler.List,
			Get:      segmentHandler.Get,
			Update:   segmentHandler.Update,
			Delete:   segmentHandler.Delete,
			Evaluate: segmentHandler.Evaluate,
		},
		RequireAuthHandler:   requireAuth,
		RequirePlatformOwner: requirePlatformOwner(authMiddleware),
		RequireAICredits:     featureGate.RequireFeature("ai_chat_messages", "AI credits exhausted — purchase more at /settings/billing"),
		RequireCampaignSlot:  featureGate.RequireFeature("campaigns_active", "Campaign limit reached — upgrade your plan at /settings/billing"),
		TrackLeadCreated:     usagePub.TrackUsage("leads_captured"),
		TrackCampaignCreated: usagePub.TrackUsage("campaigns_active"),
		AllowedOrigins:       cfg.HTTP.AllowedOrigins,
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
		cfg:                  cfg,
		logger:               logger,
		entClient:            entClient,
		db:                   sqlDB,
		redis:                redisClient,
		natsConn:             natsConn,
		outboxPublisher:      outboxPub,
		tenantConsumer:       tenantConsumer,
		subscriptionConsumer: subscriptionConsumer,
		tenantPurgeConsumer:  tenantPurgeConsumer,
		providerManager:      providerMgr,
		rbacService:          rbacService,
		usagePublisher:       usagePub,
		httpServer:           server,
	}, nil
}

// requirePlatformOwner creates middleware that restricts access to platform owners.
//
// SEC-3 (auth-client v0.10.0): a tenant superuser/admin is NOT a platform owner and must
// not reach platform-default provider config / decrypted-secret resolve / platform usage.
// Interactive (JWT) principals must therefore be claims.IsPlatformOwner. Service-to-service
// callers authenticated via the platform X-API-Key (claims.IsService — e.g. marketflow-ai
// and the worker bootstrapping their provider clients via /admin/providers/resolve) remain
// allowed, since the platform key is itself a trusted platform credential.
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

			if !claims.IsPlatformOwner && !claims.IsService {
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

	if a.subscriptionConsumer != nil {
		a.subscriptionConsumer.Stop()
	}

	if a.tenantPurgeConsumer != nil {
		a.tenantPurgeConsumer.Stop()
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
