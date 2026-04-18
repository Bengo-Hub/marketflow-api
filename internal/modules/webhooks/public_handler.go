package webhooks

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/funnels"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// TenantSyncer is the interface for synchronizing tenant data from auth-api.
// Defined here to avoid import cycles.
type TenantSyncer interface {
	SyncTenant(ctx context.Context, slug string) (uuid.UUID, error)
}

// PublicHandler handles public endpoints that require no authentication.
type PublicHandler struct {
	entClient          *ent.Client
	funnelService      *funnels.Service
	leadService        *leads.Service
	testimonialService TestimonialReader
	tenantSyncer       TenantSyncer
	logger             *zap.Logger
}

// TestimonialReader is the minimal surface area PublicHandler needs from the
// testimonials module (kept as an interface so we avoid an import cycle).
type TestimonialReader interface {
	PublicListForTenant(ctx context.Context, tenantID uuid.UUID, limit int) ([]map[string]any, error)
}

// NewPublicHandler creates a new public handler.
func NewPublicHandler(entClient *ent.Client, funnelService *funnels.Service, leadService *leads.Service, testimonialReader TestimonialReader, tenantSyncer TenantSyncer, logger *zap.Logger) *PublicHandler {
	return &PublicHandler{
		entClient:          entClient,
		funnelService:      funnelService,
		leadService:        leadService,
		testimonialService: testimonialReader,
		tenantSyncer:       tenantSyncer,
		logger:             logger.Named("public"),
	}
}

// GetProfile handles GET /api/v1/public/profile/{slug}
// @Summary Get tenant profile by slug (public)
// @Description Returns public tenant profile data without authentication
// @Tags Public
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/profile/{slug} [get]
func (h *PublicHandler) GetProfile(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	// Lookup priority: slug exact match first, then fbo_distrib_id fallback.
	// Two-step to avoid ambiguity when slug and distrib_id overlap across tenants.
	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		// Fallback: try fbo_distrib_id
		t, err = h.entClient.Tenant.Query().
			Where(tenant.FboDistribIDEQ(slug), tenant.StatusEQ("active")).
			First(r.Context())
		if err != nil {
			// Profile not found in local DB. Try JIT sync if a syncer is available.
			if h.tenantSyncer != nil {
				h.logger.Info("tenant not found locally, attempting JIT sync", zap.String("slug", slug))
				_, syncErr := h.tenantSyncer.SyncTenant(r.Context(), slug)
				if syncErr == nil {
					// Retry lookup after sync
					t, err = h.entClient.Tenant.Query().
						Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
						Only(r.Context())
					if err != nil {
						// Try distrib_id fallback again after sync
						t, err = h.entClient.Tenant.Query().
							Where(tenant.FboDistribIDEQ(slug), tenant.StatusEQ("active")).
							First(r.Context())
					}
				}
			}

			if err != nil || t == nil {
				httpapi.Error(w, http.StatusNotFound, "not_found", "profile not found", nil)
				return
			}
		}
	}

	// Return only public-safe fields
	profile := map[string]any{
		"id":            t.ID,
		"name":          t.Name,
		"slug":          t.Slug,
		"business_type": t.BusinessType,
	}
	if t.Country != nil {
		profile["country"] = *t.Country
	}
	if t.WhatsappNumber != nil {
		profile["whatsapp_number"] = *t.WhatsappNumber
	}
	if t.FboDistribID != nil {
		profile["fbo_distrib_id"] = *t.FboDistribID
	}
	if t.Metadata != nil {
		profile["metadata"] = t.Metadata
	}

	httpapi.JSON(w, http.StatusOK, profile)
}

// ListTestimonials handles GET /api/v1/public/profile/{slug}/testimonials.
// Returns up to N published testimonials for an FBO; read by the public
// /p/{slug} page.
// @Summary Public testimonials for a tenant slug
// @Tags Public
// @Produce json
// @Param slug path string true "Tenant slug"
// @Param limit query int false "Max rows (default 10, max 50)"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/profile/{slug}/testimonials [get]
func (h *PublicHandler) ListTestimonials(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}
	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "profile not found", nil)
		return
	}
	limit := 10
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	rows, err := h.testimonialService.PublicListForTenant(r.Context(), t.ID, limit)
	if err != nil {
		httpapi.Error(w, http.StatusInternalServerError, "query_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": rows})
}

// GetFunnel handles GET /api/v1/public/funnels/{slug} — JSON payload for
// programmatic consumers (e.g. funnel builder preview).
// @Summary Get published funnel by slug (JSON)
// @Tags Public
// @Produce json
// @Param slug path string true "Funnel URL slug"
// @Success 200 {object} map[string]interface{}
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/funnels/{slug} [get]
func (h *PublicHandler) GetFunnel(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}
	result, err := h.funnelService.GetBySlug(r.Context(), slug)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "funnel not found", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, result)
}

