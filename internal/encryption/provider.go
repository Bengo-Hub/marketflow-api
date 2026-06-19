package encryption

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

// DBKeyConfigKey is the ServiceConfig.config_key under which the platform-owner
// configurable AES-256-GCM encryption key is stored (config_value = base64-std
// of exactly 32 bytes, tenant_id IS NULL).
const DBKeyConfigKey = "encryption_key"

// cacheTTL bounds how long resolved candidate keys are cached before the DB is
// re-read. Kept short so a key rotation via the admin endpoint takes effect
// promptly even without an explicit Invalidate().
const cacheTTL = 30 * time.Second

// KeyProvider resolves the ordered list of candidate AES-256 keys used to
// encrypt/decrypt provider secrets at rest. Resolution is DB-first with an
// env fallback:
//
//	(a) DB key  — ServiceConfig{config_key=encryption_key, tenant_id IS NULL},
//	              config_value = base64-std of 32 bytes  → PRIMARY (candidates[0])
//	(b) ENV key — KeyFromEnv(cfg.Security.EncryptionKey)
//
// If neither is present the candidate list is empty and encryption is disabled,
// exactly as before this provider existed. Candidates are deduped (a key that
// appears in both DB and env is listed once) and cached for cacheTTL.
type KeyProvider struct {
	client *ent.Client
	envKey []byte // pre-decoded 32-byte env key, or nil

	mu         sync.RWMutex
	cached     [][]byte
	cachedFrom keySource
	cachedAt   time.Time
}

// keySource identifies where the PRIMARY key came from.
type keySource string

const (
	sourceNone keySource = ""
	sourceDB   keySource = "db"
	sourceEnv  keySource = "env"
)

// NewKeyProvider builds a KeyProvider from the ent client and the raw env config
// value (base64 or hex). The env value is decoded once; an invalid/empty value
// yields a nil env key (no env candidate).
func NewKeyProvider(client *ent.Client, envKeyB64OrHex string) *KeyProvider {
	return &KeyProvider{
		client: client,
		envKey: KeyFromEnv(envKeyB64OrHex),
	}
}

// Invalidate clears the cache so the next resolution re-reads the DB. Call after
// the admin endpoint upserts a new key.
func (p *KeyProvider) Invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.cachedFrom = sourceNone
	p.cachedAt = time.Time{}
	p.mu.Unlock()
}

// candidates returns the ordered, deduped list of 32-byte candidate keys and the
// source of the PRIMARY (candidates[0]) key. The list is empty when no key is
// configured. Results are cached for cacheTTL.
func (p *KeyProvider) candidates(ctx context.Context) ([][]byte, keySource) {
	p.mu.RLock()
	if p.cached != nil && time.Since(p.cachedAt) < cacheTTL {
		keys, src := p.cached, p.cachedFrom
		p.mu.RUnlock()
		return keys, src
	}
	if !p.cachedAt.IsZero() && time.Since(p.cachedAt) < cacheTTL {
		// Cached an empty result within TTL — honour it (avoids hammering the DB
		// when no key is configured).
		src := p.cachedFrom
		p.mu.RUnlock()
		return nil, src
	}
	p.mu.RUnlock()

	keys, src := p.resolve(ctx)

	p.mu.Lock()
	p.cached = keys
	p.cachedFrom = src
	p.cachedAt = time.Now()
	p.mu.Unlock()

	return keys, src
}

// resolve reads the DB key + env key, in order, deduping.
func (p *KeyProvider) resolve(ctx context.Context) ([][]byte, keySource) {
	var keys [][]byte
	var primary keySource

	// (a) DB key — PRIMARY when present.
	if dbKey := p.dbKey(ctx); dbKey != nil {
		keys = append(keys, dbKey)
		primary = sourceDB
	}

	// (b) ENV key — fallback candidate; PRIMARY only if no DB key.
	if p.envKey != nil {
		if !containsKey(keys, p.envKey) {
			keys = append(keys, p.envKey)
		}
		if primary == sourceNone {
			primary = sourceEnv
		}
	}

	return keys, primary
}

// dbKey loads the platform-level encryption key from ServiceConfig. Returns nil
// if absent or not exactly 32 bytes once base64-decoded. Never logs the value.
func (p *KeyProvider) dbKey(ctx context.Context) []byte {
	if p.client == nil {
		return nil
	}
	row, err := p.client.ServiceConfig.Query().
		Where(
			serviceconfig.ConfigKey(DBKeyConfigKey),
			serviceconfig.TenantIDIsNil(),
		).
		Only(ctx)
	if err != nil || row == nil {
		return nil
	}
	return decodeKey(row.ConfigValue)
}

