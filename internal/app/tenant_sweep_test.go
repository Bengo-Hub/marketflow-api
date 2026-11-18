package app

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/customfielddef"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/funnel"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/ent/scheduledmeeting"
	"github.com/bengobox/marketflow-api/internal/ent/segment"
	"github.com/bengobox/marketflow-api/internal/ent/task"
	"github.com/bengobox/marketflow-api/internal/ent/testimonial"
)

// Cross-tenant route sweep (sprint audit item 4a). The real router, every handler wired, is
// walked route by route. Each request is made as a signed-in member of tenant A, and every
// resource id in the path belongs to tenant B. Two things must hold for every route:
//
//  1. no response carries B's data: B's marker text on any status, nor B's tenant id or row
//     ids in a 2xx answer (a refusal may echo the id it refused);
//  2. after the sweep every one of B's rows still exists, unchanged.
//
// New routes are covered without editing this file: a path parameter is filled from the path
// segment before it (leads/{id} is B's lead). A parameter this table does not know fails the
// test with the route named, until a fixture is added below or the route is listed in
// sweepGaps with the reason.
//
// Needs local Postgres (the marketflow database, migrated) and Redis on 127.0.0.1:6379, the same
// as the other DB tests; it skips when either is unreachable or SKIP_POSTGRES_TESTS is set.

const sweepDSN = "postgres://postgres:postgres@localhost:5432/marketflow?sslmode=disable"

// sweepPublicPrefixes are routes no tenant signs in to: they are skipped. Each one is public by
// design or checks its own credential (covered by its own tests).
var sweepPublicPrefixes = []string{
	"/api/v1/public/",     // public profile, funnels, leads and bookings resolve the tenant by slug
	"/api/v1/webhooks/",   // provider webhooks, signature-verified by each handler
	"/api/v1/s2s/",        // tenant export, checks INTERNAL_SERVICE_KEY itself
	"/api/v1/openapi.json", // the API description
	"/v1/docs/",           // Swagger UI
	"/healthz",
	"/metrics",
}

// sweepPlatformPrefixes need a platform owner. This harness runs with JWT off, where the gate is
// not mounted, so they are skipped here; TestPlatformOwnerGate covers the gate itself.
var sweepPlatformPrefixes = []string{"/api/v1/admin/"}

// sweepServicePrefixes need the internal service key. A tenant member without it must be refused.
var sweepServicePrefixes = []string{"/api/v1/internal/"}

// sweepGaps are path segments whose resources are not ent entities, so this sweep has no tenant-B
// fixture for them yet. Each is listed with where its isolation is tested instead.
var sweepGaps = map[string]string{
	"boosts":       "ad boosts are adsmanager's own tables, with no tenant-B fixture here yet",
	"ads":          "an ad inside a boost; same as boosts",
	"inbox":        "social inbox items; isolation in internal/modules/webhooks/inbox_db_test.go",
	"brand-images": "brand images; isolation in internal/modules/brandimages/brandimages_test.go",
	"posts":        "autopilot posts; isolation in internal/modules/custom_agents/autopilot_save_db_test.go",
	"templates":    "agent templates are platform-wide, not tenant data",
}

// sweepStaticParams are path parameters that name a kind of thing, not a tenant's row.
var sweepStaticParams = map[string]string{
	"key":      "ads_specialist", // a team member or setting key; each tenant has its own
	"provider": "meta",           // an integration provider name
	"service":  "sweep",          // an endpoint-discovery service name (admin only)
}

// sweepFixture is one of tenant B's rows: its id and a check that it is still there, unchanged.
type sweepFixture struct {
	id    string
	alive func(ctx context.Context) error
}

