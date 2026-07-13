package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TenantSyncer defines the interface for synchronizing tenant data from auth-api.
type TenantSyncer interface {
	SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
}

// RouterDeps defines router construction dependencies.
type RouterDeps struct {
	Log            *zap.Logger
	HealthHandler  http.HandlerFunc
	MetricsHandler http.Handler

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

	// Tenant-scoped backups (this tenant's data only)
	BackupHandlers BackupHandlers

	// Auth middleware
	RequireAuthHandler   func(http.Handler) http.Handler
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
	SuggestionHandlers  SuggestionHandlers
	SegmentHandlers     SegmentHandlers

	// Allowed origins for CORS
	AllowedOrigins string
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
	Create        http.HandlerFunc
	List          http.HandlerFunc
	Get           http.HandlerFunc
	Update        http.HandlerFunc
	Delete        http.HandlerFunc
	Get360        http.HandlerFunc // GET /contacts/{id}/360 — full 360° profile
	GetByEmail    http.HandlerFunc // GET /internal/contacts/by-email — S2S email lookup
	GetByPhone    http.HandlerFunc // GET /internal/contacts/by-phone — S2S phone lookup
	UpsertByPhone http.HandlerFunc // POST /internal/contacts/upsert — S2S create-or-get by phone
	BulkImport    http.HandlerFunc // POST /contacts/bulk-import — batch upsert (migrations)
}

