package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/Bengo-Hub/httpware/objectstore"
	"github.com/Bengo-Hub/httpware/tenantexport"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"github.com/google/uuid"
	"net/http"
	"os"
	"strings"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/database"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/httpapi/handlers"
	"github.com/bengobox/marketflow-api/internal/integrations/gcal"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	mfmiddleware "github.com/bengobox/marketflow-api/internal/middleware"
	"github.com/bengobox/marketflow-api/internal/modules/accounts"
	"github.com/bengobox/marketflow-api/internal/modules/activities"
	"github.com/bengobox/marketflow-api/internal/modules/admin"
	"github.com/bengobox/marketflow-api/internal/modules/adsmanager"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
	"github.com/bengobox/marketflow-api/internal/modules/analytics"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
	"github.com/bengobox/marketflow-api/internal/modules/autopublish"
	"github.com/bengobox/marketflow-api/internal/modules/brandimages"
	"github.com/bengobox/marketflow-api/internal/modules/campaigns"
	"github.com/bengobox/marketflow-api/internal/modules/contacts"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	custom_agents "github.com/bengobox/marketflow-api/internal/modules/custom_agents"
	custom_fields "github.com/bengobox/marketflow-api/internal/modules/custom_fields"
	"github.com/bengobox/marketflow-api/internal/modules/deals"
	"github.com/bengobox/marketflow-api/internal/modules/endpoints"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/modules/inbox"
	"github.com/bengobox/marketflow-api/internal/modules/integrations"
	"github.com/bengobox/marketflow-api/internal/modules/knowledge"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/bengobox/marketflow-api/internal/modules/learning"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/modules/meetings"
	"github.com/bengobox/marketflow-api/internal/modules/onboarding"
	"github.com/bengobox/marketflow-api/internal/modules/pipelines"
	"github.com/bengobox/marketflow-api/internal/modules/publishing"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/modules/segments"
	"github.com/bengobox/marketflow-api/internal/modules/sequences"
	"github.com/bengobox/marketflow-api/internal/modules/shortlinks"
	"github.com/bengobox/marketflow-api/internal/modules/tasks"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/testimonials"
	"github.com/bengobox/marketflow-api/internal/modules/webhooks"
	"github.com/bengobox/marketflow-api/internal/notifications"
	platformevents "github.com/bengobox/marketflow-api/internal/platform/events"
	"github.com/bengobox/marketflow-api/internal/platform/fxrate"
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
// tenantExportHandler serves auth-api's SSO tenant backups (httpware/tenantexport). Nil without an
// INTERNAL_SERVICE_KEY, so the export is never reachable unauthenticated.
// whatsAppRichConfig reads the WhatsApp window and media limits; a bad value keeps its default.
func whatsAppRichConfig(logger *zap.Logger) creative.WhatsAppConfig {
	cfg, err := creative.WhatsAppConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Warn("whatsapp rich sends: using the defaults", zap.Error(err))
	}
	return cfg
}

// whatsAppTemplateConfig reads the utility template names for outside the window (MF_WA_TPL_*).
func whatsAppTemplateConfig(logger *zap.Logger) creative.WhatsAppTemplateConfig {
	cfg, err := creative.WhatsAppTemplateConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Warn("whatsapp templates: using the defaults", zap.Error(err))
	}
	return cfg
}

func tenantExportHandler(db *sql.DB, internalKey string) http.Handler {
	if internalKey == "" {
		return nil
	}
	return tenantexport.Handler(db, tenantexport.Options{Service: "marketflow", InternalKey: internalKey})
}

