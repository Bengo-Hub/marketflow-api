package shortlinks

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/shortlink"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// alphabet for short codes — URL-safe, avoids ambiguous chars (0/O, 1/l/I)
	alphabet = "23456789abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ"
	codeLen  = 7
)

// Service handles short link operations.
type Service struct {
	client  *ent.Client
	logger  *zap.Logger
	baseURL string
}

// NewService creates a new short link service.
func NewService(client *ent.Client, logger *zap.Logger, baseURL string) *Service {
	if baseURL == "" {
		baseURL = "https://marketflowapi.codevertexitsolutions.com"
	}
	return &Service{
		client:  client,
		logger:  logger.Named("shortlinks"),
		baseURL: baseURL,
	}
}

// CreateInput contains data to create a short link.
type CreateInput struct {
	TenantID   uuid.UUID
	TargetURL  string
	LinkType   string
	Title      *string
	CampaignID *uuid.UUID
	ExpiresAt  *time.Time
}

// Create generates a short link for the given target URL.
func (s *Service) Create(ctx context.Context, in CreateInput) (*ent.ShortLink, error) {
	code, err := s.generateUniqueCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("generate short code: %w", err)
	}

	if in.LinkType == "" {
		in.LinkType = "custom"
	}

	builder := s.client.ShortLink.Create().
		SetTenantID(in.TenantID).
		SetCode(code).
		SetTargetURL(in.TargetURL).
		SetLinkType(in.LinkType)

	if in.Title != nil {
		builder.SetNillableTitle(in.Title)
	}
	if in.CampaignID != nil {
		builder.SetNillableCampaignID(in.CampaignID)
	}
	if in.ExpiresAt != nil {
		builder.SetNillableExpiresAt(in.ExpiresAt)
	}

	result, err := builder.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("create short link: %w", err)
	}

	s.logger.Info("short link created",
		zap.String("code", code),
		zap.String("type", in.LinkType),
		zap.String("short_url", s.ShortURL(code)),
	)

	return result, nil
}

// Resolve finds the target URL for a short code and increments click count.
func (s *Service) Resolve(ctx context.Context, code string) (string, error) {
	link, err := s.client.ShortLink.Query().
		Where(
			shortlink.CodeEQ(code),
			shortlink.IsActiveEQ(true),
		).
		Only(ctx)
	if err != nil {
		return "", fmt.Errorf("short link not found: %w", err)
	}

	// Check expiration
	if link.ExpiresAt != nil && time.Now().After(*link.ExpiresAt) {
		return "", fmt.Errorf("short link expired")
	}

	// Increment click count (fire-and-forget, don't block redirect)
	go func() {
		_ = link.Update().
			AddClickCount(1).
			SetLastClickedAt(time.Now()).
			Exec(context.Background())
	}()

	return link.TargetURL, nil
}

// ShortURL returns the full short URL for a code.
func (s *Service) ShortURL(code string) string {
	return s.baseURL + "/s/" + code
}

// ListByTenant returns short links for a tenant.
func (s *Service) ListByTenant(ctx context.Context, tenantID uuid.UUID, linkType *string, limit, offset int) ([]*ent.ShortLink, int, error) {
	query := s.client.ShortLink.Query().
		Where(shortlink.TenantID(tenantID))

	if linkType != nil {
		query = query.Where(shortlink.LinkTypeEQ(*linkType))
	}

	total, err := query.Clone().Count(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("count short links: %w", err)
	}

	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	results, err := query.
		Order(ent.Desc(shortlink.FieldCreatedAt)).
		Limit(limit).
		Offset(offset).
		All(ctx)
	if err != nil {
		return nil, 0, fmt.Errorf("list short links: %w", err)
	}

	return results, total, nil
}

// generateUniqueCode generates a random short code and verifies uniqueness.
func (s *Service) generateUniqueCode(ctx context.Context) (string, error) {
	for attempts := 0; attempts < 10; attempts++ {
		code := randomCode(codeLen)
		exists, err := s.client.ShortLink.Query().
			Where(shortlink.CodeEQ(code)).
			Exist(ctx)
		if err != nil {
			return "", err
		}
		if !exists {
			return code, nil
		}
	}
	return "", fmt.Errorf("failed to generate unique code after 10 attempts")
}

// randomCode generates a cryptographically random short code.
func randomCode(length int) string {
	b := make([]byte, length)
	alphabetLen := big.NewInt(int64(len(alphabet)))
	for i := range b {
		n, _ := rand.Int(rand.Reader, alphabetLen)
		b[i] = alphabet[n.Int64()]
	}
	return string(b)
}