func reachable(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func TestTenantRouteSweep(t *testing.T) {
	if os.Getenv("SKIP_POSTGRES_TESTS") != "" {
		t.Skip("SKIP_POSTGRES_TESTS set")
	}
	sqlDB, err := sql.Open("pgx", sweepDSN)
	if err != nil || sqlDB.Ping() != nil {
		t.Skip("local postgres unavailable")
	}
	_ = sqlDB.Close()
	if !reachable("127.0.0.1:6379") {
		t.Skip("local redis unavailable")
	}

	t.Setenv("APP_ENV", "development")
	t.Setenv("POSTGRES_URL", sweepDSN)
	t.Setenv("REDIS_ADDR", "127.0.0.1:6379")
	t.Setenv("REDIS_DB", "15")
	t.Setenv("MF_SECURITY_REQUIRE_JWT", "false")
	t.Setenv("MF_SECURITY_ENFORCE_TENANT_HEADER", "true")
	t.Setenv("MF_EVENTS_ENABLED", "false")
	t.Setenv("INTERNAL_SERVICE_KEY", "sweep-internal-key-for-tests") // secret-scan:allow (test-only value)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, err := New(ctx, cfg, zap.NewNop())
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer scancel()
		_ = a.Shutdown(sctx)
	})
	db := a.entClient

	tenantA, tenantB := uuid.New(), uuid.New()
	marker := "sweepB" + strings.ReplaceAll(tenantB.String()[:8], "-", "")
	for _, id := range []uuid.UUID{tenantA, tenantB} {
		if _, err := db.Tenant.Create().SetID(id).SetName("Sweep " + id.String()).SetSlug("sweep-" + id.String()).Save(ctx); err != nil {
			t.Fatalf("seed tenant: %v", err)
		}
	}
	t.Cleanup(func() { sweepCleanup(context.Background(), db, tenantA, tenantB) })

	fixtures, err := seedTenantB(ctx, db, tenantB, marker)
	if err != nil {
		t.Fatalf("seed tenant B: %v", err)
	}
	bIDs := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		bIDs = append(bIDs, f.id)
	}

	router, ok := a.httpServer.Handler.(chi.Routes)
	if !ok {
		t.Fatalf("the server handler is not a chi router: %T", a.httpServer.Handler)
	}
	claimsA := &authclient.Claims{TenantID: tenantA.String(), Email: "member@a.test", Roles: []string{"admin"}, SubscriptionExempt: true}
	asA := func(method, path string) (int, string) {
		var body io.Reader
		if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
			body = strings.NewReader("{}")
		}
		rctx, rcancel := context.WithTimeout(authclient.ContextWithClaims(ctx, claimsA), 5*time.Second)
		defer rcancel()
		req := httptest.NewRequest(method, path, body).WithContext(rctx)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", tenantA.String())
		rec := httptest.NewRecorder()
		a.httpServer.Handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	var unknown, leaks []string
	swept := 0
	walkErr := chi.Walk(router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == "*" || method == "" {
			method = http.MethodGet
		}
		if method == http.MethodOptions || method == http.MethodHead || hasAnyPrefix(route, sweepPublicPrefixes) ||
			hasAnyPrefix(route, sweepPlatformPrefixes) || route == "/" {
			return nil
		}
		// Service routes only need the refusal checked, so any parameter will do there.
		path, gap, missing := fillRoute(route, fixtures, tenantB, hasAnyPrefix(route, sweepServicePrefixes))
		if gap != "" {
			return nil
		}
		if missing != "" {
			unknown = append(unknown, fmt.Sprintf("%s %s (no fixture for %s)", method, route, missing))
			return nil
		}
		status, body := asA(method, path)
		swept++
		if hasAnyPrefix(route, sweepServicePrefixes) {
			if status != http.StatusUnauthorized && status != http.StatusForbidden {
				leaks = append(leaks, fmt.Sprintf("%s %s: a tenant member without the service key got %d", method, route, status))
			}
			return nil
		}
		// The marker is B's text and never appears in a path, so it counts on any status. B's ids
		// may be echoed by a refusal ("tenant ... does not match"), so they count only in a 2xx.
		if strings.Contains(body, marker) {
			leaks = append(leaks, fmt.Sprintf("%s %s: %d carries tenant B's data", method, route, status))
			return nil
		}
		if status >= 200 && status < 300 {
			for _, id := range append([]string{tenantB.String()}, bIDs...) {
				if strings.Contains(body, id) {
					leaks = append(leaks, fmt.Sprintf("%s %s: %d returns tenant B's row %s", method, route, status, id))
					break
				}
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk: %v", walkErr)
	}
	if swept < 100 {
		t.Fatalf("only %d routes swept; the walk did not reach the tenant routes", swept)
	}
	for name, f := range fixtures {
		if err := f.alive(ctx); err != nil {
			leaks = append(leaks, fmt.Sprintf("tenant B's %s was changed or removed by tenant A: %v", name, err))
		}
	}
	sort.Strings(unknown)
	sort.Strings(leaks)
	if len(unknown) > 0 {
		t.Errorf("routes with a path parameter the sweep cannot fill; add a fixture or a sweepGaps entry:\n  %s", strings.Join(unknown, "\n  "))
	}
	if len(leaks) > 0 {
		t.Errorf("cross-tenant leaks:\n  %s", strings.Join(leaks, "\n  "))
	}
	t.Logf("swept %d routes as tenant A against tenant B's %d rows", swept, len(fixtures))
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) || s == strings.TrimSuffix(p, "/") {
			return true
		}
	}
	return false
}

