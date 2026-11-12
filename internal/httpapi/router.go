package httpapi

import (
	"context"
	ratelimit "github.com/Bengo-Hub/shared-ratelimit"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TenantSyncer defines the interface for synchronizing tenant data from auth-api.
type TenantSyncer interface {
	SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
}

// RouterDeps defines router construction dependencies.
type RouterDeps struct {
	// Limiter is the shared per-IP request limiter (nil disables it, e.g. in tests).
	Limiter        *ratelimit.Limiter
	Log            *zap.Logger
	HealthHandler  http.HandlerFunc
	MetricsHandler http.Handler
	// TenantExport streams one tenant's rows for auth-api's SSO tenant backups; it checks
	// INTERNAL_SERVICE_KEY itself (nil when no key is set).
	TenantExport http.Handler

	TenantSyncer TenantSyncer

	// Lead handlers
	LeadHandlers LeadHandlers

	// Contact handlers
	ContactHandlers ContactHandlers

	// Knowledge base handlers
	KnowledgeHandlers KnowledgeHandlers

	// Campaign handlers
	CampaignHandlers CampaignHandlers

	// Funnel handlers
	FunnelHandlers FunnelHandlers

	// Nurture sequence handlers
	SequenceHandlers SequenceHandlers

	// Scheduled meeting handlers
	MeetingHandlers MeetingHandlers

	// Short link handlers
	ShortLinkHandlers ShortLinkHandlers

	// Webhook handlers (public, no auth)
	WebhookHandlers WebhookHandlers

	// Public handlers (no auth)
	PublicHandlers PublicHandlers

	// Admin provider handlers (platform owner only)
	AdminProviderHandlers AdminProviderHandlers

	// Admin tenants directory (platform owner only)
	AdminTenantsHandlers AdminTenantsHandlers

	// Admin service config CRUD (platform owner only)
	AdminServiceConfigHandlers AdminServiceConfigHandlers
	AdminEncryptionKeyHandlers AdminEncryptionKeyHandlers
	// Endpoint discovery (Sprint 25): the owner's approval page plus marketflow-ai's tool list.
	EndpointHandlers EndpointHandlers

	// Analytics handlers (tenant dashboard aggregates)
	AnalyticsHandlers AnalyticsHandlers

	// Testimonial handlers (tenant-scoped CRUD for /p/{slug})
	TestimonialHandlers TestimonialHandlers

	// Integration handlers (OAuth authorize + callback for Google / Meta)
	IntegrationsHandlers IntegrationsHandlers

	// Tenant profile CRUD (FBO settings)
	TenantProfileHandlers TenantProfileHandlers

	// Tenant team member management
	TenantMemberHandlers TenantMemberHandlers

	// Auth middleware
	RequireAuthHandler func(http.Handler) http.Handler
	// CallerSync keeps the signed-in user's local MarketFlow user and role in step with SSO
	// (rbac.Service.SyncMiddleware); the role gates read them.
	CallerSync func(http.Handler) http.Handler
	// WriteGate enforces MarketFlow roles (rbac.Service.WriteGate, audit MF #5): the role ladder on
	// writes, and a restricted role's allow-list (the app reviewer) on reads and writes.
	WriteGate func(http.Handler) http.Handler
	// MyAccess answers GET /api/v1/me/access: the caller's MarketFlow role for the UI.
	MyAccess             http.HandlerFunc
	RequirePlatformOwner func(http.Handler) http.Handler

	// Feature gate middleware (subscription tier enforcement)
	RequireAICredits    func(http.Handler) http.Handler
	RequireCampaignSlot func(http.Handler) http.Handler

	// Usage tracking middleware (fire-and-forget to subscriptions-api)
	TrackLeadCreated     func(http.Handler) http.Handler
	TrackCampaignCreated func(http.Handler) http.Handler
	TrackMeetingBooked   func(http.Handler) http.Handler

	// Internal S2S handlers — X-API-Key auth only, no JWT
	InternalLeadHandlers InternalLeadHandlers
	InternalServiceKey   string

	// CRM Sprint 9 handlers
	DealHandlers        DealHandlers
	PipelineHandlers    PipelineHandlers
	AccountHandlers     AccountHandlers
	ActivityHandlers    ActivityHandlers
	TaskHandlers        TaskHandlers
	CustomFieldHandlers CustomFieldHandlers
	CustomAgentHandlers CustomAgentHandlers
	MarketingOSHandlers MarketingOSHandlers
	LearningHandlers    LearningHandlers
	OnboardingHandlers  OnboardingHandlers
	CreativeHandlers    CreativeHandlers
	PublishingHandlers  PublishingHandlers
	InboxHandlers       InboxHandlers
	AdsHandlers         AdsHandlers
	SuggestionHandlers  SuggestionHandlers
	SegmentHandlers     SegmentHandlers

	// Allowed origins for CORS
	AllowedOrigins string

	// EnforceTenantHeader answers 403 when X-Tenant-ID, X-Tenant-Slug or a tenant path parameter
	// names another tenant than the token. False only logs the mismatch (rollout step).
	EnforceTenantHeader bool
}

// tenantCheck pins the tenant headers (and the tenant path parameter urlParam, when set) to the
// signed-in tenant. Handlers read X-Tenant-ID themselves, so without this any signed-in user
// could name another tenant. Platform owners, platform service keys and calls without claims
// are not checked.
func tenantCheck(deps RouterDeps, urlParam string) func(http.Handler) http.Handler {
	cfg := httpware.TenantConfig{
		ClaimsExtractor: func(ctx context.Context) (string, string, bool, bool) {
			c, ok := authclient.ClaimsFromContext(ctx)
			if !ok || c == nil {
				return "", "", false, false
			}
			// A platform service key (IsService: marketflow-ai's agent tools, the worker) acts
			// for any tenant by design, like a platform owner; its claims carry the key owner's
			// tenant, so checking the header against them would refuse every tool call.
			// Tenant-issued keys are not service keys and stay checked.
			return c.TenantID, c.GetTenantSlug(), c.IsPlatformOwner || c.IsService, true
		},
		ReportOnly: !deps.EnforceTenantHeader,
		OnMismatch: func(r *http.Request, source, supplied, claimed string) {
			if deps.Log != nil {
				deps.Log.Warn("tenant in request does not match the token",
					zap.String("method", r.Method), zap.String("path", r.URL.Path),
					zap.String("source", source), zap.String("supplied", supplied), zap.String("claimed", claimed),
					zap.Bool("enforced", deps.EnforceTenantHeader))
			}
		},
	}
	if urlParam != "" {
		cfg.URLParamFunc = chi.URLParam
		cfg.URLParamName = urlParam
	}
	return httpware.TenantV2(cfg)
}

// AdminProviderHandlers groups platform provider management handlers.
type AdminProviderHandlers struct {
	Configure   http.HandlerFunc
	List        http.HandlerFunc
	GetSettings http.HandlerFunc
	Resolve     http.HandlerFunc
	SyncModels  http.HandlerFunc
	Test        http.HandlerFunc
	Deactivate  http.HandlerFunc
}

// AdminTenantsHandlers groups platform tenant-directory HTTP handlers.
type AdminTenantsHandlers struct {
	List http.HandlerFunc
}

// AdminServiceConfigHandlers groups platform service-config CRUD HTTP handlers.
type AdminServiceConfigHandlers struct {
	ListPlatform   http.HandlerFunc
	UpsertPlatform http.HandlerFunc
	ListTenant     http.HandlerFunc
	UpsertTenant   http.HandlerFunc
}

// EndpointHandlers are the endpoint discovery routes (Sprint 25, decision 61).
type EndpointHandlers struct {
	List          http.HandlerFunc // GET /admin/endpoints
	Decide        http.HandlerFunc // PATCH /admin/endpoints/{service}
	Scan          http.HandlerFunc // POST /admin/endpoints/scan
	InternalTools http.HandlerFunc // GET /internal/endpoints/tools?tenant_id= (service key)
}

// AdminEncryptionKeyHandlers groups the platform provider-secret encryption-key
// management HTTP handlers (view status + rotate). Platform owner only.
type AdminEncryptionKeyHandlers struct {
	Get http.HandlerFunc
	Set http.HandlerFunc
}

// LeadHandlers groups lead management HTTP handlers.
type LeadHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
	Export http.HandlerFunc
}

