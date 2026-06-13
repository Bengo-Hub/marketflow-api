package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// FBO Profile Defaults
	DefaultFBOTagline      = "Forever Business Owner."
	DefaultFBOBio          = "Forever Living Products is the largest grower, manufacturer and distributor of Aloe Vera products in the world. For over 46 years, we have been dedicated to seeking out the best sources from nature for health and vitality, and sharing them with the world."
	DefaultFBOStory        = "Forever Living Products was founded in 1978 by Rex Maughan in Scottsdale, Arizona. We are the world's largest grower, manufacturer and distributor of aloe vera and bee products. Millions of people in over 160 countries have discovered the benefits of Forever’s aloe vera products to help them and their families look better and feel better. We own our own aloe fields, manufacturing and distribution facilities, and we utilize stabilized aloe vera gel because we control every step from plant to product to you."
	DefaultFBOVideoURL     = "https://www.youtube.com/embed/YRvN_JkHSzQ"
	DefaultFBOPrimaryColor = "#f59e0b"
	DefaultFBOLogo         = "https://accounts.codevertexitsolutions.com/images/logo/codevertex.png"
	DefaultFBOOpportunity  = "Forever Business Owners are everyday people who choose to share products they enjoy. Some may earn supplemental income by building a customer base or team, but participation and effort vary from person to person. Our business model rewards individuals for product sales and for supporting others who choose to do the same.\n\nForever’s reward structure, known as the Marketing Plan, outlines the various roles, incentives, and bonuses available. It is a roadmap that explains how the business works and the requirements associated with each level. It offers tools and resources designed to support your activity, allowing you to work at your own pace."
)

// Syncer handles JIT syncing of tenant data from auth-api.
// On first encounter of a tenant slug (from JWT), it fetches the canonical
// tenant record from auth-api and upserts locally using the SAME UUID so
// all services share a consistent tenant identifier.
type Syncer struct {
	client             *ent.Client
	authAPIURL         string
	platformTenantSlug string
	httpClient         *http.Client
	logger             *zap.Logger
}

// NewSyncer creates a new TenantSyncer.
func NewSyncer(client *ent.Client, logger *zap.Logger) *Syncer {
	authAPIURL := os.Getenv("AUTH_API_URL")
	if authAPIURL == "" {
		authAPIURL = "https://sso.codevertexitsolutions.com"
	}
	platformSlug := os.Getenv("PLATFORM_TENANT")
	if platformSlug == "" {
		platformSlug = "codevertex"
	}
	return &Syncer{
		client:             client,
		authAPIURL:         authAPIURL,
		platformTenantSlug: platformSlug,
		httpClient:         &http.Client{Timeout: 10 * time.Second},
		logger:             logger.Named("tenant.syncer"),
	}
}

// authTenant is the shape returned by auth-api /api/v1/tenants/by-slug/{slug}.
type authTenant struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Slug     string         `json:"slug"`
	Status   string         `json:"status"`
	Country  string         `json:"country"`
	LogoURL  string         `json:"logo_url"`
	Metadata map[string]any `json:"metadata"`
	// Auth-api may include the owner user's profile in the response.
	Owner *struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Email     string `json:"email"`
		Phone     string `json:"phone"`
	} `json:"owner,omitempty"`
}

