package admin

import (
	"encoding/base64"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

const (
	encryptionKeyConfigKey  = encryption.DBKeyConfigKey
	encryptionKeyConfigDesc = "AES-256-GCM key for provider-secret encryption at rest"
)

// EncryptionKeyHandler exposes platform-owner endpoints to view and rotate the
// DB-stored provider-secret encryption key. The raw key is NEVER returned and
// NEVER logged — only a non-reversible fingerprint and metadata are surfaced.
type EncryptionKeyHandler struct {
	client   *ent.Client
	provider *encryption.KeyProvider
	logger   *zap.Logger
}

// NewEncryptionKeyHandler creates a new EncryptionKeyHandler.
func NewEncryptionKeyHandler(client *ent.Client, provider *encryption.KeyProvider, logger *zap.Logger) *EncryptionKeyHandler {
	return &EncryptionKeyHandler{
		client:   client,
		provider: provider,
		logger:   logger.Named("admin.encryption-key"),
	}
}

type encryptionKeyResponse struct {
	Configured     bool    `json:"configured"`
	Source         string  `json:"source"`          // "db" | "env" | ""
	KeyFingerprint string  `json:"key_fingerprint"` // first 8 hex of sha256(primary key), "" if none
	UpdatedAt      *string `json:"updated_at"`      // RFC3339 of the DB row, null when no DB key
}

// status builds the response shape shared by GET and PUT. It reads the DB row
// (if any) for updated_at and derives the fingerprint/source via the provider.
func (h *EncryptionKeyHandler) status(r *http.Request) encryptionKeyResponse {
	ctx := r.Context()

	resp := encryptionKeyResponse{
		Configured:     h.provider.Configured(ctx),
		Source:         h.provider.Source(ctx),
		KeyFingerprint: h.provider.PrimaryFingerprint(ctx),
	}

	if row, err := h.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(encryptionKeyConfigKey),
			serviceconfig.TenantIDIsNil(),
		).
		Only(ctx); err == nil && row != nil {
		ts := row.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")
		resp.UpdatedAt = &ts
	}

	return resp
}

// GetEncryptionKey returns the current encryption-key status. The raw key is
// never included.
// GET /api/v1/admin/encryption-key
func (h *EncryptionKeyHandler) GetEncryptionKey(w http.ResponseWriter, r *http.Request) {
	httpapi.JSON(w, http.StatusOK, h.status(r))
}

type setEncryptionKeyRequest struct {
	Key      string `json:"key,omitempty"`
	Generate bool   `json:"generate,omitempty"`
}

// SetEncryptionKey upserts the platform-level encryption key. A key may be
// supplied (base64 of exactly 32 bytes) or generated server-side. The raw key
// is never returned or logged.
// PUT /api/v1/admin/encryption-key
func (h *EncryptionKeyHandler) SetEncryptionKey(w http.ResponseWriter, r *http.Request) {
	var req setEncryptionKeyRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}

	var keyB64 string
	switch {
	case req.Generate || req.Key == "":
		generated, err := encryption.GenerateKey()
		if err != nil {
			h.logger.Error("failed to generate encryption key", zap.Error(err))
			httpapi.Error(w, http.StatusInternalServerError, "generate_failed", "failed to generate key", nil)
			return
		}
		keyB64 = generated
	default:
		if encryption.DecodeKey(req.Key) == nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_key", "key must base64-decode to exactly 32 bytes", nil)
			return
		}
		// Normalize to base64-std for storage.
		keyB64 = base64Std(req.Key)
	}

	ctx := r.Context()

	existing, _ := h.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(encryptionKeyConfigKey),
			serviceconfig.TenantIDIsNil(),
		).
		Only(ctx)

	var err error
	if existing != nil {
		_, err = existing.Update().
			SetConfigValue(keyB64).
			SetConfigType("string").
			SetIsSecret(true).
			SetDescription(encryptionKeyConfigDesc).
			Save(ctx)
	} else {
		// tenant_id left unset → NULL (platform-level).
		_, err = h.client.ServiceConfig.Create().
			SetConfigKey(encryptionKeyConfigKey).
			SetConfigValue(keyB64).
			SetConfigType("string").
			SetIsSecret(true).
			SetDescription(encryptionKeyConfigDesc).
			Save(ctx)
	}
	if err != nil {
		h.logger.Error("failed to upsert encryption key", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "failed to save encryption key", nil)
		return
	}

	// Refresh resolution so the new key takes effect immediately.
	h.provider.Invalidate()

	// Audit (no key material).
	actor := "system"
	if claims, ok := authclient.ClaimsFromContext(ctx); ok && claims != nil {
		if claims.Email != "" {
			actor = claims.Email
		} else if claims.Subject != "" {
			actor = claims.Subject
		}
	}
	rotated := existing != nil
	_, _ = h.client.AuditLog.Create().
		SetScope("platform").
		SetEvent("admin.encryption_key_set").
		SetActor(actor).
		SetSubjectType("service_config").
		SetPayload(map[string]any{
			"config_key": encryptionKeyConfigKey,
			"rotated":    rotated,
			"generated":  req.Generate || req.Key == "",
		}).
		Save(ctx)

	httpapi.JSON(w, http.StatusOK, h.status(r))
}

// base64Std re-encodes a base64-std-or-url 32-byte key to canonical base64-std.
// Caller has already validated it decodes to 32 bytes.
func base64Std(b64 string) string {
	if key := encryption.DecodeKey(b64); key != nil {
		return base64.StdEncoding.EncodeToString(key)
	}
	return b64
}
