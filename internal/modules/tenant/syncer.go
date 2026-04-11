package tenant

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	enttenant "github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Syncer handles JIT syncing of tenant data from auth-api.
// On first encounter of a tenant slug (from JWT), it fetches the canonical
// tenant record from auth-api and upserts locally.
type Syncer struct {
	client     *ent.Client
	authAPIURL string
	httpClient *http.Client
	logger     *zap.Logger
}

// NewSyncer creates a new TenantSyncer.
func NewSyncer(client *ent.Client, logger *zap.Logger) *Syncer {
	authAPIURL := os.Getenv("AUTH_API_URL")
	if authAPIURL == "" {
		authAPIURL = "https://sso.codevertexitsolutions.com"
	}
	return &Syncer{
		client:     client,
		authAPIURL: authAPIURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		logger:     logger.Named("tenant.syncer"),
	}
}

// SyncTenant fetches the tenant from auth-api (by slug) and upserts locally.
// Returns the canonical UUID from auth-api.
func (s *Syncer) SyncTenant(ctx context.Context, slug string) (uuid.UUID, error) {
	// Fast path: check if tenant already exists locally
	existing, err := s.client.Tenant.Query().Where(enttenant.SlugEQ(slug)).Only(ctx)
	if err == nil {
		return existing.ID, nil
	}

	// Fetch from auth-api public endpoint (no auth required)
	url := fmt.Sprintf("%s/api/v1/tenants/by-slug/%s", s.authAPIURL, slug)
	resp, err := s.httpClient.Get(url)
	if err != nil {
		return uuid.Nil, fmt.Errorf("fetch tenant from auth-api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return uuid.Nil, fmt.Errorf("auth-api returned %d for tenant %q", resp.StatusCode, slug)
	}

	var remote struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Slug    string `json:"slug"`
		Status  string `json:"status"`
		Country string `json:"country"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&remote); err != nil {
		return uuid.Nil, fmt.Errorf("decode auth-api response: %w", err)
	}

	realID, err := uuid.Parse(remote.ID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid tenant UUID %q: %w", remote.ID, err)
	}

	// Check if exists by UUID (may have been created with different slug)
	existingByID, _ := s.client.Tenant.Get(ctx, realID)
	if existingByID != nil {
		return realID, nil
	}

	// Create minimal tenant reference
	_, err = s.client.Tenant.Create().
		SetID(realID).
		SetSlug(remote.Slug).
		SetName(remote.Name).
		SetStatus(remote.Status).
		SetBusinessType("flp").
		Save(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("create tenant: %w", err)
	}

	s.logger.Info("tenant synced from auth-api",
		zap.String("slug", slug),
		zap.String("id", realID.String()),
	)

	return realID, nil
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