// RenderFunnel handles GET /f/{slug} — server-rendered HTML landing page.
//
// Renders `landing_page_html` if present; otherwise falls back to a built-in
// template that shows the funnel's steps + a form POSTing to
// /api/v1/public/leads/{tenant_slug}. Includes the embeddable chat widget.
// @Summary Render public funnel as HTML
// @Tags Public
// @Produce html
// @Param slug path string true "Funnel slug"
// @Success 200 {string} string "rendered HTML"
// @Router /f/{slug} [get]
func (h *PublicHandler) RenderFunnel(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		http.Error(w, "slug required", http.StatusBadRequest)
		return
	}
	f, err := h.funnelService.GetBySlug(r.Context(), slug)
	if err != nil {
		http.Error(w, "funnel not found", http.StatusNotFound)
		return
	}
	// Resolve the tenant slug for the lead POST target (widget-compatible).
	t, err := h.entClient.Tenant.Get(r.Context(), f.TenantID)
	if err != nil {
		http.Error(w, "tenant missing", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	// If the tenant supplied custom HTML via the builder, serve that. We still
	// inject the lead-capture JS + widget script at the end.
	if f.LandingPageHTML != "" {
		_, _ = w.Write([]byte(f.LandingPageHTML))
		_, _ = w.Write([]byte(funnelTail(t.Slug)))
		return
	}
	// Fallback: default template rendered from the structured fields.
	_, _ = w.Write([]byte(renderDefaultFunnel(f, t.Slug)))
}

func renderDefaultFunnel(f *ent.Funnel, tenantSlug string) string {
	brand := "#16a34a"
	if c, ok := f.Metadata["brand_color"].(string); ok && c != "" {
		brand = c
	}
	title := f.Name
	subtitle := "Tell us a bit about yourself and we'll reach out within 24 hours."
	if s, ok := f.Metadata["subtitle"].(string); ok && s != "" {
		subtitle = s
	}
	return fmt.Sprintf(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width,initial-scale=1" />
<title>%s</title>
<meta property="og:title" content="%s" />
<meta property="og:description" content="%s" />
<style>
  :root { --brand: %s; }
  * { box-sizing: border-box; }
  body { margin: 0; font-family: system-ui, -apple-system, Segoe UI, sans-serif; background: #f9fafb; color: #111827; }
  .wrap { max-width: 560px; margin: 0 auto; padding: 48px 20px; }
  h1 { font-size: 28px; margin: 0 0 8px; }
  p.sub { color: #6b7280; margin: 0 0 24px; }
  form { background: #fff; border: 1px solid #e5e7eb; border-radius: 12px; padding: 24px; box-shadow: 0 1px 3px rgba(0,0,0,.04); }
  label { display: block; font-size: 13px; color: #374151; margin-top: 12px; }
  input, textarea { width: 100%%; margin-top: 4px; padding: 10px 12px; border: 1px solid #d1d5db; border-radius: 8px; font-size: 14px; }
  button { margin-top: 18px; width: 100%%; padding: 12px 16px; background: var(--brand); color: #fff; border: 0; border-radius: 8px; font-size: 15px; font-weight: 600; cursor: pointer; }
  .done { text-align: center; padding: 40px 20px; color: #065f46; }
</style>
</head>
<body>
  <div class="wrap">
    <h1>%s</h1>
    <p class="sub">%s</p>
    <form id="mf-funnel" method="POST">
      <label>Name <input name="first_name" required autocomplete="given-name" /></label>
      <label>Email <input name="email" type="email" required autocomplete="email" /></label>
      <label>WhatsApp number <input name="phone" type="tel" autocomplete="tel" /></label>
      <label>What interests you? <textarea name="interest" rows="3" placeholder="Tell us what you're looking for..."></textarea></label>
      <button type="submit">Get started</button>
    </form>
    <div class="done" id="mf-done" style="display:none">
      Thanks — we'll be in touch shortly. Keep this tab open to chat with our AI assistant ↓
    </div>
  </div>
<script>
(function(){
  var tenantSlug = %q;
  var funnelSlug = %q;
  var form = document.getElementById('mf-funnel');
  var done = document.getElementById('mf-done');
  form.addEventListener('submit', function(e){
    e.preventDefault();
    var fd = new FormData(form);
    var body = {
      first_name: fd.get('first_name'),
      email: fd.get('email'),
      phone: fd.get('phone'),
      interest: fd.get('interest'),
      source: 'funnel',
      funnel_slug: funnelSlug,
      utm_source: new URLSearchParams(location.search).get('utm_source') || '',
      utm_medium: new URLSearchParams(location.search).get('utm_medium') || '',
      utm_campaign: new URLSearchParams(location.search).get('utm_campaign') || ''
    };
    fetch('/api/v1/public/leads/' + encodeURIComponent(tenantSlug), {
      method: 'POST',
      headers: {'Content-Type':'application/json'},
      body: JSON.stringify(body)
    }).then(function(r){ return r.ok; }).then(function(ok){
      if (ok) { form.style.display='none'; done.style.display='block'; }
    });
  });
})();
</script>%s
</body>
</html>`,
		htmlEscape(title), htmlEscape(title), htmlEscape(subtitle), brand,
		htmlEscape(title), htmlEscape(subtitle),
		tenantSlug, f.Slug, funnelTail(tenantSlug))
}

func funnelTail(tenantSlug string) string {
	return fmt.Sprintf(`
<script async src="/widget/chat.js" data-tenant=%q data-api-url=""></script>`, tenantSlug)
}

func htmlEscape(s string) string {
	r := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&#39;",
	)
	return r.Replace(s)
}

type publicLeadInput struct {
	Email       string         `json:"email"`
	Phone       string         `json:"phone"`
	FirstName   string         `json:"first_name"`
	LastName    string         `json:"last_name"`
	Source      string         `json:"source"`
	Interest    string         `json:"interest"`
	LeadType    string         `json:"lead_type"`
	UTMSource   string         `json:"utm_source"`
	UTMMedium   string         `json:"utm_medium"`
	UTMCampaign string         `json:"utm_campaign"`
	FunnelSlug  string         `json:"funnel_slug"`
	Answers     map[string]any `json:"answers"`
}

// PublicCreateLead handles POST /api/v1/public/leads/{slug}
// @Summary Public lead capture
// @Description Unauthenticated lead capture for the tenant identified by URL slug. Used by the embeddable chat widget and public funnels. Publishes `lead.created` to kick off the nurture loop.
// @Tags Public
// @Accept json
// @Produce json
// @Param slug path string true "Tenant URL slug"
// @Param request body publicLeadInput true "Lead data"
// @Success 201 {object} map[string]interface{}
// @Failure 400 {object} httpapi.ErrorResponse
// @Failure 404 {object} httpapi.ErrorResponse
// @Router /public/leads/{slug} [post]
func (h *PublicHandler) PublicCreateLead(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if slug == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_slug", "slug is required", nil)
		return
	}

	// Resolve tenant (active tenants only).
	t, err := h.entClient.Tenant.Query().
		Where(tenant.SlugEQ(slug), tenant.StatusEQ("active")).
		Only(r.Context())
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "tenant_not_found", "no tenant for slug", nil)
		return
	}

	var in publicLeadInput
	if err := httpapi.DecodeJSON(r, &in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid JSON body", nil)
		return
	}
	if in.Email == "" && in.Phone == "" {
		httpapi.Error(w, http.StatusBadRequest, "missing_contact", "email or phone is required", nil)
		return
	}

	source := in.Source
	if source == "" {
		source = "widget"
	}
	leadType := in.LeadType
	if leadType == "" {
		leadType = "customer"
	}

	input := leads.CreateInput{
		TenantID:             t.ID,
		Source:               source,
		LeadType:             leadType,
		QualificationAnswers: in.Answers,
		Metadata: map[string]any{
			"captured_via":    "public_endpoint",
			"captured_at":     time.Now().UTC().Format(time.RFC3339),
			"remote_addr":     r.RemoteAddr,
			"user_agent":      r.UserAgent(),
			"referer":         r.Referer(),
			"tenant_slug":     slug,
			"funnel_slug":     in.FunnelSlug,
		},
	}
	if in.Email != "" {
		e := in.Email
		input.Email = &e
	}
	if in.Phone != "" {
		p := in.Phone
		input.Phone = &p
	}
	if in.FirstName != "" {
		input.FirstName = &in.FirstName
	}
	if in.LastName != "" {
		input.LastName = &in.LastName
	}
	if in.Interest != "" {
		input.Interest = &in.Interest
	}
	if in.UTMSource != "" {
		input.UTMSource = &in.UTMSource
	}
	if in.UTMMedium != "" {
		input.UTMMedium = &in.UTMMedium
	}
	if in.UTMCampaign != "" {
		input.UTMCampaign = &in.UTMCampaign
	}

	lead, err := h.leadService.Create(r.Context(), input)
	if err != nil {
		h.logger.Error("public lead create failed",
			zap.String("slug", slug), zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "create_failed", "could not create lead", nil)
		return
	}

	// Lead creation already emits `lead.created` via the service's outbox hook;
	// marketflow-worker's LeadCreatedHandler picks it up and auto-enrols the lead.
	httpapi.JSON(w, http.StatusCreated, map[string]any{
		"id":         lead.ID,
		"tenant_id":  lead.TenantID,
		"status":     lead.Status,
		"created_at": lead.CreatedAt,
	})
}
