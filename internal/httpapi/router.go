package httpapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
)

// RouterDeps defines router construction dependencies.
type RouterDeps struct {
	HealthHandler  http.HandlerFunc
	MetricsHandler http.Handler

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

	// Analytics handlers (tenant dashboard aggregates)
	AnalyticsHandlers AnalyticsHandlers

	// Testimonial handlers (tenant-scoped CRUD for /p/{slug})
	TestimonialHandlers TestimonialHandlers

	// Integration handlers (OAuth authorize + callback for Google / Meta)
	IntegrationsHandlers IntegrationsHandlers

	// Tenant profile CRUD (FBO settings)
	TenantProfileHandlers TenantProfileHandlers

	// Auth middleware
	RequireAuthHandler      func(http.Handler) http.Handler
	RequirePlatformOwner    func(http.Handler) http.Handler

	// Feature gate middleware (subscription tier enforcement)
	RequireAICredits    func(http.Handler) http.Handler
	RequireCampaignSlot func(http.Handler) http.Handler

	// Usage tracking middleware (fire-and-forget to subscriptions-api)
	TrackLeadCreated    func(http.Handler) http.Handler
	TrackCampaignCreated func(http.Handler) http.Handler

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

// LeadHandlers groups lead management HTTP handlers.
type LeadHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
}

// ContactHandlers groups contact management HTTP handlers.
type ContactHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
}

// KnowledgeHandlers groups knowledge base HTTP handlers.
type KnowledgeHandlers struct {
	CreateEntry http.HandlerFunc
	ListEntries http.HandlerFunc
	GetEntry    http.HandlerFunc
	UpdateEntry http.HandlerFunc
	DeleteEntry http.HandlerFunc
}

// CampaignHandlers groups campaign management HTTP handlers.
type CampaignHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
	Pause  http.HandlerFunc
	Resume http.HandlerFunc
}

// FunnelHandlers groups funnel management HTTP handlers.
type FunnelHandlers struct {
	Create  http.HandlerFunc
	List    http.HandlerFunc
	Get     http.HandlerFunc
	Update  http.HandlerFunc
	Delete  http.HandlerFunc
	Publish http.HandlerFunc
}

// SequenceHandlers groups nurture sequence HTTP handlers.
type SequenceHandlers struct {
	Create http.HandlerFunc
	List   http.HandlerFunc
	Get    http.HandlerFunc
	Update http.HandlerFunc
	Delete http.HandlerFunc
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
	Dashboard        http.HandlerFunc
	TenantAudit      http.HandlerFunc
	PlatformUsage    http.HandlerFunc
	PlatformAudit    http.HandlerFunc
	CampaignMetrics  http.HandlerFunc
	SequenceMetrics  http.HandlerFunc
	ProfileMetrics   http.HandlerFunc
}

// IntegrationsHandlers groups OAuth authorize + callback endpoints
// (Google Calendar, Meta) and any related per-tenant integration management.
type IntegrationsHandlers struct {
	AvailableIntegrations http.HandlerFunc
	GoogleAuthorize       http.HandlerFunc
	GoogleCallback        http.HandlerFunc
	MetaAuthorize         http.HandlerFunc
	MetaCallback          http.HandlerFunc
	ListTenant            http.HandlerFunc
	DisconnectTenant      http.HandlerFunc
}

// WebhookHandlers groups webhook HTTP handlers (public, no auth).
type WebhookHandlers struct {
	MetaVerify           http.HandlerFunc
	MetaLeadgen          http.HandlerFunc
	CalcomBooking        http.HandlerFunc
	NotificationsDelivery http.HandlerFunc
}

// PublicHandlers groups public HTTP handlers (no auth).
type PublicHandlers struct {
	GetProfile          http.HandlerFunc
	GetFunnel           http.HandlerFunc
	RenderFunnel        http.HandlerFunc
	CreateLead          http.HandlerFunc
	RecordProfileEvent  http.HandlerFunc
	ListTestimonials    http.HandlerFunc
}