// KnowledgeHandlers groups knowledge base HTTP handlers.
type KnowledgeHandlers struct {
	CreateEntry http.HandlerFunc
	ListEntries http.HandlerFunc
	GetEntry    http.HandlerFunc
	UpdateEntry http.HandlerFunc
	DeleteEntry http.HandlerFunc
	ImportCSV   http.HandlerFunc
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

// BackupHandlers groups tenant-scoped backup endpoints (this tenant's data only).
type BackupHandlers struct {
	List           http.HandlerFunc
	Create         http.HandlerFunc
	GetSettings    http.HandlerFunc
	UpdateSettings http.HandlerFunc
	Churn          http.HandlerFunc
	Download       http.HandlerFunc
	Delete         http.HandlerFunc
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

// CustomAgentHandlers groups custom agent HTTP handlers.
type CustomAgentHandlers struct {
	Create     http.HandlerFunc
	List       http.HandlerFunc
	Get        http.HandlerFunc
	Update     http.HandlerFunc
	Delete     http.HandlerFunc
	ListRuns   http.HandlerFunc
	TriggerRun http.HandlerFunc
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
	r.Use(chimiddleware.RealIP)
	r.Use(httpware.Recover(deps.Log))
	r.Use(httpware.Logging(deps.Log))
	r.Use(chimiddleware.Timeout(60 * time.Second))
	r.Use(cors.Handler(cors.Options{
		AllowOriginFunc: func(r *http.Request, origin string) bool {
			if origin == "" {
				return true
			}
			// Allow localhost for development
			if strings.HasPrefix(origin, "http://localhost") ||
				strings.HasPrefix(origin, "http://127.0.0.1") ||
				strings.HasPrefix(origin, "https://localhost") {
				return true
			}
			// Allow any subdomain of codevertexitsolutions.com
			const suffix = ".codevertexitsolutions.com"
			const httpsPrefix = "https://"
			if len(origin) > len(httpsPrefix)+len(suffix) &&
				origin[:len(httpsPrefix)] == httpsPrefix &&
				origin[len(origin)-len(suffix):] == suffix {
				return true
			}
			// Check explicit allowed origins from config
			if deps.AllowedOrigins != "" {
				for _, allowed := range strings.Split(deps.AllowedOrigins, ",") {
					if strings.TrimSpace(allowed) == origin {
						return true
					}
				}
			}
			return false
		},
		AllowedMethods:   []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-ID", "X-Tenant-Slug", "X-Tenant-ID", "X-Outlet-ID"},
		ExposedHeaders:   []string{"Set-Cookie"},
		AllowCredentials: true,
		MaxAge:           300,
	}))

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

	// Swagger UI and OpenAPI spec
	r.Get("/v1/docs/*", SwaggerUI)
	r.Get("/api/v1/openapi.json", OpenAPIJSON)

	r.Route("/api/v1", func(r chi.Router) {
		// Protected routes (require JWT)
		r.Group(func(r chi.Router) {
			if deps.RequireAuthHandler != nil {
				r.Use(deps.RequireAuthHandler)
			}
			// Subscription enforcement — mutations only (GET/HEAD/OPTIONS pass through).
			// Uniform 7-day grace for EXPIRED tenants.
			r.Use(authclient.RequireActiveSubscriptionForMutationsWithGrace(7))

			if deps.TenantSyncer != nil {
				r.Use(func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						slug := r.Header.Get("X-Tenant-Slug")
						if slug == "" {
							if claims, ok := authclient.ClaimsFromContext(r.Context()); ok {
								slug = claims.GetTenantSlug()
							}
						}
						if slug != "" {
							_, _ = deps.TenantSyncer.SyncTenant(r.Context(), slug)
						}
						next.ServeHTTP(w, r)
					})
				})
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

			// Lead management
			r.Route("/leads", func(r chi.Router) {
				r.With(deps.TrackLeadCreated).Post("/", deps.LeadHandlers.Create)
				r.Get("/", deps.LeadHandlers.List)
				r.Get("/export", deps.LeadHandlers.Export)
				r.Get("/{id}", deps.LeadHandlers.Get)
				r.Put("/{id}", deps.LeadHandlers.Update)
				r.Delete("/{id}", deps.LeadHandlers.Delete)
			})

			// Tenant-scoped backups (this tenant's data only) — authenticated tenant context.
			if deps.BackupHandlers.List != nil {
				r.Route("/backups", func(r chi.Router) {
					r.Get("/", deps.BackupHandlers.List)
					r.Post("/", deps.BackupHandlers.Create)
					r.Get("/settings", deps.BackupHandlers.GetSettings)
					r.Put("/settings", deps.BackupHandlers.UpdateSettings)
					r.Post("/churn", deps.BackupHandlers.Churn)
					r.Get("/{name}/download", deps.BackupHandlers.Download)
					r.Delete("/{name}", deps.BackupHandlers.Delete)
				})
			}

			// Contact management
			r.Route("/contacts", func(r chi.Router) {
				r.Post("/", deps.ContactHandlers.Create)
				r.Get("/", deps.ContactHandlers.List)
				if deps.ContactHandlers.BulkImport != nil {
					r.Post("/bulk-import", deps.ContactHandlers.BulkImport)
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
				r.Post("/send-sms", deps.CampaignHandlers.SendSMS)
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
			// provider auth URL; callback is hit by the provider.
			r.Route("/integrations", func(r chi.Router) {
				r.Get("/available", deps.IntegrationsHandlers.AvailableIntegrations)
				r.Get("/google/authorize", deps.IntegrationsHandlers.GoogleAuthorize)
				r.Get("/google/callback", deps.IntegrationsHandlers.GoogleCallback)
				r.Put("/google/business-hours", deps.IntegrationsHandlers.UpdateGoogleCalendarBusinessHours)
				r.Get("/meta/authorize", deps.IntegrationsHandlers.MetaAuthorize)
				r.Get("/meta/callback", deps.IntegrationsHandlers.MetaCallback)
				r.Get("/tiktok/authorize", deps.IntegrationsHandlers.TikTokAuthorize)
				r.Get("/tiktok/callback", deps.IntegrationsHandlers.TikTokCallback)
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
				r.Route("/agents/custom", func(r chi.Router) {
					r.Post("/", deps.CustomAgentHandlers.Create)
					r.Get("/", deps.CustomAgentHandlers.List)
					r.Get("/{id}", deps.CustomAgentHandlers.Get)
					r.Put("/{id}", deps.CustomAgentHandlers.Update)
					r.Delete("/{id}", deps.CustomAgentHandlers.Delete)
					r.Get("/{id}/runs", deps.CustomAgentHandlers.ListRuns)
					if deps.CustomAgentHandlers.TriggerRun != nil {
						r.Post("/{id}/run", deps.CustomAgentHandlers.TriggerRun)
					}
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
						if r.Header.Get("X-API-Key") != svcKey {
							Error(w, http.StatusUnauthorized, "unauthorized", "invalid service key", nil)
							return
						}
						next.ServeHTTP(w, r)
					})
				})
				r.Route("/internal", func(r chi.Router) {
					r.Post("/leads", deps.InternalLeadHandlers.IngestFromService)
					if deps.ContactHandlers.GetByEmail != nil {
						r.Get("/contacts/by-email", deps.ContactHandlers.GetByEmail)
					}
					if deps.ContactHandlers.GetByPhone != nil {
						r.Get("/contacts/by-phone", deps.ContactHandlers.GetByPhone)
					}
					if deps.ContactHandlers.UpsertByPhone != nil {
						r.Post("/contacts/upsert", deps.ContactHandlers.UpsertByPhone)
					}
				})
			})
		}

		// Webhook routes (public, no auth — signature-verified by each handler)
		r.Route("/webhooks", func(r chi.Router) {
			r.Get("/meta/leadgen", deps.WebhookHandlers.MetaVerify)
			r.Post("/meta/leadgen", deps.WebhookHandlers.MetaLeadgen)
			r.Post("/notifications/delivery", deps.WebhookHandlers.NotificationsDelivery)
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

	return r
}