// InternalLeadHandlers groups service-to-service lead ingestion handlers.
// These routes are protected by X-API-Key only (no JWT required).
type InternalLeadHandlers struct {
	IngestFromService http.HandlerFunc
}

// ContactHandlers groups contact management HTTP handlers.
type ContactHandlers struct {
	Create         http.HandlerFunc
	List           http.HandlerFunc
	Get            http.HandlerFunc
	Update         http.HandlerFunc
	Delete         http.HandlerFunc
	Get360         http.HandlerFunc // GET /contacts/{id}/360 — full 360° profile
	GetByEmail     http.HandlerFunc // GET /internal/contacts/by-email — S2S email lookup
	GetByPhone     http.HandlerFunc // GET /internal/contacts/by-phone — S2S phone lookup
	UpsertByPhone  http.HandlerFunc // POST /internal/contacts/upsert — S2S create-or-get by phone
	SearchInternal http.HandlerFunc // GET /internal/contacts/search — S2S name/email/phone search
	ReachInternal  http.HandlerFunc // GET /internal/contacts/reach: S2S keyset list with consent flags (broadcast audiences)
	BulkImport     http.HandlerFunc // POST /contacts/bulk-import — batch upsert (migrations)
	Merge          http.HandlerFunc // POST /contacts/merge — fold duplicate contacts into one survivor
	UpdateInternal http.HandlerFunc // PUT /internal/contacts/{id} — S2S edit (treasury/pos "edit customer")
	DeleteInternal http.HandlerFunc // DELETE /internal/contacts/{id} — S2S delete (treasury "delete customer")
	GetInternal    http.HandlerFunc // GET /internal/contacts/{id}?tenant_id= — S2S read by id
	BatchInternal  http.HandlerFunc // POST /internal/contacts/batch — S2S read of up to 200 ids
}

// KnowledgeHandlers groups knowledge base HTTP handlers.
type KnowledgeHandlers struct {
	CreateEntry http.HandlerFunc
	ListEntries http.HandlerFunc
	GetEntry    http.HandlerFunc
	UpdateEntry http.HandlerFunc
	DeleteEntry http.HandlerFunc
	ImportCSV   http.HandlerFunc
	// FAQ suggestions from answers rated helpful (Sprint 25).
	ApproveSuggestion  http.HandlerFunc
	RejectSuggestion   http.HandlerFunc
	InternalSuggestFAQ http.HandlerFunc
}

// CampaignHandlers groups campaign management HTTP handlers.
type CampaignHandlers struct {
	Create  http.HandlerFunc
	List    http.HandlerFunc
	Get     http.HandlerFunc
	Update  http.HandlerFunc
	Delete  http.HandlerFunc
	Pause   http.HandlerFunc
	Resume  http.HandlerFunc
	SendSMS http.HandlerFunc
}

// FunnelHandlers groups funnel management HTTP handlers.
type FunnelHandlers struct {
	Create    http.HandlerFunc
	List      http.HandlerFunc
	Get       http.HandlerFunc
	Update    http.HandlerFunc
	Delete    http.HandlerFunc
	Publish   http.HandlerFunc
	Analytics http.HandlerFunc
}

// SequenceHandlers groups nurture sequence HTTP handlers.
type SequenceHandlers struct {
	Create    http.HandlerFunc
	List      http.HandlerFunc
	Get       http.HandlerFunc
	Update    http.HandlerFunc
	Delete    http.HandlerFunc
	Analytics http.HandlerFunc
}

// MeetingHandlers groups scheduled meeting HTTP handlers.
type MeetingHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
}

// AnalyticsHandlers groups dashboard/aggregate HTTP handlers.
type AnalyticsHandlers struct {
	Dashboard       http.HandlerFunc
	TenantAudit     http.HandlerFunc
	PlatformUsage   http.HandlerFunc
	PlatformAudit   http.HandlerFunc
	CampaignMetrics http.HandlerFunc
	SequenceMetrics http.HandlerFunc
	ProfileMetrics  http.HandlerFunc
}

// IntegrationsHandlers groups OAuth authorize + callback endpoints
// (Google Calendar, Meta) and any related per-tenant integration management.
type IntegrationsHandlers struct {
	AvailableIntegrations             http.HandlerFunc
	GoogleAuthorize                   http.HandlerFunc
	GoogleCallback                    http.HandlerFunc
	UpdateGoogleCalendarBusinessHours http.HandlerFunc
	MetaAuthorize                     http.HandlerFunc
	MetaCallback                      http.HandlerFunc
	MetaDataDeletion                  http.HandlerFunc // Public, signed_request
	MetaDeauthorize                   http.HandlerFunc // Public, signed_request
	MetaDeletionStatus                http.HandlerFunc // Public
	TikTokAuthorize                   http.HandlerFunc
	TikTokCallback                    http.HandlerFunc
	ListTenant                        http.HandlerFunc
	DisconnectTenant                  http.HandlerFunc
}

// WebhookHandlers groups webhook HTTP handlers (public, no auth).
type WebhookHandlers struct {
	MetaVerify            http.HandlerFunc
	MetaLeadgen           http.HandlerFunc
	NotificationsDelivery http.HandlerFunc
	TikTokLeads           http.HandlerFunc
}

// PublicHandlers groups public HTTP handlers (no auth).
type PublicHandlers struct {
	GetProfile         http.HandlerFunc
	GetFunnel          http.HandlerFunc
	RenderFunnel       http.HandlerFunc
	CreateLead         http.HandlerFunc
	RecordProfileEvent http.HandlerFunc
	ListTestimonials   http.HandlerFunc
	FunnelStepView     http.HandlerFunc
	GetAvailability    http.HandlerFunc
	CreateBooking      http.HandlerFunc
}

// TenantProfileHandlers groups tenant profile settings endpoints.
type TenantProfileHandlers struct {
	Get    http.HandlerFunc
	Update http.HandlerFunc
}

// TenantMemberHandlers groups team member management endpoints.
type TenantMemberHandlers struct {
	List       http.HandlerFunc
	Invite     http.HandlerFunc
	UpdateRole http.HandlerFunc
	Remove     http.HandlerFunc
}

// TestimonialHandlers groups authenticated testimonial CRUD endpoints.
type TestimonialHandlers struct {
	List   http.HandlerFunc
	Create http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
}

// ShortLinkHandlers groups short link HTTP handlers.
type ShortLinkHandlers struct {
	Create   http.HandlerFunc
	List     http.HandlerFunc
	Redirect http.HandlerFunc // Public — no auth, GET /s/{code}
}

