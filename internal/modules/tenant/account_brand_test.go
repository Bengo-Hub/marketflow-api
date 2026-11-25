package tenant

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// Logos are never mixed up: only one stored under the tenant's own account or named for its slug
// is taken, and the Codevertex logo belongs to the platform tenant alone.
func TestLogoOwned(t *testing.T) {
	ten := &ent.Tenant{ID: uuid.MustParse("d78b1af7-bdb2-48d7-849e-64859cf3bb8e"), Slug: "alpha-china-market"}
	other := "https://media.codevertexafrica.com/auth/68cfcb32-cf6d-4ce2-8ba8-3f977f876764/logos/brand/x.png"
	cases := []struct {
		tenant *ent.Tenant
		logo   string
		want   bool
	}{
		{ten, "https://media.codevertexafrica.com/auth/d78b1af7-bdb2-48d7-849e-64859cf3bb8e/logos/brand/a.jpg", true},
		{ten, other, false}, // another tenant's upload
		{ten, "https://alphachinamarket.co.ke/partners/client-logo.png", false},
		{&ent.Tenant{ID: uuid.New(), Slug: "mss"}, "https://accounts.codevertexafrica.com/images/logo/mss.jpeg", true},
		{&ent.Tenant{ID: uuid.New(), Slug: "codevertex-demo"}, "https://accounts.codevertexafrica.com/images/logo/codevertex.png", false},
		{&ent.Tenant{ID: uuid.New(), Slug: "codevertex"}, "https://accounts.codevertexafrica.com/images/logo/codevertex.png", true},
	}
	for _, c := range cases {
		if got := LogoOwned(c.tenant, "codevertex", c.logo); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.tenant.Slug, c.logo, got, c.want)
		}
	}
}
