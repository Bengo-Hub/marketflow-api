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

	// Short link handlers
	ShortLinkHandlers ShortLinkHandlers

	// Admin provider handlers (platform owner only)
	AdminProviderHandlers AdminProviderHandlers

	// Auth middleware
	RequireAuthHandler      func(http.Handler) http.Handler
	RequirePlatformOwner    func(http.Handler) http.Handler

	// Allowed origins for CORS
	AllowedOrigins string
}

// AdminProviderHandlers groups platform provider management handlers.
type AdminProviderHandlers struct {
	Configure   http.HandlerFunc
	List        http.HandlerFunc
	GetSettings http.HandlerFunc
	Deactivate  http.HandlerFunc
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
				r.Post("/", deps.LeadHandlers.Create)
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

			// Campaign management
			r.Route("/campaigns", func(r chi.Router) {
				r.Post("/", deps.CampaignHandlers.Create)
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
					r.Delete("/{id}", deps.AdminProviderHandlers.Deactivate)
				})
			})
		})
	})

	// Public short link redirect (no auth required)
	// GET /s/{code} → 302 redirect to target URL
	r.Get("/s/{code}", deps.ShortLinkHandlers.Redirect)

	// Public funnel hosting (no auth required)
	r.Get("/f/{slug}", func(w http.ResponseWriter, r *http.Request) {
		// TODO: Serve published funnel page by slug
		JSON(w, http.StatusOK, map[string]any{"message": "funnel page", "slug": chi.URLParam(r, "slug")})
	})

	// Public FBO profile page (no auth required)
	r.Get("/p/{slug}", func(w http.ResponseWriter, r *http.Request) {
		// TODO: Serve dynamic FBO profile page by tenant slug (Sprint 3)
		JSON(w, http.StatusOK, map[string]any{"message": "FBO profile page", "slug": chi.URLParam(r, "slug")})
	})

	return r
}
