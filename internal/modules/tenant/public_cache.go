package tenant

import (
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/ent"
)

// PublicProfileCacheTTL bounds how long a public profile response is served from Redis (Sprint 7
// #58). A profile save drops the entry at once, so the TTL only covers changes made elsewhere: a
// plan change that turns white label on or off, or a tenant sync from auth-api.
const PublicProfileCacheTTL = 2 * time.Minute

// PublicProfileCacheKey is the Redis key of the public profile looked up by name (a slug or a
// Forever distributor ID; the public route accepts both).
func PublicProfileCacheKey(lookup string) string {
	return sharedcache.Key("mf", "public", "profile", strings.TrimSpace(lookup))
}

// PublicProfileCacheKeys are every key one tenant's public profile can be cached under.
func PublicProfileCacheKeys(t *ent.Tenant) []string {
	if t == nil {
		return nil
	}
	keys := []string{PublicProfileCacheKey(t.Slug)}
	if t.FboDistribID != nil && strings.TrimSpace(*t.FboDistribID) != "" {
		keys = append(keys, PublicProfileCacheKey(*t.FboDistribID))
	}
	return keys
}