// fillRoute turns a chi pattern into a request path: each {param} is tenant B's row named by the
// segment before it, a tenant parameter is B's id, and a static parameter its fixed value. gap is
// set for a route on a sweepGaps segment; missing names a parameter nothing fills. lenient fills
// an unknown parameter with a placeholder instead.
func fillRoute(route string, fixtures map[string]sweepFixture, tenantB uuid.UUID, lenient bool) (path, gap, missing string) {
	segs := strings.Split(route, "/")
	for i, s := range segs {
		if s == "*" {
			segs[i] = "x"
			continue
		}
		if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
			if _, ok := sweepGaps[s]; ok && i+1 < len(segs) && strings.HasPrefix(segs[i+1], "{") {
				return "", s, ""
			}
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(s, "{"), "}")
		if j := strings.IndexByte(name, ':'); j >= 0 {
			name = name[:j]
		}
		if strings.EqualFold(name, "tenantID") {
			segs[i] = tenantB.String()
			continue
		}
		if v, ok := sweepStaticParams[name]; ok {
			segs[i] = v
			continue
		}
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		f, ok := fixtures[prev]
		if !ok {
			if lenient {
				segs[i] = "x"
				continue
			}
			return "", "", fmt.Sprintf("{%s} after %q", name, prev)
		}
		segs[i] = f.id
	}
	return strings.Join(segs, "/"), "", ""
}

