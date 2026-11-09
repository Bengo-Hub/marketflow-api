// Package endpoints finds read-only, marketing-safe routes in the other Codevertex services'
// OpenAPI specs so the platform owner can offer them to Vera (Sprint 25, decision 61). A weekly
// scan stores new routes as candidates; nothing reaches an agent until the owner approves it.
// Writes are never offered, and neither are reads of money, auth, users, payroll, HR, audit or
// logs: the sensitive index below decides that before a route is even stored.
package endpoints

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	StatusCandidate = "candidate"
	StatusApproved  = "approved"
	StatusRejected  = "rejected"
)

// Endpoint is one stored route of one service.
type Endpoint struct {
	Method    string    `json:"method"`
	Path      string    `json:"path"` // from the service root, e.g. /api/v1/{tenant}/catalog
	Summary   string    `json:"summary,omitempty"`
	Tags      []string  `json:"tags,omitempty"`
	Status    string    `json:"status"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	DecidedBy string    `json:"decided_by,omitempty"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

// Op is one operation read from a spec.
type Op struct {
	Method, Path, Summary string
	Tags                  []string
}

// sensitive words, matched against tags and path segments. A match blocks the route for good.
var sensitive = wordSet(
	// payments and money
	"pay", "payment", "payhero", "mpesa", "stk", "refund", "invoice", "billing", "bill", "credit", "wallet",
	"balance", "transaction", "ledger", "payout", "fee", "tax", "etims", "kra", "revenue", "account", "gateway",
	// auth and secrets
	"auth", "login", "logout", "oauth", "sso", "token", "session", "jwt", "password", "secret", "key", "apikey",
	"credential", "otp", "rbac", "role", "permission",
	// users and personal data
	"user", "me", "customer", "address", "profile", "contact",
	"guest", "track", "tracking",
	// payroll and HR
	"payroll", "salary", "employee", "staff", "hr", "leave", "attendance", "shift", "rider", "driver", "task",
	// bulk files, signed links and entitlements
	"import", "export", "template", "download", "embed", "entitlement",
	// audit, logs and operations
	"audit", "log", "metric", "healthz", "readyz", "health", "debug",
	// cross-tenant or configuration surfaces
	"admin", "platform", "compliance", "integration", "webhook", "config", "configuration", "setting",
)

// marketing words: a route must carry one (in its tags, or its path when it has no tags).
var marketing = wordSet(
	"catalog", "product", "item", "menu", "category", "promotion", "promo", "offer", "discount", "loyalty",
	"order", "outlet", "store", "shop", "analytic", "kpi", "zone", "coverage", "delivery", "course", "event",
	"plan", "feature", "review", "testimonial", "stock", "inventory", "marketplace", "lead", "campaign",
	"funnel", "collection", "brand", "location", "availability",
)

func wordSet(words ...string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

var splitWords = regexp.MustCompile(`[^a-z0-9]+`)

// words splits text into lower-case words, dropping {path params}.
func words(text string) []string {
	var out []string
	for _, seg := range strings.Split(text, "/") {
		if strings.HasPrefix(seg, "{") {
			continue
		}
		for _, w := range splitWords.Split(strings.ToLower(seg), -1) {
			if w != "" {
				out = append(out, w)
			}
		}
	}
	return out
}

// has reports whether a word, or its singular, is in the set.
func has(set map[string]bool, w string) bool {
	if set[w] || set[strings.TrimSuffix(w, "s")] || set[strings.TrimSuffix(w, "es")] {
		return true
	}
	return strings.HasSuffix(w, "ies") && set[strings.TrimSuffix(w, "ies")+"y"]
}

// Classify decides whether a route may be offered at all. The reason starts with "not a read",
// "sensitive" or "not marketing data" when it may not.
func Classify(method, path string, tags []string) (bool, string) {
	if !strings.EqualFold(method, "GET") {
		return false, "not a read"
	}
	tagWords := words(strings.Join(tags, " "))
	for _, w := range append(words(path), tagWords...) {
		if has(sensitive, w) {
			return false, "sensitive: " + w
		}
	}
	scope := tagWords
	if len(scope) == 0 {
		scope = words(path)
	}
	for _, w := range scope {
		if has(marketing, w) {
			return true, ""
		}
	}
	return false, "not marketing data"
}

// ParseSpec reads a Swagger 2.0 or OpenAPI 3 JSON document. Paths come back from the service
// root: the base path (Swagger basePath, or the path of the first OpenAPI server) is joined unless
// the path already starts with it.
func ParseSpec(data []byte) ([]Op, error) {
	var doc struct {
		BasePath string `json:"basePath"`
		Servers  []struct {
			URL string `json:"url"`
		} `json:"servers"`
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Paths == nil {
		return nil, errors.New("spec has no paths")
	}
	base := doc.BasePath
	if base == "" && len(doc.Servers) > 0 {
		if u, err := url.Parse(doc.Servers[0].URL); err == nil {
			base = u.Path
		}
	}
	base = strings.TrimSuffix(base, "/")
	var ops []Op
	for p, methods := range doc.Paths {
		full := p
		if base != "" && full != base && !strings.HasPrefix(full, base+"/") {
			full = base + p
		}
		for m, raw := range methods {
			switch m {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			var op struct {
				Summary string   `json:"summary"`
				Tags    []string `json:"tags"`
			}
			_ = json.Unmarshal(raw, &op)
			ops = append(ops, Op{Method: strings.ToUpper(m), Path: full, Summary: op.Summary, Tags: op.Tags})
		}
	}
	return ops, nil
}

// Merge folds a fresh scan into the stored list. Known routes keep their status and first-seen
// date; new allowed routes become candidates; routes that left the spec, or that the index now
// blocks, are dropped. It returns the new list (sorted by path, then method) and how many were added.
func Merge(existing []Endpoint, found []Op, now time.Time) ([]Endpoint, int) {
	known := make(map[string]Endpoint, len(existing))
	for _, e := range existing {
		known[e.Method+" "+e.Path] = e
	}
	seen := map[string]bool{}
	var out []Endpoint
	added := 0
	for _, op := range found {
		k := op.Method + " " + op.Path
		if seen[k] {
			continue
		}
		if ok, _ := Classify(op.Method, op.Path, op.Tags); !ok {
			continue
		}
		seen[k] = true
		e, was := known[k]
		if !was {
			e = Endpoint{Method: op.Method, Path: op.Path, Status: StatusCandidate, FirstSeen: now}
			added++
		}
		e.Summary, e.Tags, e.LastSeen = op.Summary, op.Tags, now
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Method < out[j].Method
	})
	return out, added
}

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// FillTenant puts the tenant's slug or UUID into tenant path params ({tenant}, {tenantSlug},
// {orgSlug} take the slug; {tenantID}, {tenant_id} and the like take the UUID) and returns the
// names of the params left for the caller to supply.
func FillTenant(path string, tenantID uuid.UUID, slug string) (string, []string) {
	var rest []string
	filled := pathParam.ReplaceAllStringFunc(path, func(m string) string {
		name := m[1 : len(m)-1]
		switch k := strings.ToLower(strings.ReplaceAll(name, "_", "")); k {
		case "tenant", "tenantslug", "orgslug":
			return url.PathEscape(slug)
		case "tenantid", "tenantuuid", "orgid":
			return tenantID.String()
		}
		rest = append(rest, name)
		return m
	})
	return filled, rest
}

var versionSeg = regexp.MustCompile(`^v[0-9]+$`)

// ToolName turns a path into the resource name Vera asks for: version and tenant segments drop
// out and other params read "by_<name>".
func ToolName(path string) string {
	var parts []string
	for _, seg := range strings.Split(path, "/") {
		switch {
		case seg == "" || seg == "api" || versionSeg.MatchString(seg):
			continue
		case strings.HasPrefix(seg, "{"):
			name := strings.Trim(seg, "{}")
			switch strings.ToLower(strings.ReplaceAll(name, "_", "")) {
			case "tenant", "tenantslug", "orgslug", "tenantid", "tenantuuid", "orgid":
				continue
			}
			parts = append(parts, "by_"+strings.ToLower(name))
		default:
			parts = append(parts, strings.Join(splitWords.Split(strings.ToLower(seg), -1), "_"))
		}
	}
	return strings.Join(parts, "_")
}