// SyncTenant fetches the tenant from auth-api (by slug) and upserts locally
// using the same UUID. Returns the canonical UUID from auth-api.
func (s *Syncer) SyncTenant(ctx context.Context, slug string) (uuid.UUID, error) {
	// Fast path: check if tenant already exists locally by slug
	existing, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).Only(ctx)
	if err == nil {
		return existing.ID, nil
	}

	// Fetch from auth-api
	remote, err := s.fetchFromAuth(slug)
	if err != nil {
		return uuid.Nil, err
	}

	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid tenant UUID %q: %w", remote.ID, err)
	}

	// Check if exists by UUID (may have been created via NATS event)
	existingByID, _ := s.client.Tenant.Get(ctx, realID)
	if existingByID != nil {
		// Update with latest data from auth-api
		upd := s.client.Tenant.UpdateOneID(realID).
			SetSlug(remote.Slug).
			SetName(s.resolveName(remote)).
			SetStatus(remote.Status)

		if remote.Country != "" {
			upd = upd.SetCountry(remote.Country)
		}

		// Update WhatsApp if available
		if remote.Owner != nil && remote.Owner.Phone != "" {
			if clean, err := CleanPhone(remote.Owner.Phone); err == nil && clean != "" {
				upd = upd.SetWhatsappNumber(clean)
			}
		}

		// Update FBO metadata defaults
		if looksLikeDistribID(remote.Slug) {
			meta := remote.Metadata
			if meta == nil {
				meta = make(map[string]any)
			}
			if _, ok := meta["tagline"]; !ok {
				meta["tagline"] = DefaultFBOTagline
			}
			if _, ok := meta["bio"]; !ok {
				meta["bio"] = DefaultFBOBio
			}
			if _, ok := meta["story"]; !ok {
				meta["story"] = DefaultFBOStory
			}
			if _, ok := meta["video_url"]; !ok {
				meta["video_url"] = DefaultFBOVideoURL
			}
			if _, ok := meta["brand_primary_color"]; !ok {
				meta["brand_primary_color"] = DefaultFBOPrimaryColor
			}
			if _, ok := meta["logo_url"]; !ok {
				meta["logo_url"] = DefaultFBOLogo
			}
			if _, ok := meta["opportunity_text"]; !ok {
				meta["opportunity_text"] = DefaultFBOOpportunity
			}
			upd = upd.SetMetadata(meta).SetFboDistribID(remote.Slug)
		}

		if _, err := upd.Save(ctx); err != nil {
			s.logger.Warn("tenant update failed", zap.String("slug", slug), zap.Error(err))
		}
		return realID, nil
	}

	// Create tenant with the same UUID as auth-api
	name := s.resolveName(remote)
	meta := remote.Metadata
	if meta == nil {
		meta = make(map[string]any)
	}

	builder := s.client.Tenant.Create().
		SetID(realID).
		SetSlug(remote.Slug).
		SetName(name).
		SetStatus(remote.Status).
		SetBusinessType("flp")

	if remote.Country != "" {
		builder = builder.SetCountry(remote.Country)
	}

	// For FBO tenants, the slug IS the distrib_id. Apply defaults.
	if looksLikeDistribID(remote.Slug) {
		builder = builder.SetFboDistribID(remote.Slug)
		if _, ok := meta["tagline"]; !ok {
			meta["tagline"] = DefaultFBOTagline
		}
		if _, ok := meta["bio"]; !ok {
			meta["bio"] = DefaultFBOBio
		}
		if _, ok := meta["story"]; !ok {
			meta["story"] = DefaultFBOStory
		}
		if _, ok := meta["video_url"]; !ok {
			meta["video_url"] = DefaultFBOVideoURL
		}
		if _, ok := meta["brand_primary_color"]; !ok {
			meta["brand_primary_color"] = DefaultFBOPrimaryColor
		}
		if _, ok := meta["logo_url"]; !ok {
			meta["logo_url"] = DefaultFBOLogo
		}
		if _, ok := meta["opportunity_text"]; !ok {
			meta["opportunity_text"] = DefaultFBOOpportunity
		}
	}

	builder = builder.SetMetadata(meta)

	// Sync owner phone as WhatsApp number (cleaned + validated)
	if remote.Owner != nil && remote.Owner.Phone != "" {
		if clean, err := CleanPhone(remote.Owner.Phone); err == nil && clean != "" {
			builder = builder.SetWhatsappNumber(clean)
		}
	}

	if _, err := builder.Save(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("create tenant: %w", err)
	}

	s.logger.Info("tenant synced from auth-api",
		zap.String("slug", slug),
		zap.String("id", realID.String()),
		zap.String("name", name),
	)

	// Record the tenant's owner (the tenant admin) as a CUSTOMER in the PLATFORM CRM — a tenant is
	// Codevertex's customer at the platform level. Tenant end-customers are synced separately into
	// their own tenant's CRM (by ordering/pos on transaction). Best-effort; never fails the sync.
	s.upsertPlatformCustomer(ctx, remote)

	// Seed the tenant's shared "Walk-in" customer so over-the-counter sales without a named customer
	// (e.g. pos on-account) can be attributed to it. Idempotent; best-effort. pos-api/treasury also
	// upsert/lookup this same contact by walkInPhone on demand, so existing tenants self-heal too.
	s.createWalkInContactIfMissing(ctx, realID)

	return realID, nil
}