// PrimaryFingerprint returns the first 8 hex chars of sha256(PRIMARY key), or ""
// if no key is configured. Derived from the key but non-reversible — safe to
// surface in API responses.
func (p *KeyProvider) PrimaryFingerprint(ctx context.Context) string {
	keys, _ := p.candidates(ctx)
	if len(keys) == 0 {
		return ""
	}
	sum := sha256.Sum256(keys[0])
	return hex.EncodeToString(sum[:])[:8]
}

// Source returns "db", "env", or "" for the PRIMARY key.
func (p *KeyProvider) Source(ctx context.Context) string {
	_, src := p.candidates(ctx)
	return string(src)
}

// Configured reports whether any candidate key is available.
func (p *KeyProvider) Configured(ctx context.Context) bool {
	keys, _ := p.candidates(ctx)
	return len(keys) > 0
}

// Encrypt encrypts plaintext with the PRIMARY candidate key (AES-256-GCM).
// If no key is configured it preserves the pre-existing behaviour: Encrypt with
// a nil key, which returns the "encryption key must be 32 bytes" error so callers
// fall back to storing the value unencrypted exactly as before.
func (p *KeyProvider) Encrypt(ctx context.Context, plaintext string) (string, error) {
	keys, _ := p.candidates(ctx)
	if len(keys) == 0 {
		return Encrypt(plaintext, nil)
	}
	return Encrypt(plaintext, keys[0])
}

// Decrypt attempts every candidate key in order and returns the first successful
// AES-256-GCM decryption.
//
// Backward-compatibility: provider secrets stored while no key was configured
// were saved as plaintext. If every candidate key fails AND the stored value
// does not look like valid GCM ciphertext (i.e. it is not base64 decodable to at
// least a GCM nonce), the value is returned as-is so legacy plaintext secrets
// remain readable. If the value *does* look like ciphertext but all keys fail,
// a decryption error is returned (we never silently surface ciphertext).
func (p *KeyProvider) Decrypt(ctx context.Context, encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}

	keys, _ := p.candidates(ctx)
	for _, k := range keys {
		if dec, err := Decrypt(encoded, k); err == nil {
			return dec, nil
		}
	}

	// All keys failed (or none configured). If the value isn't valid ciphertext,
	// treat it as legacy plaintext and return it unchanged.
	if !looksLikeCiphertext(encoded) {
		return encoded, nil
	}

	if len(keys) == 0 {
		return "", errors.New("no encryption key configured to decrypt ciphertext")
	}
	return "", errors.New("decrypt failed for all candidate keys")
}

// GenerateKey returns 32 cryptographically-random bytes, base64-std encoded —
// suitable for storing as the DB key config_value.
func GenerateKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// DecodeKey base64-decodes (std then url) a candidate key and returns it only if
// it is exactly 32 bytes; otherwise nil. Exported for the admin handler to
// validate user-supplied keys.
func DecodeKey(b64 string) []byte { return decodeKey(b64) }

func decodeKey(b64 string) []byte {
	if b64 == "" {
		return nil
	}
	if key, err := base64.StdEncoding.DecodeString(b64); err == nil && len(key) == 32 {
		return key
	}
	if key, err := base64.URLEncoding.DecodeString(b64); err == nil && len(key) == 32 {
		return key
	}
	return nil
}

// containsKey reports whether keys already holds k (constant-time-ish compare;
// equality is fine here since keys are not secret-dependent control flow).
func containsKey(keys [][]byte, k []byte) bool {
	for _, existing := range keys {
		if bytes.Equal(existing, k) {
			return true
		}
	}
	return false
}

// looksLikeCiphertext reports whether encoded plausibly is base64-encoded
// AES-256-GCM output (base64-std decodable to at least the 12-byte GCM nonce).
// Used to distinguish real ciphertext from legacy plaintext on decrypt failure.
func looksLikeCiphertext(encoded string) bool {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return false
	}
	// GCM nonce is 12 bytes; ciphertext also carries a 16-byte tag, so any real
	// encrypted value decodes to >= 12 bytes. Be lenient: require >= 12.
	return len(raw) >= 12
}