// seedTenantB creates one of tenant B's rows for every ent resource a route names by id, keyed by
// the path segment that comes before the id. Every row carries marker in a text field.
func seedTenantB(ctx context.Context, db *ent.Client, b uuid.UUID, marker string) (map[string]sweepFixture, error) {
	out := map[string]sweepFixture{}
	now := time.Now().UTC()
	exists := func(n int, err error) error {
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("row missing or its marker changed")
		}
		return nil
	}

	ld, err := db.Lead.Create().SetTenantID(b).SetEmail(marker + "@lead.test").SetFirstName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("lead: %w", err)
	}
	out["leads"] = sweepFixture{ld.ID.String(), func(ctx context.Context) error {
		return exists(db.Lead.Query().Where(lead.ID(ld.ID), lead.TenantID(b), lead.FirstName(marker)).Count(ctx))
	}}

	ct, err := db.Contact.Create().SetTenantID(b).SetEmail(marker + "@contact.test").SetFirstName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("contact: %w", err)
	}
	out["contacts"] = sweepFixture{ct.ID.String(), func(ctx context.Context) error {
		return exists(db.Contact.Query().Where(contact.ID(ct.ID), contact.TenantID(b), contact.FirstName(marker)).Count(ctx))
	}}

	kb, err := db.KnowledgeBaseEntry.Create().SetTenantID(b).SetTitle(marker).SetContent(marker + " content").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("knowledge entry: %w", err)
	}
	out["entries"] = sweepFixture{kb.ID.String(), func(ctx context.Context) error {
		return exists(db.KnowledgeBaseEntry.Query().Where(knowledgebaseentry.ID(kb.ID), knowledgebaseentry.TenantID(b), knowledgebaseentry.Title(marker)).Count(ctx))
	}}

	cp, err := db.Campaign.Create().SetTenantID(b).SetName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("campaign: %w", err)
	}
	out["campaigns"] = sweepFixture{cp.ID.String(), func(ctx context.Context) error {
		return exists(db.Campaign.Query().Where(campaign.ID(cp.ID), campaign.TenantID(b), campaign.Name(marker)).Count(ctx))
	}}

	fn, err := db.Funnel.Create().SetTenantID(b).SetName(marker).SetSlug(marker + "-funnel").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("funnel: %w", err)
	}
	out["funnels"] = sweepFixture{fn.ID.String(), func(ctx context.Context) error {
		return exists(db.Funnel.Query().Where(funnel.ID(fn.ID), funnel.TenantID(b), funnel.Name(marker)).Count(ctx))
	}}

	sq, err := db.NurtureSequence.Create().SetTenantID(b).SetName(marker).SetTriggerEvent("lead.created").SetSteps([]map[string]interface{}{}).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("sequence: %w", err)
	}
	out["sequences"] = sweepFixture{sq.ID.String(), func(ctx context.Context) error {
		return exists(db.NurtureSequence.Query().Where(nurturesequence.ID(sq.ID), nurturesequence.TenantID(b), nurturesequence.Name(marker)).Count(ctx))
	}}

	mt, err := db.ScheduledMeeting.Create().SetTenantID(b).SetScheduledAt(now.Add(48 * time.Hour)).SetNotes(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("meeting: %w", err)
	}
	out["meetings"] = sweepFixture{mt.ID.String(), func(ctx context.Context) error {
		return exists(db.ScheduledMeeting.Query().Where(scheduledmeeting.ID(mt.ID), scheduledmeeting.TenantID(b), scheduledmeeting.Notes(marker)).Count(ctx))
	}}

	ts, err := db.Testimonial.Create().SetTenantID(b).SetAuthorName(marker).SetBody(marker + " says").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("testimonial: %w", err)
	}
	out["testimonials"] = sweepFixture{ts.ID.String(), func(ctx context.Context) error {
		return exists(db.Testimonial.Query().Where(testimonial.ID(ts.ID), testimonial.TenantID(b), testimonial.AuthorName(marker)).Count(ctx))
	}}

	pl, err := db.Pipeline.Create().SetTenantID(b).SetName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("pipeline: %w", err)
	}
	out["pipelines"] = sweepFixture{pl.ID.String(), func(ctx context.Context) error {
		return exists(db.Pipeline.Query().Where(pipeline.ID(pl.ID), pipeline.TenantID(b), pipeline.Name(marker)).Count(ctx))
	}}
	st, err := db.PipelineStage.Create().SetTenantID(b).SetPipelineID(pl.ID).SetName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("stage: %w", err)
	}
	out["stages"] = sweepFixture{st.ID.String(), func(ctx context.Context) error {
		return exists(db.PipelineStage.Query().Where(pipelinestage.ID(st.ID), pipelinestage.TenantID(b), pipelinestage.Name(marker)).Count(ctx))
	}}
	dl, err := db.Deal.Create().SetTenantID(b).SetTitle(marker).SetPipelineID(pl.ID).SetStageID(st.ID).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("deal: %w", err)
	}
	out["deals"] = sweepFixture{dl.ID.String(), func(ctx context.Context) error {
		return exists(db.Deal.Query().Where(deal.ID(dl.ID), deal.TenantID(b), deal.Title(marker), deal.StageID(st.ID)).Count(ctx))
	}}

	ac, err := db.Account.Create().SetTenantID(b).SetName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("account: %w", err)
	}
	out["accounts"] = sweepFixture{ac.ID.String(), func(ctx context.Context) error {
		return exists(db.Account.Query().Where(account.ID(ac.ID), account.TenantID(b), account.Name(marker)).Count(ctx))
	}}

	tk, err := db.Task.Create().SetTenantID(b).SetEntityType("lead").SetEntityID(ld.ID).SetTitle(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("task: %w", err)
	}
	out["tasks"] = sweepFixture{tk.ID.String(), func(ctx context.Context) error {
		return exists(db.Task.Query().Where(task.ID(tk.ID), task.TenantID(b), task.Title(marker)).Count(ctx))
	}}

	cf, err := db.CustomFieldDef.Create().SetTenantID(b).SetEntityType("lead").SetFieldKey(marker).SetFieldLabel(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("custom field: %w", err)
	}
	out["defs"] = sweepFixture{cf.ID.String(), func(ctx context.Context) error {
		return exists(db.CustomFieldDef.Query().Where(customfielddef.ID(cf.ID), customfielddef.TenantID(b), customfielddef.FieldLabel(marker)).Count(ctx))
	}}

	ag, err := db.CustomAgent.Create().SetTenantID(b).SetName(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("custom agent: %w", err)
	}
	out["custom"] = sweepFixture{ag.ID.String(), func(ctx context.Context) error {
		return exists(db.CustomAgent.Query().Where(customagent.ID(ag.ID), customagent.TenantID(b), customagent.Name(marker)).Count(ctx))
	}}

	br, err := db.CreativeBrief.Create().SetTenantID(b).SetTitle(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("brief: %w", err)
	}
	out["briefs"] = sweepFixture{br.ID.String(), func(ctx context.Context) error {
		return exists(db.CreativeBrief.Query().Where(creativebrief.ID(br.ID), creativebrief.TenantID(b), creativebrief.Title(marker)).Count(ctx))
	}}
	cv, err := db.CreativeVersion.Create().SetTenantID(b).SetBriefID(br.ID).SetVersionNo(1).SetFormat("poster").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}
	cvStatus := cv.Status
	out["versions"] = sweepFixture{cv.ID.String(), func(ctx context.Context) error {
		return exists(db.CreativeVersion.Query().Where(creativeversion.ID(cv.ID), creativeversion.TenantID(b), creativeversion.Status(cvStatus)).Count(ctx))
	}}
	as, err := db.CreativeAsset.Create().SetTenantID(b).SetVersionID(cv.ID).SetStorageKey(marker + "/poster.png").SetURL("https://media.test/" + marker + ".png").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("asset: %w", err)
	}
	out["assets"] = sweepFixture{as.ID.String(), func(ctx context.Context) error {
		return exists(db.CreativeAsset.Query().Where(creativeasset.ID(as.ID), creativeasset.TenantID(b), creativeasset.StorageKey(marker+"/poster.png")).Count(ctx))
	}}

	ca, err := db.ConnectedAccount.Create().SetTenantID(b).SetPlatform("facebook").SetAccountType("page").SetExternalID(marker).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("connected account: %w", err)
	}
	pj, err := db.PublishJob.Create().SetTenantID(b).SetAssetID(as.ID).SetConnectedAccountID(ca.ID).SetPlatform("facebook").
		SetScheduledAt(now.Add(72 * time.Hour)).SetDedupeKey(marker + "-job").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("publish job: %w", err)
	}
	pjStatus := pj.Status
	out["jobs"] = sweepFixture{pj.ID.String(), func(ctx context.Context) error {
		return exists(db.PublishJob.Query().Where(publishjob.ID(pj.ID), publishjob.TenantID(b), publishjob.Status(pjStatus), publishjob.ScheduledAt(pj.ScheduledAt)).Count(ctx))
	}}

	sg, err := db.Segment.Create().SetTenantID(b).SetName(marker).SetFilterRules(map[string]interface{}{}).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("segment: %w", err)
	}
	out["segments"] = sweepFixture{sg.ID.String(), func(ctx context.Context) error {
		return exists(db.Segment.Query().Where(segment.ID(sg.ID), segment.TenantID(b), segment.Name(marker)).Count(ctx))
	}}

	// An agent suggestion is an unread activity; dismissing it marks it read.
	sgn, err := db.Activity.Create().SetTenantID(b).SetEntityType("lead").SetEntityID(ld.ID).SetActivityType("suggestion").
		SetSource("agent").SetSubject(marker).SetIsRead(false).Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("suggestion: %w", err)
	}
	out["suggestions"] = sweepFixture{sgn.ID.String(), func(ctx context.Context) error {
		return exists(db.Activity.Query().Where(activity.ID(sgn.ID), activity.TenantID(b), activity.IsRead(false), activity.Subject(marker)).Count(ctx))
	}}

	authUser := uuid.New()
	mu, err := db.MarketflowUser.Create().SetTenantID(b).SetAuthServiceUserID(authUser).SetEmail(marker + "@member.test").Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("member: %w", err)
	}
	// Members are addressed by their SSO user id.
	out["members"] = sweepFixture{authUser.String(), func(ctx context.Context) error {
		return exists(db.MarketflowUser.Query().Where(marketflowuser.ID(mu.ID), marketflowuser.TenantID(b), marketflowuser.Email(marker+"@member.test")).Count(ctx))
	}}
	return out, nil
}