// TenantProfileHandlers groups tenant profile settings endpoints.
type TenantProfileHandlers struct {
	Get    http.HandlerFunc
	Update http.HandlerFunc
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

// NewRouter wires HTTP routes.
func NewRouter(deps RouterDeps) http.Handler {
	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.RealIP)
	r.Use(chimiddleware.Recoverer)
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
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-Request-ID", "X-Tenant-Slug", "X-Tenant-ID"},
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

			// Lead management
			r.Route("/leads", func(r chi.Router) {
				r.With(deps.TrackLeadCreated).Post("/", deps.LeadHandlers.Create)
				r.Get("/", deps.LeadHandlers.List)
				r.Get("/{id}", deps.LeadHandlers.Get)
				r.Put("/{id}", deps.LeadHandlers.Update)
				r.Delete("/{id}", deps.LeadHandlers.Delete)
			})

			// Contact management
			r.Route("/contacts", func(r chi.Router) {
				r.Post("/", deps.ContactHandlers.Create)
				r.Get("/", deps.ContactHandlers.List)
				r.Get("/{id}", deps.ContactHandlers.Get)
				r.Put("/{id}", deps.ContactHandlers.Update)
				r.Delete("/{id}", deps.ContactHandlers.Delete)
			})

			// Knowledge base management
			r.Route("/knowledge", func(r chi.Router) {
				r.Post("/entries", deps.KnowledgeHandlers.CreateEntry)
				r.Get("/entries", deps.KnowledgeHandlers.ListEntries)
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

			// Funnel management
			r.Route("/funnels", func(r chi.Router) {
				r.Post("/", deps.FunnelHandlers.Create)
				r.Get("/", deps.FunnelHandlers.List)
				r.Get("/{id}", deps.FunnelHandlers.Get)
				r.Put("/{id}", deps.FunnelHandlers.Update)
				r.Delete("/{id}", deps.FunnelHandlers.Delete)
				r.Post("/{id}/publish", deps.FunnelHandlers.Publish)
			})

			// Nurture sequence management
			r.Route("/sequences", func(r chi.Router) {
				r.Post("/", deps.SequenceHandlers.Create)
				r.Get("/", deps.SequenceHandlers.List)
				r.Get("/{id}", deps.SequenceHandlers.Get)
				r.Put("/{id}", deps.SequenceHandlers.Update)
				r.Delete("/{id}", deps.SequenceHandlers.Delete)
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
				r.Get("/meta/authorize", deps.IntegrationsHandlers.MetaAuthorize)
				r.Get("/meta/callback", deps.IntegrationsHandlers.MetaCallback)
				r.Get("/", deps.IntegrationsHandlers.ListTenant)
				r.Delete("/{provider}", deps.IntegrationsHandlers.DisconnectTenant)
			})

			// Short link management (authenticated)
			r.Route("/short-links", func(r chi.Router) {
				r.Post("/", deps.ShortLinkHandlers.Create)
				r.Get("/", deps.ShortLinkHandlers.List)
			})

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
			})
		})

		// Webhook routes (public, no auth — signature-verified by each handler)
		r.Route("/webhooks", func(r chi.Router) {
			r.Get("/meta/leadgen", deps.WebhookHandlers.MetaVerify)
			r.Post("/meta/leadgen", deps.WebhookHandlers.MetaLeadgen)
			r.Post("/calcom/booking", deps.WebhookHandlers.CalcomBooking)
			r.Post("/notifications/delivery", deps.WebhookHandlers.NotificationsDelivery)
		})

		// Public API routes (no auth). Lead capture is public and rate-limited
		// at the ingress layer; the handler enforces tenant lookup + basic validation.
		r.Route("/public", func(r chi.Router) {
			r.Get("/profile/{slug}", deps.PublicHandlers.GetProfile)
			r.Get("/profile/{slug}/testimonials", deps.PublicHandlers.ListTestimonials)
			r.Get("/funnels/{slug}", deps.PublicHandlers.GetFunnel)
			r.With(deps.TrackLeadCreated).Post("/leads/{slug}", deps.PublicHandlers.CreateLead)
			r.Post("/profile-events", deps.PublicHandlers.RecordProfileEvent)
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