// DealHandlers groups deal management HTTP handlers.
type DealHandlers struct {
	Create   http.HandlerFunc
	List     http.HandlerFunc
	Get      http.HandlerFunc
	Update   http.HandlerFunc
	Move     http.HandlerFunc
	MarkWon  http.HandlerFunc
	MarkLost http.HandlerFunc
	Delete   http.HandlerFunc
}

// PipelineHandlers groups pipeline and stage HTTP handlers.
type PipelineHandlers struct {
	Create      http.HandlerFunc
	List        http.HandlerFunc
	Get         http.HandlerFunc
	Update      http.HandlerFunc
	Delete      http.HandlerFunc
	CreateStage http.HandlerFunc
	UpdateStage http.HandlerFunc
	DeleteStage http.HandlerFunc
}

// AccountHandlers groups account (company) HTTP handlers.
type AccountHandlers struct {
	Create       http.HandlerFunc
	List         http.HandlerFunc
	Get          http.HandlerFunc
	Update       http.HandlerFunc
	Delete       http.HandlerFunc
	ListContacts http.HandlerFunc
}

// ActivityHandlers groups activity timeline HTTP handlers.
type ActivityHandlers struct {
	Log             http.HandlerFunc
	GetTimeline     http.HandlerFunc
	LeadTimeline    http.HandlerFunc
	ContactTimeline http.HandlerFunc
	DealTimeline    http.HandlerFunc
}

// SuggestionHandlers groups proactive suggestion HTTP handlers.
type SuggestionHandlers struct {
	List    http.HandlerFunc
	Dismiss http.HandlerFunc
}

// TaskHandlers groups task HTTP handlers.
type TaskHandlers struct {
	Create   http.HandlerFunc
	List     http.HandlerFunc
	Get      http.HandlerFunc
	Update   http.HandlerFunc
	Complete http.HandlerFunc
	Delete   http.HandlerFunc
}

// CustomFieldHandlers groups custom field definition and value HTTP handlers.
type CustomFieldHandlers struct {
	CreateDef        http.HandlerFunc
	ListDefs         http.HandlerFunc
	UpdateDef        http.HandlerFunc
	DeleteDef        http.HandlerFunc
	SetLeadValues    http.HandlerFunc
	GetLeadValues    http.HandlerFunc
	SetContactValues http.HandlerFunc
	GetContactValues http.HandlerFunc
	SetDealValues    http.HandlerFunc
	GetDealValues    http.HandlerFunc
}

// MarketingOSHandlers groups the Marketing OS endpoints owned by marketflow-api
// (brand context, team member overrides, and the context marketflow-ai reads).
type MarketingOSHandlers struct {
	GetBrandContext http.HandlerFunc
	PutBrandContext http.HandlerFunc
	ListTeam        http.HandlerFunc
	PutTeamMember   http.HandlerFunc
	GetSettings     http.HandlerFunc
	PutSettings     http.HandlerFunc
	InternalContext http.HandlerFunc
	// BusinessFacts is what the chat may state as true about the business (service key only).
	BusinessFacts http.HandlerFunc
	// SetupTeam fits the prebuilt team to the business (portal); InternalSetupTeam is the same
	// for marketflow-ai (service key).
	SetupTeam         http.HandlerFunc
	InternalSetupTeam http.HandlerFunc
	// FindComplaints is the Fix step's search, for the owner to review.
	FindComplaints http.HandlerFunc
	// GetResearch reads what the weekly competitor scan found.
	GetResearch http.HandlerFunc
}

// LearningHandlers are the internal routes of what the Marketing OS learns (Sprint 25).
type LearningHandlers struct {
	Phrases http.HandlerFunc
	Record  http.HandlerFunc
}

// OnboardingHandlers groups the setup checklist and the brand context draft.
type OnboardingHandlers struct {
	Checklist http.HandlerFunc
	Dismiss   http.HandlerFunc
	Draft     http.HandlerFunc
}

// CreativeHandlers groups the Creative Studio endpoints (briefs, poster
// versions, review links).
type CreativeHandlers struct {
	Downloaded        http.HandlerFunc // POST /creative/assets/{id}/downloaded
	ListBriefs        http.HandlerFunc
	SearchOfferings   http.HandlerFunc
	CreateBrief       http.HandlerFunc
	GetBrief          http.HandlerFunc
	UpdateBrief       http.HandlerFunc
	LeadPage          http.HandlerFunc
	CreateVersion     http.HandlerFunc
	GetVersion        http.HandlerFunc
	Approve           http.HandlerFunc
	Discard           http.HandlerFunc
	Revise            http.HandlerFunc
	Extend            http.HandlerFunc
	CreateMotion      http.HandlerFunc
	CreateVideoAd     http.HandlerFunc
	UpdateScript      http.HandlerFunc
	RenderVideoAd     http.HandlerFunc
	MakeStoryboard    http.HandlerFunc
	ApproveStoryboard http.HandlerFunc
	ReviseVideoAd     http.HandlerFunc
	QuoteVideoAd      http.HandlerFunc
	JobCallback       http.HandlerFunc // S2S: POST /internal/creative/jobs/{id} from marketflow-ai
	CreateReviewLink  http.HandlerFunc
	RevokeReviewLinks http.HandlerFunc
	OpenReview        http.HandlerFunc // Public — GET /r/{token}
}

// AdsHandlers groups the paid ads routes (Sprint 18).
type AdsHandlers struct {
	Accounts    http.HandlerFunc
	CreateKES   http.HandlerFunc
	Quote       http.HandlerFunc
	Boost       http.HandlerFunc
	Get         http.HandlerFunc
	ConfirmPaid http.HandlerFunc
	Pause       http.HandlerFunc
	Resume      http.HandlerFunc
	// ResumeAd switches back on one ad the budget optimiser paused.
	ResumeAd http.HandlerFunc
}

// InboxHandlers groups the social inbox routes (Sprint 22).
type InboxHandlers struct {
	List    http.HandlerFunc
	Reply   http.HandlerFunc
	Handled http.HandlerFunc
	Promote http.HandlerFunc
	// Hide hides or unhides a comment; Delete deletes it (Facebook Page and Instagram only).
	Hide   http.HandlerFunc
	Delete http.HandlerFunc
}

// PublishingHandlers groups the organic publishing routes (Sprint 16).
type PublishingHandlers struct {
	Accounts http.HandlerFunc
	Schedule http.HandlerFunc
	List     http.HandlerFunc
	Cancel   http.HandlerFunc
	Retry    http.HandlerFunc
	Move     http.HandlerFunc
}

// CustomAgentHandlers groups custom agent HTTP handlers.
type CustomAgentHandlers struct {
	Create     http.HandlerFunc
	List       http.HandlerFunc
	Get        http.HandlerFunc
	Update     http.HandlerFunc
	Delete     http.HandlerFunc
	ListRuns   http.HandlerFunc
	TriggerRun http.HandlerFunc
	// Templates and HireTemplate serve the agent store (Sprint 20).
	Templates    http.HandlerFunc
	HireTemplate http.HandlerFunc
	// PutAutopilot and RollbackAutopilot save an ad autopilot's settings (Sprint 20).
	PutAutopilot      http.HandlerFunc
	RollbackAutopilot http.HandlerFunc
	// DryRunAutopilot previews the next run and the month's credits, spending nothing.
	DryRunAutopilot http.HandlerFunc
}