// sweepCleanup removes both tenants' rows from the tables the fixtures and the sweep's own
// writes touch. Best effort: a row it misses only takes space in the local test database.
func sweepCleanup(ctx context.Context, db *ent.Client, ids ...uuid.UUID) {
	for _, id := range ids {
		_, _ = db.PublishJob.Delete().Where(publishjob.TenantID(id)).Exec(ctx)
		_, _ = db.CreativeAsset.Delete().Where(creativeasset.TenantID(id)).Exec(ctx)
		_, _ = db.CreativeVersion.Delete().Where(creativeversion.TenantID(id)).Exec(ctx)
		_, _ = db.CreativeBrief.Delete().Where(creativebrief.TenantID(id)).Exec(ctx)
		_, _ = db.ConnectedAccount.Delete().Where(connectedaccount.TenantID(id)).Exec(ctx)
		_, _ = db.Task.Delete().Where(task.TenantID(id)).Exec(ctx)
		_, _ = db.Activity.Delete().Where(activity.TenantID(id)).Exec(ctx)
		_, _ = db.Deal.Delete().Where(deal.TenantID(id)).Exec(ctx)
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(id)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(id)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(contact.TenantID(id)).Exec(ctx)
		_, _ = db.Lead.Delete().Where(lead.TenantID(id)).Exec(ctx)
		_, _ = db.KnowledgeBaseEntry.Delete().Where(knowledgebaseentry.TenantID(id)).Exec(ctx)
		_, _ = db.Campaign.Delete().Where(campaign.TenantID(id)).Exec(ctx)
		_, _ = db.Funnel.Delete().Where(funnel.TenantID(id)).Exec(ctx)
		_, _ = db.NurtureSequence.Delete().Where(nurturesequence.TenantID(id)).Exec(ctx)
		_, _ = db.ScheduledMeeting.Delete().Where(scheduledmeeting.TenantID(id)).Exec(ctx)
		_, _ = db.Testimonial.Delete().Where(testimonial.TenantID(id)).Exec(ctx)
		_, _ = db.Account.Delete().Where(account.TenantID(id)).Exec(ctx)
		_, _ = db.CustomFieldDef.Delete().Where(customfielddef.TenantID(id)).Exec(ctx)
		_, _ = db.CustomAgent.Delete().Where(customagent.TenantID(id)).Exec(ctx)
		_, _ = db.Segment.Delete().Where(segment.TenantID(id)).Exec(ctx)
		_, _ = db.MarketflowUser.Delete().Where(marketflowuser.TenantID(id)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(id).Exec(ctx)
	}
}