func New(ctx context.Context, cfg *config.Config, logger *zap.Logger) (*App, error) {
	// ── Database ──────────────────────────────────────────────────────────
	entClient, sqlDB, err := database.NewClient(ctx, cfg.Database)
	if err != nil {
		return nil, err
	}
	// Schema migrations run once per rollout in marketflow-migrate (entrypoint, advisory-locked,
	// direct DSN). The in-app copy (POSTGRES_RUN_MIGRATIONS, default true) is gone: any pod
	// started without that variable would have migrated unlocked from every replica.

	// ── Redis ─────────────────────────────────────────────────────────────
	redisClient, err := sharedcache.NewRedis(ctx, sharedcache.RedisConfig{
		Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB, TLS: cfg.Redis.EnableTLS,
	})
	if err != nil {
		return nil, err
	}
	// Scheduled work runs once per period fleet-wide (sharedcache.ClaimPeriod).
	sharedcache.SetLeaseClient(redisClient)

	// ── NATS Event Publishing (optional) ──────────────────────────────────
	var natsConn *nats.Conn
	var outboxPub *eventslib.OutboxPoller
	if cfg.Events.Enabled {
		natsConn, err = platformevents.Connect(cfg.Events)
		if err != nil {
			return nil, fmt.Errorf("connect to NATS: %w", err)
		}
		logger.Info("NATS connection established", zap.String("url", cfg.Events.NATSURL))
		// Drop revoked/rotated API keys from every validator on this pod at once.
		_ = eventslib.NewBroadcaster(logger, natsConn, "auth").Subscribe("apikey.changed", func(m eventslib.BroadcastMessage) {
			authclient.InvalidateAPIKeyHash(string(m.Data))
		})

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
		// Opt-outs handled by notifications-api (unsubscribe link, STOP reply, quick reply) clear the
		// contact's consent flag here so MarketFlow's own campaigns honour them too.
		if err := platformevents.NewUnsubscribeConsumer(entClient, logger).Start(natsConn); err != nil {
			logger.Warn("app: failed to start unsubscribe consumer", zap.Error(err))
		}

		tenantPurgeConsumer = platformevents.NewTenantPurgeConsumer(sqlDB, logger)
		if err := tenantPurgeConsumer.Start(natsConn); err != nil {
			logger.Warn("app: failed to start tenant purge consumer", zap.Error(err))
		}
	}

	// ── Platform Provider Admin Handler ────────────────────────────────────
	platformProviderHandler := admin.NewPlatformProviderHandler(entClient, keyProvider, logger).
		WithBalances(redisClient, cfg.AI.ProviderBalancesKey)
	encryptionKeyHandler := admin.NewEncryptionKeyHandler(entClient, keyProvider, logger)
	endpointStore := endpoints.EntStore{DB: entClient}
	scanServices, err := endpoints.ServicesFromConfig(cfg.EndpointScan.Services)
	if err != nil {
		return nil, err
	}
	endpointHandler := &endpoints.Handler{Store: endpointStore, Logger: logger,
		Scanner: endpoints.NewScanner(endpointStore, scanServices, cfg.EndpointScan.Key(cfg.CrossService.InternalServiceKey), logger)}
	adminTenantsHandler := admin.NewTenantsHandler(entClient, logger)
	// The cost report prices USD costs with treasury's platform rate (shared cache), falling back
	// to the output registry's fx_rate when treasury has none.
	costReportHandler := admin.NewCostReportHandler(entClient, sqlDB,
		fxrate.New(cfg.CrossService.TreasuryURL, cfg.CrossService.InternalServiceKey, cfg.Security.PlatformTenant,
			sharedcache.New(redisClient, logger)).KESPer, logger).
		// The platform owner's own spend (no credits) is shown apart.
		WithPlatformSlug(cfg.Security.PlatformTenant)
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
	segmentService := segments.NewService(entClient, logger)
	contactHandler := contacts.NewHandler(contactService, logger,
		contacts.WithSegmentFilter(segmentService.MemberPredicates),
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

	campaignService := campaigns.NewService(entClient, logger).WithBroadcaster(notifications.New(notifications.Config{BaseURL: cfg.CrossService.NotificationsURL, APIKey: cfg.CrossService.InternalServiceKey}))
	campaignHandler := campaigns.NewHandler(campaignService, logger)

	funnelService := funnels.NewService(entClient, logger)
	funnelHandler := funnels.NewHandler(funnelService, logger)
	funnelAnalyticsHandler := funnels.NewAnalyticsHandler(funnelService, redisClient, entClient, logger)

	sequenceService := sequences.NewService(entClient, logger)
	sequenceHandler := sequences.NewHandler(sequenceService, logger)

	meetingService := meetings.NewService(entClient, gcal.NewSyncer(entClient, keyProvider, logger), logger)
	// OWASP M2: daily caps on invites sent to outside addresses (public booking page and the AI's
	// book_meeting tool), shared by both entry points.
	bookingGuard := meetings.NewBookingGuard(ratelimit.NewQuota(redisClient), meetings.BookingCaps{
		PerTenantDay: cfg.Security.BookingsPerTenantDay,
		PerEmailDay:  cfg.Security.BookingsPerEmailDay,
		PerIPDay:     cfg.Security.BookingsPerIPDay,
	}, logger)
	meetingHandler := meetings.NewHandler(meetingService, logger).WithBookingGuard(bookingGuard)
	availabilitySvc := gcal.NewAvailabilityService(entClient, keyProvider, logger)

	analyticsHandler := analytics.NewHandler(entClient, logger).WithCache(sharedcache.New(redisClient, logger))
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

	// Ad autopilot veto window (Sprint 20, owner 2026-11-14): the plan feature is read strictly
	// (fail closed, never the demo bypass), together with the tenant's acceptance of the terms.
	custom_agents.SetDefaultVeto(cfg.Autopilot.DefaultVetoHours)
	strictFeature := func(ctx context.Context, tenantID, feature string) bool {
		return subsClient.StrictHasFeature(ctx, tenantID, feature)
	}
	// The pipeline is built further down with the Studio; an agent switched off, set back to
	// approve-each or deleted has its waiting veto-window posts cancelled through it at once.
	var autopilotPipeline *autopublish.Pipeline
	// The Autopilot terms are auth-api's versioned legal document (owner 2026-11-14): one cached
	// client for the settings page and the pipeline. A typed nil must not become a non-nil interface.
	var autopilotTerms custom_agents.TermsSource
	if c := cfg.Autopilot.TermsSource(cfg.Security.AuthServiceURL, cfg.CrossService.InternalServiceKey); c != nil {
		autopilotTerms = c
	}
	customAgentServiceOpts := []func(*custom_agents.Service){custom_agents.WithPlatformTenant(cfg.Security.PlatformTenant),
		custom_agents.WithCreditForecaster(custom_agents.NewCreditForecaster(cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey)),
		custom_agents.WithAutoPublishGate(strictFeature, cfg.Autopilot.TermsVersion),
		custom_agents.WithTermsEffectiveAt(cfg.Autopilot.TermsEffective()),
		custom_agents.WithTermsSource(autopilotTerms, cfg.Autopilot.TermsPageURL),
		custom_agents.WithFeatureCheck(subsClient.StrictFeature),
		// Autopilot agents (owner 2026-11-14): marketflow_autopilot and max_autopilot_agents, read
		// strictly; the platform tenant is exempt.
		custom_agents.WithAutopilotPlan(subsClient.StrictFeature, subsClient.StrictLimit),
		custom_agents.WithAgentChanged(func(ctx context.Context, tenantID, agentID uuid.UUID) {
			if autopilotPipeline != nil {
				autopilotPipeline.AgentChanged(ctx, tenantID, agentID)
			}
		})}
	if natsConn != nil {
		customAgentServiceOpts = append(customAgentServiceOpts, custom_agents.WithNATS(natsConn))
	}
	customAgentService := custom_agents.NewService(entClient, logger, customAgentServiceOpts...)
	customAgentHandler := custom_agents.NewHandler(customAgentService, logger)
	// Choosing the veto window and accepting its terms is for the roles that approve ads.
	customAgentHandler.CanApprove = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, creative.ApprovalRoles...)
	}
	crawlerReader := marketingos.NewCrawlerReader(cfg.CrossService.CrawlerURL, cfg.CrossService.InternalServiceKey)
	marketingOSService := marketingos.NewService(entClient, subsClient, logger).
		// Team setup reads the website and online store from auth-api, their owner, read only.
		WithWebPresence(func(ctx context.Context, tenantID uuid.UUID) (bool, bool) {
			t, err := entClient.Tenant.Get(ctx, tenantID)
			if err != nil || tenantSyncer == nil {
				return false, false
			}
			p, err := tenantSyncer.AuthProfile(ctx, t.Slug)
			if err != nil || p == nil {
				return false, false
			}
			return strings.TrimSpace(p.Website) != "" || strings.TrimSpace(p.Storefront) != "", true
		}).
		// The chat's business facts read the account record from auth-api (cached 10 minutes).
		WithAccountFacts(func(ctx context.Context, slug string) (marketingos.AccountFacts, error) {
			if tenantSyncer == nil {
				return marketingos.AccountFacts{}, fmt.Errorf("auth-api is not configured")
			}
			p, err := tenantSyncer.AuthProfile(ctx, slug)
			if err != nil || p == nil {
				return marketingos.AccountFacts{}, fmt.Errorf("account facts: %w", err)
			}
			return marketingos.AccountFacts{Name: p.Name, Phone: p.ContactPhone, Email: p.ContactEmail, Website: p.Website, Storefront: p.Storefront,
				Address: p.Address, City: p.City}, nil
		}).
		// Sites that add their address and hours in the browser are read through the crawler.
		WithPageReader(crawlerReader).
		// Only the platform owner's tenant gets every plan level in the AI work (not demo tenants).
		WithPlatformTenant(cfg.Security.PlatformTenant).
		// A demo tenant follows the free plan's features and chat limit (its password is public).
		WithDemoPlan(cfg.Security.DemoPlanCode)
	marketingOSHandler := marketingos.NewHandler(marketingOSService, logger)
	marketingOSHandler.Complaints = marketingos.NewComplaintFinder(cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey)
	marketingOSHandler.ScanQuota = marketingos.NewScanQuotaReader(cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey)
	learningHandler := learning.NewHandler(learning.NewStore(entClient), logger)
	marketingOSHandler.CanManage = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, creative.ApprovalRoles...)
	}
	// One cached reader of codevertex-website's catalogue and courses serves both the
	// onboarding draft and the platform tenant's offering picker.
	websiteCatalog := onboarding.NewWebsiteCatalog(cfg.CrossService.WebsiteURL, sharedcache.New(redisClient, logger))
	catalogueOfferings := creative.NewOfferings(cfg.CrossService.InventoryURL, cfg.CrossService.InternalServiceKey, logger).
		WithPromotions(cfg.CrossService.POSURL).
		WithSource(onboarding.WebsiteOfferings(entClient, websiteCatalog, cfg.Security.PlatformTenant))
	// Team setup also sees what the business sells in its own systems, not only the knowledge
	// base: inventory, POS promotions and, for the platform tenant, the website catalogue.
	marketingOSService.WithCatalogue(func(ctx context.Context, tenantID uuid.UUID) bool {
		list, err := catalogueOfferings.Search(ctx, tenantID, "", 1)
		return err == nil && len(list) > 0
	})
	// Onboarding: setup checklist and a brand context draft from the business's own data
	// (account details, best sellers, website catalogue for the platform tenant).
	// Brand images (WP3, owner 2026-11-12): private objects under the tenant's prefix, the same
	// shared object storage configuration as the worker (OBJECT_STORAGE_*).
	brandImagesCfg, err := brandimages.ConfigFromEnv(os.Getenv)
	if err != nil {
		logger.Warn("brand image limits: using the defaults", zap.Error(err))
	}
	mediaStore, err := objectstore.New("marketflow", objectstore.FromEnv())
	if err != nil {
		logger.Warn("object storage unavailable; brand image uploads off", zap.Error(err))
	}
	brandImagesSvc := brandimages.NewService(entClient, mediaStore, brandImagesCfg, logger)
	// Website pictures first (owner, 2026-11-14): a business that already shows itself on its own
	// site (auth-api website and storefront) gets those pictures linked; it uploads only the rest.
	if tenantSyncer != nil {
		brandImagesSvc.WithSiteImport(func(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
			t, err := entClient.Tenant.Get(ctx, tenantID)
			if err != nil {
				return nil, err
			}
			p, err := tenantSyncer.AuthProfile(ctx, t.Slug)
			if err != nil || p == nil {
				return nil, err
			}
			var sites []string
			for _, s := range []string{p.Website, p.Storefront} {
				if s = strings.TrimSpace(s); s != "" {
					sites = append(sites, s)
				}
			}
			return sites, nil
		})
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				if sharedcache.ClaimPeriod(ctx, "marketflow:brand-images-site", brandImagesCfg.SiteSweepEvery) {
					brandImagesSvc.SweepSiteImports(ctx, brandImagesCfg.SiteRetryAfter, brandImagesCfg.SiteSweepLimit)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	brandImagesHandler := brandimages.NewHandler(brandImagesSvc, logger)
	brandLinks := brandimages.NewLinker(mediaStore, brandImagesCfg.RenderLinkTTL)
	onboardingHandler := onboarding.NewHandler(onboarding.NewService(entClient, marketingOSService, tenantSyncer,
		creative.NewTopOfferings(catalogueOfferings, cfg.CrossService.POSURL, cfg.CrossService.InternalServiceKey, logger),
		websiteCatalog, cfg.Security.PlatformTenant, logger).WithBrandImagesMin(brandImagesCfg.Min).WithPlanStatus(subsClient.PlanStatus).
		// Seed defaults (platform owner): marketingos stays the only brand context writer, and the
		// knowledge base keeps drafted answers as pending suggestions.
		WithSeed(onboarding.SeedDeps{
			SaveBrand:     marketingOSService.SaveBrandContext,
			Facts:         marketingOSService.BusinessFacts,
			KBTitles:      kbService.TenantTitles,
			DraftFAQ:      kbService.DraftFAQ,
			TeamSetupDone: marketingOSService.TeamSetupDone,
			SetupTeam:     marketingOSService.SetupTeamIfNew,
		}), logger)
	// Ad autopilot veto window: a job report settled here carries the agent's video on at once
	// (the worker's sweep catches anything missed). It works through its own Studio service with
	// the daily render quota, and quotes renders through marketflow-ai like the Studio.
	pubStore := &publishing.EntStore{DB: entClient, Keys: keyProvider}
	autopilotPipeline = autopublish.New(autopublish.Deps{DB: entClient,
		Studio:  creative.NewService(entClient, logger).WithQuota(ratelimit.NewQuota(redisClient)).WithFeatureCheck(consumerFeatureGate),
		Quoter: creative.NewRenderer(entClient, cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey, logger).WithBrandImages(brandLinks).
			WithFacts(marketingOSService.BusinessFacts, crawlerReader),
		Publish: pubStore,
		Gate:    custom_agents.AutoPublishGate{DB: entClient, HasFeature: strictFeature, TermsVersion: cfg.Autopilot.TermsVersion, TermsEffectiveAt: cfg.Autopilot.TermsEffective(), CheckFeature: subsClient.StrictFeature, Terms: autopilotTerms, TermsPageURL: cfg.Autopilot.TermsPageURL},
		Notif:   notifications.New(notifications.Config{BaseURL: cfg.CrossService.NotificationsURL, APIKey: cfg.CrossService.InternalServiceKey}),
		Links:   autopublish.NewVetoLinks(encryption.KeyFromEnv(cfg.Security.EncryptionKey)),
		AppURL:  cfg.CrossService.PortalURL, Config: cfg.Autopilot, Logger: logger})
	// Review links are signed with the env encryption key (stable, like the OAuth
	// state), separated from it by the token's own HMAC domain.
	creativeHandler := creative.NewHandler(creative.NewService(entClient, logger).WithQuota(ratelimit.NewQuota(redisClient)).
		// A version deleted permanently loses its stored files at once (delete_version.go).
		WithFiles(creativeFiles(mediaStore)).
		WithLeadFunnels(funnelService.ForAds()).WithBrandImagesMin(brandImagesCfg.Min).
		// Gate G0: a video is made only when the plan includes that kind of video (plan_gate.go).
		WithFeatureCheck(consumerFeatureGate),
		creative.NewReviews(entClient, encryption.KeyFromEnv(cfg.Security.EncryptionKey)),
		catalogueOfferings, logger).
		WithApprover(func(ctx context.Context, tenantID uuid.UUID) bool {
			return rbacService.CallerHasAnyRole(ctx, tenantID, creative.ApprovalRoles...)
		}).
		// marketflow-ai's job callback settles motion posters and video ads here;
		// the worker's renderer starts them and polls only when a push is overdue.
		// The API's renderer still needs marketflow-ai's URL and key for the Studio's direct calls
		// (video quote, storyboard, ad templates): an empty URL made each of those fail with
		// "unsupported protocol scheme" (fixed 2026-11-19). Run is never started here, so this
		// instance does not poll the queue.
		WithJobs(creative.NewRenderer(entClient, cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey, logger).WithBrandImages(brandLinks).
			// The address, contacts and website pages for the setting brief and the end card (place.go).
			WithFacts(marketingOSService.BusinessFacts, crawlerReader).
			// A video made from a WhatsApp turn: marketflow-ai writes the storyboard and finished-ad
			// messages (with the render code); they go out from the platform number.
			WithOSTurn(creative.NewWhatsAppTurnNotifier(cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey,
				creative.NewPlatformWhatsApp(entClient,
					notifications.New(notifications.Config{BaseURL: cfg.CrossService.NotificationsURL, APIKey: cfg.CrossService.InternalServiceKey}),
					cfg.Security.PlatformTenant, "marketflow-api").
					// The storyboard link, the finished ad as a video and the render buttons, while
					// the requester's 24-hour window is open (the worker marks it on each message).
					WithRich(redisClient, whatsAppRichConfig(logger)).
					// Outside the window: the approved mf_video_ready template, or nothing.
					WithTemplates(whatsAppTemplateConfig(logger), logger), logger)).
			WithAutopilot(autopilotPipeline))
	// Boost, post and reply are also open to the Meta App Review account (owner 2026-11-12); the
	// role gate keeps it to exactly those routes. Approving an ad stays with the approving roles.
	reviewActionRoles := rbac.WithAppReviewer(creative.ApprovalRoles)
	// Paid ads (Sprint 18): off until the platform ads/meta_enabled switch; the approving roles
	// launch and pause; launch credits go through marketflow-ai's charge route.
	adsHandler := &adsmanager.Handler{DB: entClient, Keys: keyProvider,
		Meta: meta.NewClient("").WithSecret(meta.PlatformSecret(entClient, keyProvider, cfg.Meta.AppSecret)),
		// Strict: paid_ads_launch waives the launch charge, so an outage must not grant it.
		HasFeature: func(ctx context.Context, tenantID, feature string) bool {
			return subsClient.StrictHasFeature(ctx, tenantID, feature)
		},
		Charge:        adsmanager.AICharge(cfg.CrossService.AIURL, cfg.CrossService.InternalServiceKey),
		LaunchCredits: adsmanager.DefaultLaunchCredits, Enabled: adsmanager.MetaEnabled(entClient), Logger: logger.Named("ads"),
		WABoost: adsmanager.WABoostDefaults{BudgetKES: cfg.Meta.WABoostBudgetKES, Days: cfg.Meta.WABoostDays, Objective: cfg.Meta.WABoostObjective},
		CanLaunch: func(ctx context.Context, tenantID uuid.UUID) bool {
			return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
		},
		// Audit M1: a boost at or above the tenant's threshold needs a WhatsApp code sent to the
		// launcher's verified number from the platform number.
		Codes: &adsmanager.BoostCodes{
			Store: adsmanager.RedisOTPStore{RDB: redisClient},
			Send: creative.NewPlatformWhatsApp(entClient,
				notifications.New(notifications.Config{BaseURL: cfg.CrossService.NotificationsURL, APIKey: cfg.CrossService.InternalServiceKey}),
				cfg.Security.PlatformTenant, "marketflow-api").SendOTP,
			Phone: adsmanager.VerifiedPhone(redisClient, cfg.Security.AuthServiceURL, cfg.CrossService.InternalServiceKey),
		},
		// A non-KES boost is sized in KES with treasury's platform rate (shared cache); no rate
		// means the code is required.
		KESPer: fxrate.New(cfg.CrossService.TreasuryURL, cfg.CrossService.InternalServiceKey, cfg.Security.PlatformTenant,
			sharedcache.New(redisClient, logger)).KESPer,
	}
	// Google Ads (owner 2026-11-19): the business's own account results and conversion mapping;
	// the approving roles change the mapping.
	googleAdsHandler := googleads.New(entClient, keyProvider, logger.Named("google-ads"))
	googleAdsHandler.CanManage = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
	}
	// Social inbox (Sprint 22): the approving roles may reply for the business.
	inboxHandler := inbox.NewHandler(entClient, keyProvider, leadService, inbox.NewGraph(meta.GraphURL).WithProof(meta.PlatformSecret(entClient, keyProvider, cfg.Meta.AppSecret)), logger)
	inboxHandler.CanReply = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
	}
	// Hiding or deleting a comment on the business's own post: members and above, plus the reviewer.
	inboxHandler.CanModerate = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
	}
	// Owner 2026-11-10: approved videos are removed 48 hours after they are posted or downloaded;
	// the admins are emailed the link first.
	afterUse := &creative.AfterUse{DB: entClient, Logger: logger, Notify: creative.EmailAfterUse(entClient,
		notifications.New(notifications.Config{BaseURL: cfg.CrossService.NotificationsURL, APIKey: cfg.CrossService.InternalServiceKey}),
		cfg.CrossService.PortalURL, logger)}
	creativeHandler.AfterUse = afterUse
	autopilotPipeline.AfterUse = afterUse
	// The agents' posts in their veto window, the signed-in veto (the approving roles) and the
	// emailed cancel link.
	autopublishHandler := autopublish.NewHandler(autopilotPipeline, logger)
	autopublishHandler.CanApprove = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, creative.ApprovalRoles...)
	}
	// Organic publishing: the same roles that approve an ad may post it or cancel a post.
	publishingHandler := publishing.NewHandler(&publishing.EntStore{DB: entClient, Keys: keyProvider}, logger)
	publishingHandler.CanPost = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
	}
	publishingHandler.OnJobDone = afterUse.CheckJob
	// Approve and post from WhatsApp (Sprint 17): marketflow-ai redeems the code, this route does
	// the Studio's approve and organic post with the same checks; never paid ads.
	waApprover := creative.NewService(entClient, logger)
	publishingHandler.WhatsApp = publishing.WhatsAppPublishDeps{
		IsApprover: func(ctx context.Context, tenantID, authUserID uuid.UUID) bool {
			return rbac.AuthUserHasAnyRole(ctx, entClient, tenantID, authUserID, creative.ApprovalRoles)
		},
		NeedsReview: creative.FlaggedForReview,
		Approve: func(ctx context.Context, tenantID, versionID, by uuid.UUID) error {
			_, err := waApprover.ApproveFromWhatsApp(ctx, tenantID, versionID, by)
			if errors.Is(err, creative.ErrPPBApprovalRequired) {
				return fmt.Errorf("%w: %w", publishing.ErrPPBHold, err)
			}
			return err
		},
	}
	// Delete from platform (owner 2026-11-14): the approving roles (owner, admin) plus the app
	// reviewer, who must show instagram_manage_contents in App Review; members cannot. Only where
	// the platform's API allows it (TikTok has no delete, so it is left out and answers with its
	// manual steps).
	publishingHandler.CanDelete = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, reviewActionRoles...)
	}
	{
		pubSecret := meta.PlatformSecret(entClient, keyProvider, cfg.Meta.AppSecret)
		deleters := []publishing.Deleter{
			publishing.NewFacebookReels(meta.GraphURL).WithProof(pubSecret),
			publishing.NewInstagramReels(meta.GraphURL).WithProof(pubSecret),
			publishing.NewYouTubeShorts(publishing.YouTubeUploadBase, publishing.YouTubeAPIBase, ""),
		}
		publishingHandler.Deleters = make(map[string]publishing.Deleter, len(deleters))
		for _, d := range deleters {
			publishingHandler.Deleters[d.Platform()] = d
		}
	}

	segmentHandler := segments.NewHandler(segmentService, logger)

	// ── Subscription usage + feature gate ─────────────────────────────
	usagePub := mfmiddleware.NewUsagePublisher(cfg.Subscriptions.URL, cfg.Subscriptions.APIKey, logger)
	featureGate := mfmiddleware.NewFeatureGate(cfg.Subscriptions.URL, cfg.Subscriptions.APIKey, logger)

	// Integrations OAuth flow handler. Uses the env encryption key as the HMAC
	// key for signing `state`; no separate secret needed. This is the OAuth
	// state-signing key (must stay stable across the redirect round-trip) and is
	// deliberately kept on the env value rather than the rotatable DB key.
	integrationsHandler := integrations.NewHandler(entClient, encryption.KeyFromEnv(cfg.Security.EncryptionKey), keyProvider, logger).
		WithPortal(cfg.CrossService.PortalURL).
		WithMeta(cfg.Meta.AppSecret, cfg.Meta.DeletionStatusURL).
		// OWASP M1: connect states are single use and bound to the starting browser.
		WithStateStore(redisClient, cfg.Redis.Namespace, cfg.Security.OAuthBrowserBinding)

	// Webhook handlers — Meta app credentials are platform-level (MF_META_*),
	// shared across all tenants; per-tenant link is Tenant.meta_page_id.
	metaWebhookHandler := webhooks.NewMetaHandler(leadService, webhooks.MetaConfig{
		DB:            sqlDB,
		AppSecret:     cfg.Meta.AppSecret,
		VerifyToken:   cfg.Meta.VerifyToken,
		AllowUnsigned: cfg.App.Environment == "development",
		Secret:        meta.PlatformSecret(entClient, keyProvider, cfg.Meta.AppSecret),
		Inbox:         &webhooks.InboxStore{DB: entClient, Logger: logger.Named("webhooks.inbox")},
	}, logger)
	tiktokWebhookHandler := webhooks.NewTikTokHandler(leadService, webhooks.TikTokConfig{
		DB:            sqlDB,
		SharedSecret:  cfg.TikTok.WebhookSecret,
		SigningSecret: cfg.TikTok.SigningSecret,
		AllowUnsigned: cfg.App.Environment == "development",
	}, logger)

	// Testimonial CRUD + public reader for /p/{slug}
	testimonialHandler := testimonials.NewHandler(entClient, logger)
	tenantProfileHandler := tenant.NewHandler(entClient, logger)
	// One cache for the public profile reads and the profile saves that drop them (Sprint 7 #58).
	publicProfileCache := sharedcache.New(redisClient, logger)
	tenantProfileHandler.ProfileCache = publicProfileCache
	// Owner 2026-11-14: only the approving roles change the AI label.
	tenantProfileHandler.CanApprove = func(ctx context.Context, tenantID uuid.UUID) bool {
		return rbacService.CallerHasAnyRole(ctx, tenantID, creative.ApprovalRoles...)
	}
	tenantMembersHandler := tenant.NewMembersHandler(rbacService, entClient, cfg.Security.AuthServiceURL, cfg.CrossService.InternalServiceKey, logger)
	// Register MarketFlow's own SSO roles (marketflow_admin, marketflow_member) so invites can
	// grant them; in the background, a failure only means the next start tries again.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := rbac.PushRolesToAuth(ctx, cfg.Security.AuthServiceURL, cfg.CrossService.InternalServiceKey); err != nil {
			logger.Warn("marketflow SSO roles not registered", zap.Error(err))
		}
	}()

	// Public handlers
	publicHandler := webhooks.NewPublicHandler(entClient, funnelService, leadService, meetingService, availabilitySvc, testimonialHandler, tenantSyncer, logger).
		// White label hides "Powered by Codevertex" (Business plan); fail closed, never the demo bypass.
		WithFeatureCheck(subsClient.StrictHasFeature).
		WithCache(publicProfileCache).
		WithBookingGuard(bookingGuard)

	// Tenant backups are made from the SSO client area (auth-api collects this service's
	// /api/v1/s2s/tenant-export); the platform pg_dumpall covers disaster recovery.

	// ── Router ────────────────────────────────────────────────────────────
	var requireAuth func(http.Handler) http.Handler
	if authMiddleware != nil {
		requireAuth = authMiddleware.RequireAuth
	}

	router := httpapi.NewRouter(httpapi.RouterDeps{
		Limiter: ratelimit.NewLimiter(redisClient, logger, "marketflow"),
		PublicLimits: httpapi.PublicLimits{Leads: cfg.Security.PublicLeadsPerMinute,
			Bookings: cfg.Security.PublicBookingsPerMinute, Events: cfg.Security.PublicEventsPerMinute,
			Reads: cfg.Security.PublicReadsPerMinute, Webhooks: cfg.Security.WebhooksPerMinute},
		Log:            logger,
		HealthHandler:  handlers.Health,
		MetricsHandler: promhttp.Handler(),
		TenantExport:   tenantExportHandler(sqlDB, cfg.CrossService.InternalServiceKey),
		LeadHandlers: httpapi.LeadHandlers{
			Create: leadHandler.Create,
			List:   leadHandler.List,
			Get:    leadHandler.Get,
			Update: leadHandler.Update,
			Delete: leadHandler.Delete,
			Export: leadHandler.Export,
		},
		ContactHandlers: httpapi.ContactHandlers{
			Create:         contactHandler.Create,
			List:           contactHandler.List,
			Get:            contactHandler.Get,
			Update:         contactHandler.Update,
			Delete:         contactHandler.Delete,
			Get360:         contactHandler.Get360,
			GetByEmail:     contactHandler.GetByEmail,
			GetByPhone:     contactHandler.GetByPhone,
			UpsertByPhone:  contactHandler.UpsertByPhone,
			SearchInternal: contactHandler.SearchInternal,
			ReachInternal:  contactHandler.ReachInternal,
			BulkImport:     contactHandler.BulkImport,
			Merge:          contactHandler.Merge,
			UpdateInternal: contactHandler.UpdateInternal,
			DeleteInternal: contactHandler.DeleteInternal,
			GetInternal:    contactHandler.InternalGet,
			BatchInternal:  contactHandler.InternalBatch,
		},
		KnowledgeHandlers: httpapi.KnowledgeHandlers{
			CreateEntry: kbHandler.CreateEntry,
			ListEntries: kbHandler.ListEntries,
			GetEntry:    kbHandler.GetEntry,
			UpdateEntry: kbHandler.UpdateEntry,
			DeleteEntry: kbHandler.DeleteEntry,
			ImportCSV:   kbHandler.ImportCSV,

			ApproveSuggestion:  kbHandler.ApproveSuggestion,
			RejectSuggestion:   kbHandler.RejectSuggestion,
			InternalSuggestFAQ: kbHandler.InternalSuggestFAQ,
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
			Purge:     funnelHandler.Purge,
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
			GetAvailability:    publicHandler.GetAvailability,
			CreateBooking:      publicHandler.CreateBooking,
		},
		TenantSyncer: tenantSyncer,
		TenantProfileHandlers: httpapi.TenantProfileHandlers{
			Get:    tenantProfileHandler.GetProfile,
			Update: tenantProfileHandler.UpdateProfile,
			// The business types an owner can pick, with descriptions (Sprint 7 #32).
			BusinessTypes: tenantProfileHandler.ListBusinessTypes,
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
			ROI:             analyticsHandler.ROI,
			Report:          analyticsHandler.Report,
		},
		IntegrationsHandlers: httpapi.IntegrationsHandlers{
			AvailableIntegrations:             integrationsHandler.AvailableIntegrations,
			GoogleAuthorize:                   integrationsHandler.GoogleAuthorize,
			GoogleCallback:                    integrationsHandler.GoogleCallback,
			UpdateGoogleCalendarBusinessHours: integrationsHandler.UpdateGoogleCalendarBusinessHours,
			MetaAuthorize:                     integrationsHandler.MetaAuthorize,
			MetaCallback:                      integrationsHandler.MetaCallback,
			MetaDataDeletion:                  integrationsHandler.MetaDataDeletion,
			MetaDeauthorize:                   integrationsHandler.MetaDeauthorize,
			MetaDeletionStatus:                integrationsHandler.MetaDeletionStatus,
			TikTokAuthorize:                   integrationsHandler.TikTokAuthorize,
			TikTokCallback:                    integrationsHandler.TikTokCallback,
			TikTokCreatorAuthorize:            integrationsHandler.TikTokCreatorAuthorize,
			TikTokCreatorCallback:             integrationsHandler.TikTokCreatorCallback,
			YouTubeAuthorize:                  integrationsHandler.YouTubeAuthorize,
			YouTubeCallback:                   integrationsHandler.YouTubeCallback,
			GoogleAdsAuthorize:                integrationsHandler.GoogleAdsAuthorize,
			GoogleAdsCallback:                 integrationsHandler.GoogleAdsCallback,
			ListTenant:                        integrationsHandler.ListTenant,
			DisconnectTenant:                  integrationsHandler.DisconnectTenant,
		},
		AdminTenantsHandlers: httpapi.AdminTenantsHandlers{
			List: adminTenantsHandler.List,
		},
		AdminCostReport: costReportHandler.CostReport,
		AdminProviderHandlers: httpapi.AdminProviderHandlers{
			Configure:   platformProviderHandler.ConfigureProvider,
			List:        platformProviderHandler.ListProviders,
			GetSettings: platformProviderHandler.GetProviderSettings,
			Resolve:     platformProviderHandler.ResolveProvider,
			SyncModels:  platformProviderHandler.SyncModels,
			Test:        platformProviderHandler.TestProvider,
			Deactivate:  platformProviderHandler.DeactivateProvider,
			Defaults:    platformProviderHandler.ListProviderDefaults,
			SetDefault:  platformProviderHandler.SetProviderDefault,
			Balances:    platformProviderHandler.ProviderBalances,
			Billing:     platformProviderHandler.ProviderBilling,
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
		EndpointHandlers: httpapi.EndpointHandlers{
			List: endpointHandler.List, Decide: endpointHandler.Decide, Scan: endpointHandler.Scan, InternalTools: endpointHandler.InternalTools,
		},
		InternalLeadHandlers: httpapi.InternalLeadHandlers{
			IngestFromService: leadHandler.IngestFromService,
			List:              leadHandler.ListInternal,
			Stats:             leadHandler.StatsInternal,
			Get:               leadHandler.GetInternal,
			Patch:             leadHandler.PatchInternal,
			Delete:            leadHandler.DeleteInternal,
			Import:            leadHandler.ImportInternal,
		},
		InternalServiceKey: cfg.Subscriptions.APIKey,
		ResolveTenantSlug: func(ctx context.Context, slug string) (uuid.UUID, error) {
			t, err := entClient.Tenant.Query().Where(enttenant.SlugEQ(slug), enttenant.StatusEQ("active")).Only(ctx)
			if err != nil {
				return uuid.Nil, err
			}
			return t.ID, nil
		},
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
			Create:            customAgentHandler.Create,
			List:              customAgentHandler.List,
			Get:               customAgentHandler.Get,
			Update:            customAgentHandler.Update,
			Delete:            customAgentHandler.Delete,
			ListRuns:          customAgentHandler.ListRuns,
			TriggerRun:        customAgentHandler.TriggerRun,
			Templates:         customAgentHandler.Templates,
			HireTemplate:      customAgentHandler.HireTemplate,
			PutAutopilot:      customAgentHandler.PutAutopilot,
			RollbackAutopilot: customAgentHandler.RollbackAutopilot,
			DryRunAutopilot:   customAgentHandler.DryRunAutopilot,
			GetAutoPublish:    customAgentHandler.GetAutoPublish,
			GetAutopilotPlan:  customAgentHandler.GetAutopilotPlan,
			PutAutoPublish:    customAgentHandler.PutAutoPublish,
			AutopilotPosts:    autopublishHandler.Posts,
			AutopilotVeto:     autopublishHandler.Veto,
			AutopilotVetoJob:  autopublishHandler.VetoJob,
			PublicVetoView:    autopublishHandler.PublicView,
			PublicVetoPost:    autopublishHandler.PublicVeto,

			// The autopilot dashboard: drafts waiting for the owner and the last 7 days (Sprint 20).
			AutopilotDashboard: customAgentHandler.GetAutopilotDashboard,
		},
		OnboardingHandlers: httpapi.OnboardingHandlers{
			Checklist: onboardingHandler.Checklist,
			Dismiss:   onboardingHandler.Dismiss,
			Draft:     onboardingHandler.Draft,
			MarkStep:  onboardingHandler.MarkStep,
			// Platform owner: fill empty defaults from each tenant's own sources (seed.go).
			SeedTenant: onboardingHandler.SeedTenant,
			SeedAll:    onboardingHandler.SeedAll,
		},
		BrandImageHandlers: httpapi.BrandImageHandlers{
			List:       brandImagesHandler.List,
			Upload:     brandImagesHandler.Upload,
			Delete:     brandImagesHandler.Delete,
			ImportSite: brandImagesHandler.ImportSite,
		},
		MarketingOSHandlers: httpapi.MarketingOSHandlers{
			GetBrandContext:   marketingOSHandler.GetBrandContext,
			PutBrandContext:   marketingOSHandler.PutBrandContext,
			ListTeam:          marketingOSHandler.ListTeam,
			PutTeamMember:     marketingOSHandler.PutTeamMember,
			GetSettings:       marketingOSHandler.GetSettings,
			PutSettings:       marketingOSHandler.PutSettings,
			InternalContext:   marketingOSHandler.InternalContext,
			BusinessFacts:     marketingOSHandler.InternalBusinessFacts,
			SetupTeam:         marketingOSHandler.SetupTeam,
			InternalSetupTeam: marketingOSHandler.InternalSetupTeam,
			FindComplaints:    marketingOSHandler.FindComplaints,
			GetResearch:       marketingOSHandler.GetResearch,
		},
		LearningHandlers: httpapi.LearningHandlers{
			Phrases: learningHandler.Phrases,
			Record:  learningHandler.Record,
		},
		CreativeHandlers: httpapi.CreativeHandlers{
			Downloaded:        creativeHandler.Downloaded,
			DownloadFile:      creativeHandler.DownloadFile,
			ListBriefs:        creativeHandler.ListBriefs,
			SearchOfferings:   creativeHandler.SearchOfferings,
			AdTemplates:       creativeHandler.AdTemplates,
			CreateBrief:       creativeHandler.CreateBrief,
			GetBrief:          creativeHandler.GetBrief,
			UpdateBrief:       creativeHandler.UpdateBrief,
			LeadPage:          creativeHandler.LeadPage,
			CreateVersion:     creativeHandler.CreateVersion,
			GetVersion:        creativeHandler.GetVersion,
			Approve:           creativeHandler.Approve,
			Discard:           creativeHandler.Discard,
			DeleteBrief:       creativeHandler.DeleteBrief,
			DeleteVersion:     creativeHandler.DeleteVersion,
			RestoreVersion:    creativeHandler.RestoreVersion,
			Revise:            creativeHandler.Revise,
			Extend:            creativeHandler.Extend,
			CreateMotion:      creativeHandler.CreateMotion,
			CreateVideoAd:     creativeHandler.CreateVideoAd,
			UpdateScript:      creativeHandler.UpdateScript,
			RenderVideoAd:     creativeHandler.RenderVideoAd,
			MakeStoryboard:    creativeHandler.MakeStoryboard,
			ApproveStoryboard: creativeHandler.ApproveStoryboard,
			ReviseVideoAd:     creativeHandler.ReviseVideoAd,
			QuoteVideoAd:      creativeHandler.QuoteVideoAd,
			RecomposeEndCard:  creativeHandler.RecomposeEndCardPreview,
			ResetSettings:     creativeHandler.ResetSettings,
			JobCallback:       creativeHandler.JobCallback,
			OSTurnEvent:       creativeHandler.OSTurnEvent,
			CreateReviewLink:  creativeHandler.CreateReviewLink,
			RevokeReviewLinks: creativeHandler.RevokeReviewLinks,
			OpenReview:        creativeHandler.OpenReview,
			ListPPBApprovals:  creativeHandler.ListPPBApprovals,
			SetPPBApproval:    creativeHandler.SetPPBApproval,
			BriefPPBApproval:  creativeHandler.BriefPPBApproval,
		},
		AdsHandlers: httpapi.AdsHandlers{
			Accounts: adsHandler.Accounts, CreateKES: adsHandler.CreateKESAccount, Quote: adsHandler.Quote, Boost: adsHandler.Boost,
			Get: adsHandler.Get, ConfirmPaid: adsHandler.ConfirmPaid, Pause: adsHandler.Pause, Resume: adsHandler.Resume, ResumeAd: adsHandler.ResumeAd,
			BoostOTP:         adsHandler.BoostOTP,
			GoogleAccounts: googleAdsHandler.Accounts, GoogleCampaigns: googleAdsHandler.Campaigns,
			GoogleConversionActions: googleAdsHandler.ConversionActions, GoogleSetMapping: googleAdsHandler.SetMapping,
			OSTurnBoostQuote: adsHandler.OSTurnBoostQuote, OSTurnBoost: adsHandler.OSTurnBoost, OSTurnBoostOTP: adsHandler.OSTurnBoostOTP,
		},
		InboxHandlers: httpapi.InboxHandlers{
			List:    inboxHandler.List,
			Reply:   inboxHandler.Reply,
			Handled: inboxHandler.Handled,
			Promote: inboxHandler.Promote,
			Hide:    inboxHandler.Hide,
			Delete:  inboxHandler.Delete,
			// Private replies need pages_messaging (Facebook) and instagram_manage_messages
			// (Instagram); until App Review grants them, Meta refuses and the item says so.
			PrivateReply: inboxHandler.PrivateReply,
		},
		PublishingHandlers: httpapi.PublishingHandlers{
			Accounts:           publishingHandler.Accounts,
			Schedule:           publishingHandler.Schedule,
			List:               publishingHandler.List,
			Cancel:             publishingHandler.Cancel,
			Retry:              publishingHandler.Retry,
			Move:               publishingHandler.Move,
			DeleteFromPlatform: publishingHandler.DeleteFromPlatform,
			OSTurnPublish:      publishingHandler.OSTurnPublish,
			TikTokCreatorInfo:  publishingHandler.TikTokCreatorInfo,
			TikTokWebhook:      publishingHandler.TikTokWebhook,
			BestTimes:          publishingHandler.BestTimes,
			SaveBestTimes:      publishingHandler.SaveBestTimes,
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
		CallerSync:           rbacService.SyncMiddleware,
		WriteGate:            rbacService.WriteGate,
		MyAccess:             rbacService.MyAccess,
		RequirePlatformOwner: requirePlatformOwner(authMiddleware),
		RequireAICredits:     featureGate.RequireFeature("ai_chat_messages", "AI credits exhausted — purchase more at /settings/billing"),
		RequireCampaignSlot:  featureGate.RequireFeature("campaigns_active", "Campaign limit reached — upgrade your plan at /settings/billing"),
		TrackLeadCreated:     usagePub.TrackUsage("leads_captured"),
		TrackCampaignCreated: usagePub.TrackUsage("campaigns_active"),
		TrackMeetingBooked:   usagePub.TrackUsage("meetings_booked"),
		AllowedOrigins:       cfg.HTTP.AllowedOrigins,
		EnforceTenantHeader:  cfg.Security.EnforceTenantHeader,
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
	return platformOwnerGate
}

// platformOwnerGate admits platform owners to every /admin route. A service key (the internal
// key is shared across the platform) reaches only GET /admin/providers/resolve, the read that
// marketflow-ai and the worker bootstrap from; configure, deactivate, service config, tenants,
// usage and audit need a signed-in platform owner (audit MF #4).
func platformOwnerGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, ok := authclient.ClaimsFromContext(r.Context())
		if !ok || claims == nil {
			httpapi.Error(w, http.StatusUnauthorized, "unauthorized", "authentication required", nil)
			return
		}
		if claims.IsPlatformOwner {
			next.ServeHTTP(w, r)
			return
		}
		if claims.IsService && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/admin/providers/resolve") {
			next.ServeHTTP(w, r)
			return
		}
		httpapi.Error(w, http.StatusForbidden, "forbidden", "platform owner access required", nil)
	})
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