// SegmentHandlers groups segment management HTTP handlers.
type SegmentHandlers struct {
	Create   http.HandlerFunc
	List     http.HandlerFunc
	Get      http.HandlerFunc
	Update   http.HandlerFunc
	Delete   http.HandlerFunc
	Evaluate http.HandlerFunc
}

// NewRouter wires HTTP routes.
func NewRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()
	r.Use(httpware.RequestID)
	// Never chi RealIP: it trusts client-sent True-Client-IP/X-Forwarded-For.
	r.Use(ratelimit.TrustedRealIP)
	r.Use(httpware.Recover(deps.Log))
	r.Use(httpware.Logging(deps.Log))
	r.Use(httpware.BypassForStreaming(chimiddleware.Timeout(60 * time.Second)))
	// Local dev on any port, every platform subdomain, plus the ALLOWED_ORIGINS list.
	corsOrigins := []string{
		"http://localhost", "http://localhost:*", "https://localhost:*", "http://127.0.0.1:*",
		"https://*.codevertexafrica.com",
	}
	for _, o := range strings.Split(deps.AllowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			corsOrigins = append(corsOrigins, o)
		}
	}
	r.Use(httpware.CORS(httpware.StandardCORS(corsOrigins)))
	// Per-IP abuse limit (marketflow-api had none); after CORS so 429s carry CORS headers.
	if deps.Limiter != nil {
		r.Use(deps.Limiter.Middleware(ratelimit.IPKey, 300, time.Minute))
	}

	// Base URL redirects to Swagger docs
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/v1/docs/", http.StatusMovedPermanently)
	})

	// Health and metrics
	if deps.HealthHandler != nil {
		r.Get("/healthz", deps.HealthHandler)
	}
	if deps.MetricsHandler != nil {
		r.Method("GET", "/metrics", deps.MetricsHandler)
	}
	if deps.TenantExport != nil {
		r.Get("/api/v1/s2s/tenant-export", deps.TenantExport.ServeHTTP)
	}

	// Swagger UI and OpenAPI spec
	r.Get("/v1/docs/*", SwaggerUI)
	r.Get("/api/v1/openapi.json", OpenAPIJSON)

	r.Route("/api/v1", func(r chi.Router) {
		// Protected routes (require JWT)
		r.Group(func(r chi.Router) {
			if deps.RequireAuthHandler != nil {
				r.Use(deps.RequireAuthHandler)
			}
			r.Use(tenantCheck(deps, ""))
			// Subscription enforcement — mutations only (GET/HEAD/OPTIONS pass through).
			// Uniform 7-day grace for EXPIRED tenants.
			r.Use(authclient.RequireActiveSubscriptionForMutationsWithGrace(7))
			// Module gate: block the WHOLE marketflow module (reads and writes alike) for a
			// tenant whose plan never included it. Safe at this top level — marketflow-ui has no
			// service-level /auth/me, it uses SSO's own /api/v1/auth/me directly for bootstrap.
			r.Use(authclient.RequireServiceAccess("marketflow"))
			// Mutations-only annual support-fee gate for a perpetual/one-time-license tenant
			// (e.g. boi-enterprises on POWERSUITE_DUKA_GOLD_ONE_TIME, which already includes
			// marketflow from Tier 1) whose support fee has gone unpaid past its 7-day grace
			// window — independent axis from the subscription gate above. No-ops for every
			// tenant without a support-fee obligation at all.
			r.Use(authclient.RequireSupportFeeCurrentForMutations(7))

			if deps.TenantSyncer != nil {
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						claims, _ := authclient.ClaimsFromContext(r.Context())
						slug := r.Header.Get("X-Tenant-Slug")
						if slug == "" && claims != nil {
							slug = claims.GetTenantSlug()
						}
						var resolved uuid.UUID
						var syncErr error
						if slug != "" {
							resolved, syncErr = deps.TenantSyncer.SyncTenant(r.Context(), slug)
						}
						// A value that is not a UUID ("undefined", a slug) is as good as none.
						sent := r.Header.Get("X-Tenant-ID")
						if sent != "" {
							if _, err := uuid.Parse(sent); err != nil {
								r.Header.Del("X-Tenant-ID")
							}
						}
						// Handlers read the tenant from X-Tenant-ID. Clients that send only the slug
						// (the UI for platform owners, or a page whose URL slug differs from the
						// stored one) used to get 400 "missing X-Tenant-ID". Fill it in: a tenant
						// user's own tenant from the token (never one named by a header), and the
						// slug's tenant for platform owners and service keys, who act for any tenant.
						// A tenant user always acts for the tenant in their token. Handlers trust this
						// header, so a tenant ID a tenant user sends is overwritten, never kept: keeping
						// it let a member of one tenant read and write another tenant's contacts.
						if claims != nil && !claims.IsPlatformOwner && !claims.IsService && claims.TenantID != "" {
							r.Header.Set("X-Tenant-ID", claims.TenantID)
						}
						if r.Header.Get("X-Tenant-ID") == "" {
							switch {
							case resolved != uuid.Nil:
								r.Header.Set("X-Tenant-ID", resolved.String())
							default:
								// Nothing to act for: the handler answers 400. Logged with enough to
								// tell why, and no personal data.
								if deps.Log != nil {
									deps.Log.Warn("no tenant resolved for request",
										zap.String("path", r.URL.Path), zap.String("slug", slug),
										zap.Bool("sent_tenant_id", sent != ""), zap.Bool("has_claims", claims != nil),
										zap.Bool("claims_tenant", claims != nil && claims.TenantID != ""),
										zap.Bool("platform_owner", claims != nil && claims.IsPlatformOwner),
										zap.NamedError("sync_error", syncErr))
								}
							}
						}
						next.ServeHTTP(w, r)
					})
				})
			}

			if deps.CallerSync != nil {
				r.Use(deps.CallerSync)
			}
			if deps.WriteGate != nil {
				r.Use(deps.WriteGate)
			}

			// Optional outlet context — extracts X-Outlet-ID if present
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if outletID := httpware.OutletHeader(r); outletID != "" {
						r = r.WithContext(httpware.WithOutletID(r.Context(), outletID))
					}
					next.ServeHTTP(w, r)
				})
			})

			// The caller's own MarketFlow role, for the UI's menu and buttons.
			if deps.MyAccess != nil {
				r.Get("/me/access", deps.MyAccess)
			}

			// Lead management
			r.Route("/leads", func(r chi.Router) {
				r.With(deps.TrackLeadCreated).Post("/", deps.LeadHandlers.Create)
				r.Get("/", deps.LeadHandlers.List)
				r.Get("/export", deps.LeadHandlers.Export)
				r.Get("/{id}", deps.LeadHandlers.Get)
				r.Put("/{id}", deps.LeadHandlers.Update)
				r.Delete("/{id}", deps.LeadHandlers.Delete)
			})

			// Contact management
			r.Route("/contacts", func(r chi.Router) {
				r.Post("/", deps.ContactHandlers.Create)
				r.Get("/", deps.ContactHandlers.List)
				if deps.ContactHandlers.BulkImport != nil {
					r.Post("/bulk-import", deps.ContactHandlers.BulkImport)
				}
				if deps.ContactHandlers.Merge != nil {
					r.Post("/merge", deps.ContactHandlers.Merge)
				}
				r.Get("/{id}", deps.ContactHandlers.Get)
				r.Put("/{id}", deps.ContactHandlers.Update)
				r.Delete("/{id}", deps.ContactHandlers.Delete)
				if deps.ContactHandlers.Get360 != nil {
					r.Get("/{id}/360", deps.ContactHandlers.Get360)
				}
			})

			// Knowledge base management
			r.Route("/knowledge", func(r chi.Router) {
				r.Post("/entries", deps.KnowledgeHandlers.CreateEntry)
				r.Get("/entries", deps.KnowledgeHandlers.ListEntries)
				r.Post("/entries/import", deps.KnowledgeHandlers.ImportCSV)
				r.Get("/entries/{id}", deps.KnowledgeHandlers.GetEntry)
				r.Put("/entries/{id}", deps.KnowledgeHandlers.UpdateEntry)
				r.Delete("/entries/{id}", deps.KnowledgeHandlers.DeleteEntry)
				if deps.KnowledgeHandlers.ApproveSuggestion != nil {
					r.Post("/entries/{id}/approve", deps.KnowledgeHandlers.ApproveSuggestion)
					r.Post("/entries/{id}/reject", deps.KnowledgeHandlers.RejectSuggestion)
				}
			})

			// Campaign management — create gated by subscription tier.
			r.Route("/campaigns", func(r chi.Router) {
				r.With(deps.RequireCampaignSlot, deps.TrackCampaignCreated).Post("/", deps.CampaignHandlers.Create)
				r.Get("/", deps.CampaignHandlers.List)
				r.Get("/{id}", deps.CampaignHandlers.Get)
				r.Put("/{id}", deps.CampaignHandlers.Update)
				r.Delete("/{id}", deps.CampaignHandlers.Delete)
				r.Post("/{id}/pause", deps.CampaignHandlers.Pause)
				r.Post("/{id}/resume", deps.CampaignHandlers.Resume)
			})

			// Bulk SMS for a campaign — tenant + campaign in the path so POS/admin
			// callers can target a CRM segment without relying on header context.
			r.Route("/{tenantID}/campaigns/{campaignID}", func(r chi.Router) {
				r.With(tenantCheck(deps, "tenantID")).Post("/send-sms", deps.CampaignHandlers.SendSMS)
			})

			// Funnel management
			r.Route("/funnels", func(r chi.Router) {
				r.Post("/", deps.FunnelHandlers.Create)
				r.Get("/", deps.FunnelHandlers.List)
				r.Get("/{id}", deps.FunnelHandlers.Get)
				r.Put("/{id}", deps.FunnelHandlers.Update)
				r.Delete("/{id}", deps.FunnelHandlers.Delete)
				r.Post("/{id}/publish", deps.FunnelHandlers.Publish)
				r.Get("/{id}/analytics", deps.FunnelHandlers.Analytics)
			})

			// Nurture sequence management
			r.Route("/sequences", func(r chi.Router) {
				r.Post("/", deps.SequenceHandlers.Create)
				r.Get("/", deps.SequenceHandlers.List)
				r.Get("/{id}", deps.SequenceHandlers.Get)
				r.Put("/{id}", deps.SequenceHandlers.Update)
				r.Delete("/{id}", deps.SequenceHandlers.Delete)
				r.Get("/{id}/analytics", deps.SequenceHandlers.Analytics)
			})

			// Scheduled meeting management
			r.Route("/meetings", func(r chi.Router) {
				r.Post("/", deps.MeetingHandlers.Create)
				r.Get("/", deps.MeetingHandlers.List)
				r.Get("/{id}", deps.MeetingHandlers.Get)
				r.Put("/{id}", deps.MeetingHandlers.Update)
			})

			// Testimonial CRUD — rendered on the public /p/{slug} page.
			// Tenant profile settings (FBO branding, phone, bio, distribID)
			r.Route("/tenant/profile", func(r chi.Router) {
				r.Get("/", deps.TenantProfileHandlers.Get)
				r.Patch("/", deps.TenantProfileHandlers.Update)
			})

			// Tenant team member management
			if deps.TenantMemberHandlers.List != nil {
				r.Route("/tenant/members", func(r chi.Router) {
					r.Get("/", deps.TenantMemberHandlers.List)
					r.Post("/invite", deps.TenantMemberHandlers.Invite)
					r.Put("/{userId}", deps.TenantMemberHandlers.UpdateRole)
					r.Delete("/{userId}", deps.TenantMemberHandlers.Remove)
				})
			}

			r.Route("/testimonials", func(r chi.Router) {
				r.Get("/", deps.TestimonialHandlers.List)
				r.Post("/", deps.TestimonialHandlers.Create)
				r.Patch("/{id}", deps.TestimonialHandlers.Update)
				r.Delete("/{id}", deps.TestimonialHandlers.Delete)
			})

			// Dashboard aggregates for the tenant dashboard UI.
			r.Route("/analytics", func(r chi.Router) {
				r.Get("/dashboard", deps.AnalyticsHandlers.Dashboard)
				r.Get("/audit", deps.AnalyticsHandlers.TenantAudit)
				r.Get("/campaigns/{id}", deps.AnalyticsHandlers.CampaignMetrics)
				r.Get("/sequences/{id}", deps.AnalyticsHandlers.SequenceMetrics)
				r.Get("/profile", deps.AnalyticsHandlers.ProfileMetrics)
			})

			// Per-tenant integration OAuth flows. authorize returns the
			// provider auth URL; the callbacks are public routes (below).
			r.Route("/integrations", func(r chi.Router) {
				r.Get("/available", deps.IntegrationsHandlers.AvailableIntegrations)
				r.Get("/google/authorize", deps.IntegrationsHandlers.GoogleAuthorize)
				r.Put("/google/business-hours", deps.IntegrationsHandlers.UpdateGoogleCalendarBusinessHours)
				r.Get("/meta/authorize", deps.IntegrationsHandlers.MetaAuthorize)
				r.Get("/tiktok/authorize", deps.IntegrationsHandlers.TikTokAuthorize)
				r.Get("/", deps.IntegrationsHandlers.ListTenant)
				r.Delete("/{provider}", deps.IntegrationsHandlers.DisconnectTenant)
			})

			// Short link management (authenticated)
			r.Route("/short-links", func(r chi.Router) {
				r.Post("/", deps.ShortLinkHandlers.Create)
				r.Get("/", deps.ShortLinkHandlers.List)
			})

			// CRM: Deal management
			if deps.DealHandlers.Create != nil {
				r.Route("/deals", func(r chi.Router) {
					r.Post("/", deps.DealHandlers.Create)
					r.Get("/", deps.DealHandlers.List)
					r.Get("/{id}", deps.DealHandlers.Get)
					r.Put("/{id}", deps.DealHandlers.Update)
					r.Delete("/{id}", deps.DealHandlers.Delete)
					r.Post("/{id}/move", deps.DealHandlers.Move)
					r.Post("/{id}/won", deps.DealHandlers.MarkWon)
					r.Post("/{id}/lost", deps.DealHandlers.MarkLost)
				})
			}

			// CRM: Pipeline management
			if deps.PipelineHandlers.Create != nil {
				r.Route("/pipelines", func(r chi.Router) {
					r.Post("/", deps.PipelineHandlers.Create)
					r.Get("/", deps.PipelineHandlers.List)
					r.Get("/{id}", deps.PipelineHandlers.Get)
					r.Put("/{id}", deps.PipelineHandlers.Update)
					r.Delete("/{id}", deps.PipelineHandlers.Delete)
					r.Post("/{id}/stages", deps.PipelineHandlers.CreateStage)
					r.Put("/{id}/stages/{stageId}", deps.PipelineHandlers.UpdateStage)
					r.Delete("/{id}/stages/{stageId}", deps.PipelineHandlers.DeleteStage)
				})
			}

			// CRM: Account management
			if deps.AccountHandlers.Create != nil {
				r.Route("/accounts", func(r chi.Router) {
					r.Post("/", deps.AccountHandlers.Create)
					r.Get("/", deps.AccountHandlers.List)
					r.Get("/{id}", deps.AccountHandlers.Get)
					r.Put("/{id}", deps.AccountHandlers.Update)
					r.Delete("/{id}", deps.AccountHandlers.Delete)
					r.Get("/{id}/contacts", deps.AccountHandlers.ListContacts)
				})
			}

			// CRM: Activity timeline
			if deps.ActivityHandlers.Log != nil {
				r.Route("/activities", func(r chi.Router) {
					r.Post("/", deps.ActivityHandlers.Log)
					r.Get("/", deps.ActivityHandlers.GetTimeline)
				})
				r.Get("/leads/{id}/timeline", deps.ActivityHandlers.LeadTimeline)
				r.Get("/contacts/{id}/timeline", deps.ActivityHandlers.ContactTimeline)
				r.Get("/deals/{id}/timeline", deps.ActivityHandlers.DealTimeline)
			}

			// CRM: Task management
			if deps.TaskHandlers.Create != nil {
				r.Route("/tasks", func(r chi.Router) {
					r.Post("/", deps.TaskHandlers.Create)
					r.Get("/", deps.TaskHandlers.List)
					r.Get("/{id}", deps.TaskHandlers.Get)
					r.Put("/{id}", deps.TaskHandlers.Update)
					r.Delete("/{id}", deps.TaskHandlers.Delete)
					r.Post("/{id}/complete", deps.TaskHandlers.Complete)
				})
			}

			// CRM: Custom fields
			if deps.CustomFieldHandlers.CreateDef != nil {
				r.Route("/custom-fields", func(r chi.Router) {
					r.Post("/defs", deps.CustomFieldHandlers.CreateDef)
					r.Get("/defs", deps.CustomFieldHandlers.ListDefs)
					r.Put("/defs/{id}", deps.CustomFieldHandlers.UpdateDef)
					r.Delete("/defs/{id}", deps.CustomFieldHandlers.DeleteDef)
				})
				r.Get("/leads/{id}/custom-fields", deps.CustomFieldHandlers.GetLeadValues)
				r.Put("/leads/{id}/custom-fields", deps.CustomFieldHandlers.SetLeadValues)
				r.Get("/contacts/{id}/custom-fields", deps.CustomFieldHandlers.GetContactValues)
				r.Put("/contacts/{id}/custom-fields", deps.CustomFieldHandlers.SetContactValues)
				r.Get("/deals/{id}/custom-fields", deps.CustomFieldHandlers.GetDealValues)
				r.Put("/deals/{id}/custom-fields", deps.CustomFieldHandlers.SetDealValues)
			}

			// CRM: Custom agents
			if deps.CustomAgentHandlers.Create != nil {
				if deps.CustomAgentHandlers.Templates != nil {
					r.Get("/agents/templates", deps.CustomAgentHandlers.Templates)
					r.Post("/agents/templates/{id}/hire", deps.CustomAgentHandlers.HireTemplate)
				}
				r.Route("/agents/custom", func(r chi.Router) {
					r.Post("/", deps.CustomAgentHandlers.Create)
					r.Get("/", deps.CustomAgentHandlers.List)
					r.Get("/{id}", deps.CustomAgentHandlers.Get)
					r.Put("/{id}", deps.CustomAgentHandlers.Update)
					r.Delete("/{id}", deps.CustomAgentHandlers.Delete)
					r.Get("/{id}/runs", deps.CustomAgentHandlers.ListRuns)
					if deps.CustomAgentHandlers.PutAutopilot != nil {
						r.Put("/{id}/autopilot", deps.CustomAgentHandlers.PutAutopilot)
						r.Post("/{id}/autopilot/rollback", deps.CustomAgentHandlers.RollbackAutopilot)
						r.Get("/{id}/autopilot/dry-run", deps.CustomAgentHandlers.DryRunAutopilot)
					}
					if deps.CustomAgentHandlers.TriggerRun != nil {
						r.Post("/{id}/run", deps.CustomAgentHandlers.TriggerRun)
					}
				})
			}

			// Onboarding: setup checklist and a brand context draft (nothing saved by the draft).
			if deps.OnboardingHandlers.Checklist != nil {
				r.Get("/tenant/onboarding", deps.OnboardingHandlers.Checklist)
				r.Post("/tenant/onboarding/dismiss", deps.OnboardingHandlers.Dismiss)
				r.Get("/tenant/marketing-context/draft", deps.OnboardingHandlers.Draft)
			}

			// Marketing OS: brand context and team member overrides. The tenant
			// header is checked against the token by the group's tenant check.
			if deps.MarketingOSHandlers.GetBrandContext != nil {
				r.Get("/tenant/marketing-context", deps.MarketingOSHandlers.GetBrandContext)
				r.Put("/tenant/marketing-context", deps.MarketingOSHandlers.PutBrandContext)
				r.Get("/agents/team", deps.MarketingOSHandlers.ListTeam)
				if deps.MarketingOSHandlers.SetupTeam != nil {
					r.Post("/agents/team/setup", deps.MarketingOSHandlers.SetupTeam)
				}
				r.Put("/agents/team/{key}", deps.MarketingOSHandlers.PutTeamMember)
				r.Get("/tenant/os-settings", deps.MarketingOSHandlers.GetSettings)
				r.Put("/tenant/os-settings", deps.MarketingOSHandlers.PutSettings)
				if deps.MarketingOSHandlers.FindComplaints != nil {
					r.Post("/tenant/market-research/complaints", deps.MarketingOSHandlers.FindComplaints)
				}
				if deps.MarketingOSHandlers.GetResearch != nil {
					r.Get("/tenant/market-research", deps.MarketingOSHandlers.GetResearch)
				}
			}

			// Creative Studio: briefs and the poster versions made from them.
			if deps.CreativeHandlers.ListBriefs != nil {
				r.Route("/creative", func(r chi.Router) {
					r.Get("/briefs", deps.CreativeHandlers.ListBriefs)
					r.Get("/offerings", deps.CreativeHandlers.SearchOfferings)
					r.Post("/briefs", deps.CreativeHandlers.CreateBrief)
					r.Get("/briefs/{id}", deps.CreativeHandlers.GetBrief)
					r.Put("/briefs/{id}", deps.CreativeHandlers.UpdateBrief)
					if deps.CreativeHandlers.LeadPage != nil {
						r.Post("/briefs/{id}/lead-page", deps.CreativeHandlers.LeadPage)
					}
					r.Post("/briefs/{id}/versions", deps.CreativeHandlers.CreateVersion)
					r.Post("/briefs/{id}/video-ads", deps.CreativeHandlers.CreateVideoAd)
					r.Get("/versions/{id}", deps.CreativeHandlers.GetVersion)
					r.Post("/versions/{id}/approve", deps.CreativeHandlers.Approve)
					r.Post("/versions/{id}/discard", deps.CreativeHandlers.Discard)
					if deps.CreativeHandlers.Downloaded != nil {
						r.Post("/assets/{id}/downloaded", deps.CreativeHandlers.Downloaded)
					}
					r.Post("/versions/{id}/revise", deps.CreativeHandlers.Revise)
					r.Post("/versions/{id}/extend", deps.CreativeHandlers.Extend)
					r.Post("/versions/{id}/motion", deps.CreativeHandlers.CreateMotion)
					r.Put("/versions/{id}/script", deps.CreativeHandlers.UpdateScript)
					r.Post("/versions/{id}/render", deps.CreativeHandlers.RenderVideoAd)
					r.Post("/versions/{id}/storyboard", deps.CreativeHandlers.MakeStoryboard)
					r.Post("/versions/{id}/storyboard/approve", deps.CreativeHandlers.ApproveStoryboard)
					r.Post("/versions/{id}/revision", deps.CreativeHandlers.ReviseVideoAd)
					r.Get("/versions/{id}/quote", deps.CreativeHandlers.QuoteVideoAd)
					r.Post("/versions/{id}/review-link", deps.CreativeHandlers.CreateReviewLink)
					r.Delete("/versions/{id}/review-link", deps.CreativeHandlers.RevokeReviewLinks)
				})
			}

			// Paid ads on the client's own Meta ad account: accounts, quote, boost, paid, pause.
			if deps.AdsHandlers.Quote != nil {
				r.Route("/ads", func(r chi.Router) {
					r.Get("/accounts", deps.AdsHandlers.Accounts)
					r.Post("/accounts/kes", deps.AdsHandlers.CreateKES)
					r.Post("/quote", deps.AdsHandlers.Quote)
					r.Post("/boosts", deps.AdsHandlers.Boost)
					r.Get("/boosts/{id}", deps.AdsHandlers.Get)
					r.Post("/boosts/{id}/paid", deps.AdsHandlers.ConfirmPaid)
					r.Post("/boosts/{id}/pause", deps.AdsHandlers.Pause)
					if deps.AdsHandlers.Resume != nil {
						r.Post("/boosts/{id}/resume", deps.AdsHandlers.Resume)
						if deps.AdsHandlers.ResumeAd != nil {
							r.Post("/boosts/{id}/ads/{ad_id}/resume", deps.AdsHandlers.ResumeAd)
						}
					}
				})
			}

			// Social inbox: comments and DMs with intent tags, reply, handled, promote.
			if deps.InboxHandlers.List != nil {
				r.Route("/inbox", func(r chi.Router) {
					r.Get("/", deps.InboxHandlers.List)
					r.Post("/{id}/reply", deps.InboxHandlers.Reply)
					r.Post("/{id}/handled", deps.InboxHandlers.Handled)
					r.Post("/{id}/promote", deps.InboxHandlers.Promote)
					// Hide, unhide or delete a Facebook Page or Instagram comment.
					if deps.InboxHandlers.Hide != nil {
						r.Post("/{id}/hide", deps.InboxHandlers.Hide)
						r.Delete("/{id}", deps.InboxHandlers.Delete)
					}
				})
			}

			// Organic publishing: approved videos to connected accounts, the calendar.
			if deps.PublishingHandlers.Schedule != nil {
				r.Route("/publishing", func(r chi.Router) {
					r.Get("/accounts", deps.PublishingHandlers.Accounts)
					r.Post("/jobs", deps.PublishingHandlers.Schedule)
					r.Get("/jobs", deps.PublishingHandlers.List)
					r.Post("/jobs/{id}/cancel", deps.PublishingHandlers.Cancel)
					r.Post("/jobs/{id}/retry", deps.PublishingHandlers.Retry)
					r.Post("/jobs/{id}/move", deps.PublishingHandlers.Move)
				})
			}

			// CRM: Proactive suggestions
			if deps.SuggestionHandlers.List != nil {
				r.Get("/suggestions", deps.SuggestionHandlers.List)
				if deps.SuggestionHandlers.Dismiss != nil {
					r.Post("/suggestions/{id}/dismiss", deps.SuggestionHandlers.Dismiss)
				}
			}

			// CRM: Segments
			if deps.SegmentHandlers.Create != nil {
				r.Route("/segments", func(r chi.Router) {
					r.Post("/", deps.SegmentHandlers.Create)
					r.Get("/", deps.SegmentHandlers.List)
					r.Get("/{id}", deps.SegmentHandlers.Get)
					r.Put("/{id}", deps.SegmentHandlers.Update)
					r.Delete("/{id}", deps.SegmentHandlers.Delete)
					r.Post("/{id}/evaluate", deps.SegmentHandlers.Evaluate)
				})
			}

			// Admin routes (platform owner only)
			r.Route("/admin", func(r chi.Router) {
				if deps.RequirePlatformOwner != nil {
					r.Use(deps.RequirePlatformOwner)
				}

				// Provider configuration management
				r.Route("/providers", func(r chi.Router) {
					r.Post("/configure", deps.AdminProviderHandlers.Configure)
					r.Get("/", deps.AdminProviderHandlers.List)
					r.Get("/settings", deps.AdminProviderHandlers.GetSettings)
					// /resolve is the internal service-to-service read path;
					// returns DECRYPTED values so marketflow-ai + worker can
					// bootstrap their clients. Gated by platform-owner/JWT or
					// X-API-Key (same middleware).
					r.Get("/resolve", deps.AdminProviderHandlers.Resolve)
					r.Post("/sync-models", deps.AdminProviderHandlers.SyncModels)
					r.Post("/test", deps.AdminProviderHandlers.Test)
					r.Delete("/{id}", deps.AdminProviderHandlers.Deactivate)
				})

				// Platform usage aggregate + audit trail (platform-owner only).
				r.Get("/usage", deps.AnalyticsHandlers.PlatformUsage)
				r.Get("/audit", deps.AnalyticsHandlers.PlatformAudit)

				// Tenant directory (drives /platform/tenants UI + tenant switcher).
				r.Get("/tenants", deps.AdminTenantsHandlers.List)

				// Service config CRUD (platform defaults + tenant overrides)
				if deps.AdminServiceConfigHandlers.ListPlatform != nil {
					r.Route("/service-config", func(r chi.Router) {
						r.Get("/", deps.AdminServiceConfigHandlers.ListPlatform)
						r.Put("/{key}", deps.AdminServiceConfigHandlers.UpsertPlatform)
						r.Get("/tenant/{tenantId}", deps.AdminServiceConfigHandlers.ListTenant)
						r.Put("/tenant/{tenantId}/{key}", deps.AdminServiceConfigHandlers.UpsertTenant)
					})
				}

				// Provider-secret encryption key (view status + rotate).
				// Raw key is never returned. Platform owner only (same gate).
				if deps.AdminEncryptionKeyHandlers.Get != nil {
					r.Route("/encryption-key", func(r chi.Router) {
						r.Get("/", deps.AdminEncryptionKeyHandlers.Get)
						r.Put("/", deps.AdminEncryptionKeyHandlers.Set)
					})
				}
				if deps.EndpointHandlers.List != nil {
					r.Route("/endpoints", func(r chi.Router) {
						r.Get("/", deps.EndpointHandlers.List)
						r.Post("/scan", deps.EndpointHandlers.Scan)
						r.Patch("/{service}", deps.EndpointHandlers.Decide)
					})
				}
			})
		})

		// Internal S2S routes — X-API-Key only, no JWT.
		// Called by platform services (codevertex-website, ordering-backend, etc.)
		// using the shared INTERNAL_SERVICE_KEY to push leads into MarketFlow.
		if deps.InternalServiceKey != "" && deps.InternalLeadHandlers.IngestFromService != nil {
			r.Group(func(r chi.Router) {
				svcKey := deps.InternalServiceKey
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if !httpware.ValidServiceKey(r, svcKey) {
							Error(w, http.StatusUnauthorized, "unauthorized", "invalid service key", nil)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
				r.Route("/internal", func(r chi.Router) {
					r.Post("/leads", deps.InternalLeadHandlers.IngestFromService)
					// Marketing OS context for marketflow-ai (tenant in the query, service key only).
					if deps.MarketingOSHandlers.InternalContext != nil {
						r.Get("/os/context", deps.MarketingOSHandlers.InternalContext)
						if deps.MarketingOSHandlers.BusinessFacts != nil {
							r.Get("/os/business-facts", deps.MarketingOSHandlers.BusinessFacts)
						}
					}
					if deps.EndpointHandlers.InternalTools != nil {
						r.Get("/endpoints/tools", deps.EndpointHandlers.InternalTools)
					}
					if deps.MarketingOSHandlers.InternalSetupTeam != nil {
						r.Post("/os/team/setup", deps.MarketingOSHandlers.InternalSetupTeam)
					}
					// What the Marketing OS learns from use (Sprint 25): phrasings per intent.
					if deps.LearningHandlers.Phrases != nil {
						r.Get("/learning/phrases", deps.LearningHandlers.Phrases)
						r.Post("/learning/phrases", deps.LearningHandlers.Record)
					}
					// Answers rated helpful, kept as FAQ suggestions for the owner to approve.
					if deps.KnowledgeHandlers.InternalSuggestFAQ != nil {
						r.Post("/knowledge/faq-suggestions", deps.KnowledgeHandlers.InternalSuggestFAQ)
					}
					// marketflow-ai pushes a finished or failed media job (motion poster, video ad).
					if deps.CreativeHandlers.JobCallback != nil {
						r.Post("/creative/jobs/{id}", deps.CreativeHandlers.JobCallback)
					}
					if deps.ContactHandlers.GetByEmail != nil {
						r.Get("/contacts/by-email", deps.ContactHandlers.GetByEmail)
					}
					if deps.ContactHandlers.GetByPhone != nil {
						r.Get("/contacts/by-phone", deps.ContactHandlers.GetByPhone)
					}
					if deps.ContactHandlers.UpsertByPhone != nil {
						r.Post("/contacts/upsert", deps.ContactHandlers.UpsertByPhone)
					}
					if deps.ContactHandlers.SearchInternal != nil {
						r.Get("/contacts/search", deps.ContactHandlers.SearchInternal)
					}
					// Registered before /contacts/{id} so "reach" is never read as a contact id.
					if deps.ContactHandlers.ReachInternal != nil {
						r.Get("/contacts/reach", deps.ContactHandlers.ReachInternal)
					}
					// Static paths above and /contacts/batch win over /contacts/{id} in chi.
					if deps.ContactHandlers.BatchInternal != nil {
						r.Post("/contacts/batch", deps.ContactHandlers.BatchInternal)
					}
					if deps.ContactHandlers.GetInternal != nil {
						r.Get("/contacts/{id}", deps.ContactHandlers.GetInternal)
					}
					if deps.ContactHandlers.UpdateInternal != nil {
						r.Put("/contacts/{id}", deps.ContactHandlers.UpdateInternal)
					}
					if deps.ContactHandlers.DeleteInternal != nil {
						r.Delete("/contacts/{id}", deps.ContactHandlers.DeleteInternal)
					}
				})
			})
		}

		// OAuth callbacks (public). The provider redirects the browser here without a bearer
		// token, so inside the authenticated group every connect ended in a 401. The HMAC-signed,
		// 10-minute state names the tenant. The paths are registered with Meta, TikTok and Google.
		r.Get("/integrations/google/callback", deps.IntegrationsHandlers.GoogleCallback)
		r.Get("/integrations/meta/callback", deps.IntegrationsHandlers.MetaCallback)
		// Meta's required data deletion and deauthorize callbacks (signed_request,
		// verified with the app secret) and the public status of a deletion request.
		if deps.IntegrationsHandlers.MetaDataDeletion != nil {
			r.Post("/integrations/meta/data-deletion", deps.IntegrationsHandlers.MetaDataDeletion)
			r.Post("/integrations/meta/deauthorize", deps.IntegrationsHandlers.MetaDeauthorize)
			r.Get("/public/data-deletion/{code}", deps.IntegrationsHandlers.MetaDeletionStatus)
		}
		r.Get("/integrations/tiktok/callback", deps.IntegrationsHandlers.TikTokCallback)

		// Webhook routes (public, no auth — signature-verified by each handler)
		r.Route("/webhooks", func(r chi.Router) {
			r.Get("/meta/leadgen", deps.WebhookHandlers.MetaVerify)
			r.Post("/meta/leadgen", deps.WebhookHandlers.MetaLeadgen)
			// Internal peer only: a service key, never public (audit MF #6).
			if deps.RequireAuthHandler != nil {
				r.With(deps.RequireAuthHandler, requireServiceCaller).
					Post("/notifications/delivery", deps.WebhookHandlers.NotificationsDelivery)
			}
			r.Post("/tiktok/leads", deps.WebhookHandlers.TikTokLeads)
		})

		// Public API routes (no auth). Lead capture is public and rate-limited
		// at the ingress layer; the handler enforces tenant lookup + basic validation.
		r.Route("/public", func(r chi.Router) {
			r.Get("/profile/{slug}", deps.PublicHandlers.GetProfile)
			r.Get("/profile/{slug}/testimonials", deps.PublicHandlers.ListTestimonials)
			r.Get("/funnels/{slug}", deps.PublicHandlers.GetFunnel)
			r.With(deps.TrackLeadCreated).Post("/leads/{slug}", deps.PublicHandlers.CreateLead)
			r.Post("/profile-events", deps.PublicHandlers.RecordProfileEvent)
			r.Post("/funnels/{slug}/step-view", deps.PublicHandlers.FunnelStepView)
			r.Get("/availability/{slug}", deps.PublicHandlers.GetAvailability)
			r.With(deps.TrackMeetingBooked).Post("/book/{slug}", deps.PublicHandlers.CreateBooking)
		})
	})

	// Public short link redirect (no auth required)
	// GET /s/{code} → 302 redirect to target URL
	r.Get("/s/{code}", deps.ShortLinkHandlers.Redirect)

	// Public funnel hosting — server-rendered HTML landing page (no auth).
	r.Get("/f/{slug}", deps.PublicHandlers.RenderFunnel)

	// Public FBO profile page (no auth required)
	r.Get("/p/{slug}", deps.PublicHandlers.GetProfile)

	// Public creative review link (HMAC token, expiring, revocable) → 302 to the draft.
	if deps.CreativeHandlers.OpenReview != nil {
		r.Get("/r/{token}", deps.CreativeHandlers.OpenReview)
	}

	return r
}
