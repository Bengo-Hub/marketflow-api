package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
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
		authAPIURL = "https://sso.codevertexafrica.com"
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
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Slug         string         `json:"slug"`
	Status       string         `json:"status"`
	Country      string         `json:"country"`
	LogoURL      string         `json:"logo_url"`
	Website      string         `json:"website"`
	ContactEmail string         `json:"contact_email"`
	ContactPhone string         `json:"contact_phone"`
	UseCase      string         `json:"use_case"`
	UseCases     []string       `json:"use_cases"`
	OrgSize      string         `json:"org_size"`
	BrandColors  map[string]any `json:"brand_colors"`
	Metadata     map[string]any `json:"metadata"`
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

		// FBO defaults fill only missing keys. The local row's metadata is the base, so a
		// slug rename never drops profile edits or the Marketing OS brand context.
		if isFLPTenant(remote.Slug, remote.Metadata) {
			meta := applyFBODefaults(mergeMissing(existingByID.Metadata, remote.Metadata))
			upd = upd.SetMetadata(meta)
			if looksLikeDistribID(remote.Slug) {
				upd = upd.SetFboDistribID(remote.Slug)
			}
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

	// Only Forever distributors are FLP. The schema default is "flp", so it is set
	// explicitly here; before 2026-10-23 every synced tenant was created as FLP.
	flp := isFLPTenant(remote.Slug, remote.Metadata)
	businessType := BusinessTypeGeneric
	if flp {
		businessType = BusinessTypeFLP
	}

	builder := s.client.Tenant.Create().
		SetID(realID).
		SetSlug(remote.Slug).
		SetName(name).
		SetStatus(remote.Status).
		SetBusinessType(businessType)

	if remote.Country != "" {
		builder = builder.SetCountry(remote.Country)
	}

	// For FBO tenants, the slug IS the distrib_id. Apply defaults, including the
	// featured Forever products.
	if flp {
		if looksLikeDistribID(remote.Slug) {
			builder = builder.SetFboDistribID(remote.Slug)
		}
		meta = applyFBODefaults(meta)
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

// EnsureWalkInForAllTenants backfills the shared Walk-in customer for every existing tenant. Run once
// at boot so tenants synced BEFORE walk-in seeding existed (e.g. the demo tenant) also get one, rather
// than relying on lazy on-demand creation. Idempotent and best-effort.
func (s *Syncer) EnsureWalkInForAllTenants(ctx context.Context) {
	tenants, err := s.client.Tenant.Query().All(ctx)
	if err != nil {
		s.logger.Warn("CRM: walk-in backfill — list tenants failed", zap.Error(err))
		return
	}
	for _, t := range tenants {
		s.createWalkInContactIfMissing(ctx, t.ID)
	}
	s.logger.Info("CRM: walk-in backfill complete", zap.Int("tenants", len(tenants)))
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

// AuthProfile is the business identity auth-api holds for a tenant, read to pre-fill
// MarketFlow's profile and brand context. auth-api stays its owner; nothing here is stored.
type AuthProfile struct {
	Name         string   `json:"name"`
	Country      string   `json:"country,omitempty"`
	LogoURL      string   `json:"logo_url,omitempty"`
	Website      string   `json:"website,omitempty"`
	ContactEmail string   `json:"contact_email,omitempty"`
	ContactPhone string   `json:"contact_phone,omitempty"`
	UseCases     []string `json:"use_cases,omitempty"`
	OrgSize      string   `json:"org_size,omitempty"`
	PrimaryColor string   `json:"primary_color,omitempty"`
}

// AuthProfile reads a tenant's business details from auth-api, bounded by ctx.
func (s *Syncer) AuthProfile(ctx context.Context, slug string) (*AuthProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/v1/tenants/by-slug/%s", s.authAPIURL, url.PathEscape(slug)), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch tenant from auth-api: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("auth-api returned %d for tenant %q", resp.StatusCode, slug)
	}
	var t authTenant
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("decode auth-api response: %w", err)
	}
	p := &AuthProfile{
		Name: t.Name, Country: t.Country, LogoURL: t.LogoURL, Website: t.Website,
		ContactEmail: t.ContactEmail, ContactPhone: t.ContactPhone, OrgSize: t.OrgSize,
	}
	for _, u := range append([]string{t.UseCase}, t.UseCases...) {
		if u = strings.TrimSpace(u); u != "" && !slices.Contains(p.UseCases, u) {
			p.UseCases = append(p.UseCases, u)
		}
	}
	if c, ok := t.BrandColors["primary"].(string); ok {
		p.PrimaryColor = c
	}
	return p, nil
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
