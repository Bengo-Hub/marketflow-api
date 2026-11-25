package tenant

import (
	"context"
	"strings"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/tenant/metastore"
	"github.com/bengobox/marketflow-api/internal/platform/shareddefaults"
)

// mirrorBrand keeps the account's logo and colour beside the tenant (shareddefaults.AccountKey), so
// a business without its own inherits them when read. A logo that is not clearly the tenant's own
// is never taken (LogoOwned).
func (s *Syncer) mirrorBrand(ctx context.Context, remote *authTenant) error {
	id, err := uuid.Parse(remote.ID)
	if err != nil {
		return err
	}
	t, err := s.client.Tenant.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil
		}
		return err
	}
	logo := remote.LogoURL
	if !LogoOwned(t, s.platformTenantSlug, logo) {
		logo = ""
	}
	colour, _ := remote.BrandColors["primary"].(string)
	next, changed := shareddefaults.ReadAccount(t.Metadata).Next(logo, colour)
	if !changed {
		return nil
	}
	return metastore.SetKeys(ctx, s.client, id, map[string]any{shareddefaults.AccountKey: next})
}

// LogoOwned reports whether an account logo is clearly this tenant's own: an upload stored under
// the tenant's own id in the media store, or a platform-hosted logo file named after its slug.
// The Codevertex logo belongs to the platform tenant only. Anything else (another domain, a client
// or partner logo, a shared default) is not taken.
func LogoOwned(t *ent.Tenant, platformSlug, logo string) bool {
	return logoWhy(t, logo) != "" && (!isCodevertexLogo(logo) || (platformSlug != "" && t.Slug == platformSlug))
}

// logoWhy says why a logo is the tenant's own, or "" when it cannot tell.
func logoWhy(t *ent.Tenant, logo string) string {
	u := strings.ToLower(strings.TrimSpace(logo))
	if strings.HasPrefix(u, "https://media.codevertexafrica.com/auth/"+t.ID.String()+"/") {
		return "uploaded to this tenant's own account folder"
	}
	const hosted = "https://accounts.codevertexafrica.com/images/logo/"
	if strings.HasPrefix(u, hosted) {
		file := strings.TrimPrefix(u, hosted)
		if i := strings.LastIndexByte(file, '.'); i > 0 {
			file = file[:i]
		}
		if file != "" && file == strings.ToLower(t.Slug) {
			return "platform-hosted logo file named for this tenant's slug"
		}
	}
	return ""
}

func isCodevertexLogo(u string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(u)), "/images/logo/codevertex.png")
}