// upsertPlatformCustomer records a tenant's owner as a customer contact under the PLATFORM tenant's
// CRM scope. Idempotent (dedups by email/phone). Skips the platform tenant itself and owners with no
// contact info. This is how tenant ADMINS (Codevertex's customers) reach the CRM — distinct from
// tenant END-customers (synced into their tenant's CRM on order) and from platform owners / tenant
// staff, who are never recorded as customers.
func (s *Syncer) upsertPlatformCustomer(ctx context.Context, remote *authTenant) {
	if remote == nil || remote.Owner == nil {
		return
	}
	owner := remote.Owner
	if owner.Email == "" && owner.Phone == "" {
		return
	}
	if s.platformTenantSlug == "" || remote.Slug == s.platformTenantSlug {
		return // don't record the platform tenant's own owner as its own customer
	}

	platform, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(s.platformTenantSlug)).Only(ctx)
	if err != nil {
		s.logger.Warn("platform CRM: platform tenant not found; skipping owner contact",
			zap.String("platform_slug", s.platformTenantSlug), zap.Error(err))
		return
	}

	cleanPhone := ""
	if owner.Phone != "" {
		if c, perr := CleanPhone(owner.Phone); perr == nil {
			cleanPhone = c
		}
	}

	// Dedup by email or phone within the platform tenant.
	if owner.Email != "" {
		if _, e := s.client.Contact.Query().Where(contact.TenantID(platform.ID), contact.EmailEQ(owner.Email)).First(ctx); e == nil {
			return
		}
	}
	if cleanPhone != "" {
		if _, e := s.client.Contact.Query().Where(contact.TenantID(platform.ID), contact.PhoneEQ(cleanPhone)).First(ctx); e == nil {
			return
		}
	}

	builder := s.client.Contact.Create().SetTenantID(platform.ID).SetContactType("customer")
	if owner.Email != "" {
		builder = builder.SetEmail(owner.Email)
	}
	if cleanPhone != "" {
		builder = builder.SetPhone(cleanPhone)
	}
	if owner.FirstName != "" {
		builder = builder.SetFirstName(owner.FirstName)
	}
	if owner.LastName != "" {
		builder = builder.SetLastName(owner.LastName)
	}
	if _, err := builder.Save(ctx); err != nil {
		s.logger.Warn("platform CRM: upsert owner contact failed",
			zap.String("tenant_slug", remote.Slug), zap.Error(err))
		return
	}
	s.logger.Info("platform CRM: tenant owner recorded as platform customer",
		zap.String("tenant_slug", remote.Slug))
}

// walkInPhone is the well-known sentinel identifier for the shared per-tenant "Walk-in" customer.
// pos-api/treasury upsert and look this contact up by this exact phone, so it must stay in sync.
const walkInPhone = "+000000000000"

// createWalkInContactIfMissing seeds a shared "Walk-in Customer" contact for a tenant (idempotent by
// phone) so transactions that need a customer but have none can be attributed to it. Best-effort.
func (s *Syncer) createWalkInContactIfMissing(ctx context.Context, tenantID uuid.UUID) {
	if _, e := s.client.Contact.Query().
		Where(contact.TenantID(tenantID), contact.PhoneEQ(walkInPhone)).First(ctx); e == nil {
		return // already exists
	}
	if _, err := s.client.Contact.Create().
		SetTenantID(tenantID).
		SetContactType("customer").
		SetFirstName("Walk-in").
		SetLastName("Customer").
		SetPhone(walkInPhone).
		SetMetadata(map[string]any{"is_default_walkin": true}).
		Save(ctx); err != nil {
		s.logger.Warn("CRM: seed walk-in customer failed",
			zap.String("tenant_id", tenantID.String()), zap.Error(err))
		return
	}
	s.logger.Info("CRM: seeded walk-in customer", zap.String("tenant_id", tenantID.String()))
}

// resolveName picks the best display name: owner full name > tenant name.
func (s *Syncer) resolveName(remote *authTenant) string {
	if remote.Owner != nil {
		full := ""
		if remote.Owner.FirstName != "" {
			full = remote.Owner.FirstName
		}
		if remote.Owner.LastName != "" {
			if full != "" {
				full += " "
			}
			full += remote.Owner.LastName
		}
		if full != "" {
			return full
		}
	}
	return remote.Name
}

func (s *Syncer) fetchFromAuth(slug string) (*authTenant, error) {
	url := fmt.Sprintf("%s/api/v1/tenants/by-slug/%s", s.authAPIURL, slug)
	resp, err := s.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetch tenant from auth-api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth-api returned %d for tenant %q", resp.StatusCode, slug)
	}
	var remote authTenant
	if err := json.NewDecoder(resp.Body).Decode(&remote); err != nil {
		return nil, fmt.Errorf("decode auth-api response: %w", err)
	}
	return &remote, nil
}

// GetOrSync returns a tenant by ID, syncing by slug if not found locally.
func (s *Syncer) GetOrSync(ctx context.Context, tenantID uuid.UUID, slug string) (*ent.Tenant, error) {
	t, err := s.client.Tenant.Get(ctx, tenantID)
	if err == nil {
		return t, nil
	}
	if slug != "" {
		_, syncErr := s.SyncTenant(ctx, slug)
		if syncErr != nil {
			return nil, fmt.Errorf("sync tenant %q: %w", slug, syncErr)
		}
		return s.client.Tenant.Get(ctx, tenantID)
	}
	return nil, fmt.Errorf("tenant %s not found and no slug for sync", tenantID)
}

// looksLikeDistribID checks if a slug is a numeric FBO distributor ID.
func looksLikeDistribID(s string) bool {
	if len(s) < 8 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
