package httpapi_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/bengobox/marketflow-api/internal/modules/rbac"
)

var (
	routeLine  = regexp.MustCompile(`r\.Route\("([^"]+)"`)
	methodLine = regexp.MustCompile(`\.(Get|Post|Put|Patch|Delete)\("([^"]+)", `)
	routeParam = regexp.MustCompile(`\{[^}]+\}`)
)

// tenantRoutes reads the signed-in tenant routes from router.go, up to the internal service
// routes. The source is read because most routes mount only when their handlers are set.
func tenantRoutes(t *testing.T) [][2]string {
	t.Helper()
	src, err := os.ReadFile("router.go")
	if err != nil {
		t.Fatal(err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	from, to := strings.Index(s, `r.Get("/me/access"`), strings.Index(s, "// Internal S2S routes")
	if from < 0 || to < from {
		t.Fatal("router.go changed shape: update this test")
	}
	type open struct {
		prefix string
		indent int
	}
	var stack []open
	var out [][2]string
	for _, line := range strings.Split(s[from:to], "\n") {
		indent := len(line) - len(strings.TrimLeft(line, "\t"))
		if strings.TrimSpace(line) == "})" {
			for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if m := routeLine.FindStringSubmatch(line); m != nil {
			stack = append(stack, open{m[1], indent})
			continue
		}
		if m := methodLine.FindStringSubmatch(line); m != nil {
			p := ""
			for _, o := range stack {
				p += o.prefix
			}
			out = append(out, [2]string{strings.ToUpper(m[1]), strings.TrimRight(p+m[2], "/")})
		}
	}
	return out
}

// Plan item 74: every signed-in tenant route has one permission in the catalogue, and the roles
// that hold it are the roles the write gate lets through.
func TestEveryTenantRouteHasAPermission(t *testing.T) {
	known := map[string]bool{}
	for _, p := range rbac.Permissions() {
		known[p.Code] = true
	}
	routes := tenantRoutes(t)
	if len(routes) < 200 {
		t.Fatalf("only %d routes found: update this test", len(routes))
	}
	// Open to every signed-in caller: their own role, and hiding the setup guide for themselves.
	open := map[string]bool{"/me/access": true, "/tenant/onboarding/dismiss": true}
	for _, rt := range routes {
		method, path := rt[0], routeParam.ReplaceAllString(rt[1], "7f1c2e3a-0b4d-4e5f-8a6b-9c0d1e2f3a4b")
		if open[path] {
			continue
		}
		code := rbac.PermissionFor(method, path)
		if !known[code] {
			t.Errorf("%s %s has no permission (got %q): give its area an entry in rbac modules", method, rt[1], code)
			continue
		}
		for _, role := range []string{rbac.RoleTenantAdmin, rbac.RoleMember, rbac.RoleViewer} {
			// The platform console has its own gate (platformOwnerGate); no tenant role holds its codes.
			gate := rbac.GateAllows(role, method, path) && !strings.HasPrefix(path, "/admin")
			if holds := rbac.RoleGrants(role, code); gate != holds {
				t.Errorf("%s %s: %s passes the gate %v, holds %s %v", method, rt[1], role, gate, code, holds)
			}
		}
	}
}
