package rbac

import (
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const syncCacheTTL = 5 * time.Minute

type syncCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

var callerSyncs = &syncCache{seen: map[string]time.Time{}}

// fresh reports whether key was synced within the TTL, and records it when not.
func (c *syncCache) fresh(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if t, ok := c.seen[key]; ok && now.Sub(t) < syncCacheTTL {
		return true
	}
	if len(c.seen) > 10000 { // bounded: drop everything rather than grow without limit
		c.seen = map[string]time.Time{}
	}
	c.seen[key] = now
	return false
}

func (c *syncCache) forget(key string) {
	c.mu.Lock()
	delete(c.seen, key)
	c.mu.Unlock()
}

// SyncMiddleware keeps each signed-in user's local MarketFlow user and role in step with SSO,
// in the tenant of their token (the tenant they act for). Service keys are not users. A change
// of SSO roles is picked up at once (the roles are part of the cache key); otherwise a caller is
// re-synced at most every five minutes. A failed sync never blocks the request: the role gates
// then answer from what is stored.
func (s *Service) SyncMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authclient.ClaimsFromContext(r.Context())
		if ok && c != nil && !c.IsService {
			tid, tErr := uuid.Parse(c.TenantID)
			uid, uErr := c.UserID()
			if tErr == nil && uErr == nil {
				roles := append([]string(nil), c.Roles...)
				sort.Strings(roles)
				key := tid.String() + "|" + uid.String() + "|" + strings.Join(roles, ",")
				if !callerSyncs.fresh(key) {
					if err := s.SyncCaller(r.Context(), tid, uid, c.Email, c.Roles); err != nil {
						callerSyncs.forget(key)
						s.logger.Warn("caller sync failed", zap.String("tenant_id", tid.String()), zap.Error(err))
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
