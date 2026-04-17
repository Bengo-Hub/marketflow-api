package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowpermission"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/rolepermission"
	"github.com/bengobox/marketflow-api/internal/flp/seed"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	_ = godotenv.Load()

	dbURL := os.Getenv("POSTGRES_URL")
	if dbURL == "" {
		log.Fatal("POSTGRES_URL is required")
	}

	// SEED_SCOPE controls KB visibility:
	//   "platform" (default) — shared across every tenant, consumed by the RAG
	//                          as the base knowledge corpus
	//   "tenant"             — tenant-private overrides / customisations
	scope := os.Getenv("SEED_SCOPE")
	if scope == "" {
		scope = "platform"
	}
	if scope != "platform" && scope != "tenant" {
		log.Fatalf("invalid SEED_SCOPE %q (use 'platform' or 'tenant')", scope)
	}

	// TENANT_ID: for platform scope we still need a real tenant row (FK
	// constraint) but it's purely a storage home — the platform-owner
	// tenant. For tenant scope the user must supply the target tenant UUID.
	tenantID := os.Getenv("TENANT_ID")
	if tenantID == "" && scope == "tenant" {
		log.Fatal("TENANT_ID is required when SEED_SCOPE=tenant")
	}
	var tid uuid.UUID
	if tenantID != "" {
		var err error
		tid, err = uuid.Parse(tenantID)
		if err != nil {
			log.Fatalf("invalid TENANT_ID: %v", err)
		}
	}

	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(4)
	defer db.Close()

	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}

	drv := entsql.OpenDB(dialect.Postgres, db)
	client := ent.NewClient(ent.Driver(drv))
	defer client.Close()

	// Resolve the platform-owner tenant UUID for platform-scope rows (FK).
	if scope == "platform" && tid == uuid.Nil {
		ownerSlug := os.Getenv("PLATFORM_TENANT_SLUG")
		if ownerSlug == "" {
			ownerSlug = "codevertex"
		}
		var ownerID string
		if err := db.QueryRowContext(ctx,
			`SELECT id FROM tenants WHERE slug = $1 LIMIT 1`, ownerSlug,
		).Scan(&ownerID); err != nil {
			log.Fatalf("resolve platform tenant %q: %v (ensure auth-api seed has run and the tenant exists)", ownerSlug, err)
		}
		tid, err = uuid.Parse(ownerID)
		if err != nil {
			log.Fatalf("parse platform tenant id: %v", err)
		}
		log.Printf("Platform scope: anchoring rows to %s tenant (%s)", ownerSlug, tid)
	}

	log.Printf("Seeding scope=%s tenant=%s...", scope, tid)

	// Phase 1: RBAC — permissions, roles, role-permission mappings (Django-style)
	permsSeeded := seedPermissions(ctx, client)
	rolesSeeded := seedRoles(ctx, client, tid)
	mappingsSeeded := seedRolePermissionMappings(ctx, client, tid)
	log.Printf("RBAC: %d permissions, %d roles, %d mappings seeded", permsSeeded, rolesSeeded, mappingsSeeded)

	// Phase 2: Knowledge base seed data
	productsSeeded := seedProducts(ctx, client, tid, scope)
	faqSeeded := seedFAQ(ctx, client, tid, scope)
	bizSeeded := seedBusinessOpportunity(ctx, client, tid, scope)
	complianceSeeded := seedCompliance(ctx, client, tid, scope)

	log.Printf("KB: %d products, %d FAQ, %d business, %d compliance entries",
		productsSeeded, faqSeeded, bizSeeded, complianceSeeded)

	// Phase 3: Default nurture sequence templates (platform scope only).
	// Every tenant auto-enrols leads into these on lead.created; tenants can
	// clone them into tenant scope later when they want customisation.
	sequencesSeeded := seedNurtureSequences(ctx, client, tid, scope)
	log.Printf("Nurture: %d sequence templates seeded", sequencesSeeded)

	// Phase 4: Default campaign templates — starter lead-gen / recruitment /
	// retargeting blueprints tenants can clone and populate with their own
	// ad-account IDs + creative.
	campaignsSeeded := seedCampaignTemplates(ctx, client, tid, scope)
	log.Printf("Campaigns: %d starter templates seeded", campaignsSeeded)
}

func seedProducts(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	data, err := seed.SeedFS.ReadFile("products.json")
	if err != nil {
		log.Printf("Warning: failed to read products.json: %v", err)
		return 0
	}

	var catalog struct {
		Products []struct {
			ItemCode    string   `json:"item_code"`
			Name        string   `json:"name"`
			Category    string   `json:"category"`
			CC          float64  `json:"cc"`
			Description string   `json:"description"`
			Benefits    []string `json:"benefits"`
			Ingredients string   `json:"ingredients"`
			Contents    string   `json:"contents"`
			Directions  string   `json:"directions"`
			Tags        []string `json:"tags"`
		} `json:"products"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		log.Printf("Warning: failed to parse products.json: %v", err)
		return 0
	}

	count := 0
	for _, p := range catalog.Products {
		// Check if already exists
		exists, _ := client.KnowledgeBaseEntry.Query().
			Where(
				knowledgebaseentry.TenantID(tenantID),
				knowledgebaseentry.ProductCodeEQ(p.ItemCode),
			).Exist(ctx)
		if exists {
			continue
		}

		content := p.Description
		if len(p.Benefits) > 0 {
			content += "\n\nBenefits:\n"
			for _, b := range p.Benefits {
				content += "- " + b + "\n"
			}
		}
		if p.Ingredients != "" {
			content += "\nIngredients: " + p.Ingredients
		}
		if p.Contents != "" {
			content += "\nContents: " + p.Contents
		}
		if p.Directions != "" {
			content += "\nDirections: " + p.Directions
		}

		builder := client.KnowledgeBaseEntry.Create().
			SetTenantID(tenantID).
			SetScope(scope).
			SetCategory("product").
			SetTitle(p.Name).
			SetContent(content).
			SetProductCode(p.ItemCode).
			SetProductCc(p.CC)

		if p.Tags != nil {
			builder.SetTags(p.Tags)
		}

		if _, err := builder.Save(ctx); err != nil {
			log.Printf("Warning: failed to seed product %s: %v", p.ItemCode, err)
			continue
		}
		count++
	}
	return count
}

func seedFAQ(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	data, err := seed.SeedFS.ReadFile("faq.json")
	if err != nil {
		log.Printf("Warning: failed to read faq.json: %v", err)
		return 0
	}

	var faqData struct {
		Entries []struct {
			Category string   `json:"category"`
			Question string   `json:"question"`
			Answer   string   `json:"answer"`
			Tags     []string `json:"tags"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &faqData); err != nil {
		log.Printf("Warning: failed to parse faq.json: %v", err)
		return 0
	}

	count := 0
	for _, f := range faqData.Entries {
		exists, _ := client.KnowledgeBaseEntry.Query().
			Where(
				knowledgebaseentry.TenantID(tenantID),
				knowledgebaseentry.TitleEQ(f.Question),
				knowledgebaseentry.CategoryEQ("faq"),
			).Exist(ctx)
		if exists {
			continue
		}

		builder := client.KnowledgeBaseEntry.Create().
			SetTenantID(tenantID).
			SetScope(scope).
			SetCategory("faq").
			SetTitle(f.Question).
			SetContent(f.Answer)

		if f.Tags != nil {
			builder.SetTags(f.Tags)
		}

		if _, err := builder.Save(ctx); err != nil {
			log.Printf("Warning: failed to seed FAQ '%s': %v", f.Question, err)
			continue
		}
		count++
	}
	return count
}

func seedBusinessOpportunity(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	data, err := seed.SeedFS.ReadFile("business_opportunity.json")
	if err != nil {
		log.Printf("Warning: failed to read business_opportunity.json: %v", err)
		return 0
	}

	var biz struct {
		Company struct {
			Name              string   `json:"name"`
			Founded           int      `json:"founded"`
			Countries         int      `json:"countries"`
			YearsInBusiness   int      `json:"years_in_business"`
			KeyDifferentiator string   `json:"key_differentiator"`
			Certifications    []string `json:"certifications"`
		} `json:"company"`
		IncomeStreams []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Example     string `json:"example"`
		} `json:"income_streams"`
		RankLadder []struct {
			Rank                      string  `json:"rank"`
			CCRequirement             string  `json:"cc_requirement"`
			Requirement               string  `json:"requirement"`
			MonthlyIncomeIllustration *string `json:"monthly_income_illustration"`
		} `json:"rank_ladder"`
		HowToGetStarted []struct {
			Step   int    `json:"step"`
			Action string `json:"action"`
		} `json:"how_to_get_started"`
	}
	if err := json.Unmarshal(data, &biz); err != nil {
		log.Printf("Warning: failed to parse business_opportunity.json: %v", err)
		return 0
	}

	count := 0

	// Company overview
	companyContent := fmt.Sprintf("%s was founded in %d and operates in %d+ countries with %d+ years of uninterrupted growth. %s\n\nCertifications: %v",
		biz.Company.Name, biz.Company.Founded, biz.Company.Countries, biz.Company.YearsInBusiness,
		biz.Company.KeyDifferentiator, biz.Company.Certifications)
	if created := createIfNotExists(ctx, client, tenantID, scope, "business_opportunity", "Company Overview", companyContent, []string{"company", "overview"}); created {
		count++
	}

	// Income streams
	for _, is := range biz.IncomeStreams {
		content := is.Description
		if is.Example != "" {
			content += "\n\nExample: " + is.Example
		}
		if created := createIfNotExists(ctx, client, tenantID, scope, "business_opportunity", "Income Stream: "+is.Name, content, []string{"income", "business"}); created {
			count++
		}
	}

	// Rank ladder
	var rankContent string
	for _, r := range biz.RankLadder {
		req := r.CCRequirement
		if req == "" {
			req = r.Requirement
		}
		income := "N/A"
		if r.MonthlyIncomeIllustration != nil {
			income = *r.MonthlyIncomeIllustration
		}
		rankContent += fmt.Sprintf("- %s: %s (illustration: %s)\n", r.Rank, req, income)
	}
	if created := createIfNotExists(ctx, client, tenantID, scope, "business_opportunity", "FBO Rank Ladder", rankContent, []string{"ranks", "compensation"}); created {
		count++
	}

	// How to get started
	var startContent string
	for _, s := range biz.HowToGetStarted {
		startContent += fmt.Sprintf("Step %d: %s\n", s.Step, s.Action)
	}
	if created := createIfNotExists(ctx, client, tenantID, scope, "business_opportunity", "How to Get Started", startContent, []string{"getting-started", "join"}); created {
		count++
	}

	return count
}

func seedCompliance(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	data, err := seed.SeedFS.ReadFile("compliance.json")
	if err != nil {
		log.Printf("Warning: failed to read compliance.json: %v", err)
		return 0
	}

	var comp struct {
		Disclaimers       map[string]string `json:"disclaimers"`
		FactualClaimsAllowed []string       `json:"factual_claims_allowed"`
	}
	if err := json.Unmarshal(data, &comp); err != nil {
		log.Printf("Warning: failed to parse compliance.json: %v", err)
		return 0
	}

	count := 0

	// Disclaimers
	var disclaimerContent string
	for key, val := range comp.Disclaimers {
		disclaimerContent += fmt.Sprintf("[%s]: %s\n\n", key, val)
	}
	if created := createIfNotExists(ctx, client, tenantID, scope, "compliance", "Required Disclaimers", disclaimerContent, []string{"disclaimers", "compliance"}); created {
		count++
	}

	// Factual claims
	var factsContent string
	for _, f := range comp.FactualClaimsAllowed {
		factsContent += "- " + f + "\n"
	}
	if created := createIfNotExists(ctx, client, tenantID, scope, "compliance", "Approved Factual Claims", factsContent, []string{"facts", "approved-claims"}); created {
		count++
	}

	return count
}

func createIfNotExists(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope, category, title, content string, tags []string) bool {
	// Dedup by title+category at the scope level so re-running the seed is safe
	// and platform rows aren't duplicated per tenant.
	exists, _ := client.KnowledgeBaseEntry.Query().
		Where(
			knowledgebaseentry.ScopeEQ(scope),
			knowledgebaseentry.TenantID(tenantID),
			knowledgebaseentry.TitleEQ(title),
			knowledgebaseentry.CategoryEQ(category),
		).Exist(ctx)
	if exists {
		return false
	}

	builder := client.KnowledgeBaseEntry.Create().
		SetTenantID(tenantID).
		SetScope(scope).
		SetCategory(category).
		SetTitle(title).
		SetContent(content).
		SetCreatedAt(time.Now())

	if tags != nil {
		builder.SetTags(tags)
	}

	if _, err := builder.Save(ctx); err != nil {
		log.Printf("Warning: failed to seed '%s': %v", title, err)
		return false
	}
	return true
}

// ════════════════════════════════════════════════════════════════════════════
// RBAC Seeding — Django-style permissions, roles, and role-permission mappings
// ════════════════════════════════════════════════════════════════════════════

// permUUID generates a deterministic UUID from a permission code.
func permUUID(code string) uuid.UUID {
	hash := sha256.Sum256([]byte("marketflow:" + code))
	return uuid.UUID(hash[:16])
}

// roleUUID generates a deterministic UUID from tenant + role code.
func roleUUID(tenantID uuid.UUID, roleCode string) uuid.UUID {
	hash := sha256.Sum256([]byte("marketflow_role:" + tenantID.String() + ":" + roleCode))
	return uuid.UUID(hash[:16])
}

// permDef emits canonical permission codes in the `resource:action` format
// defined in shared-docs/TRINITY-AUTHORIZATION-PATTERN.md and
// shared-docs/sso-integration-guide.md. Auth-service mints these same codes
// into the JWT `permissions` claim so every downstream service (marketflow,
// notifications, treasury, inventory, …) checks the same strings via
// `authclient.RequirePermission("leads:manage")` etc.
//
// Format: `<resource>:<action>` — e.g. `leads:manage`, `campaigns:view_own`.
// Platform-wide resources live in marketflow's permission table for RBAC
// role-composition UIs; the canonical codes flow through unchanged.
type permDef struct {
	code   string // leads:manage
	module string // leads
	action string // manage
	name   string // Manage Leads
	desc   string // Full management of leads
}

// All marketflow modules and actions
var allPermDefs []permDef

func init() {
	modules := []struct {
		name string
		desc string
	}{
		{"leads", "Lead"},
		{"contacts", "Contact"},
		{"knowledge", "Knowledge Base"},
		{"campaigns", "Campaign"},
		{"funnels", "Funnel"},
		{"shortlinks", "Short Link"},
		{"providers", "Provider Config"},
		{"config", "Service Config"},
		{"analytics", "Analytics"},
		{"chat", "AI Chat"},
	}

	actions := []struct {
		code string
		name string
		desc string
	}{
		{"add", "Add", "Create new"},
		{"view", "View", "View all"},
		{"view_own", "View Own", "View own"},
		{"change", "Change", "Edit any"},
		{"change_own", "Change Own", "Edit own"},
		{"delete", "Delete", "Delete"},
		{"manage", "Manage", "Full management"},
	}

	for _, m := range modules {
		for _, a := range actions {
			// Canonical `resource:action` — matches the auth-service JWT
			// `permissions` claim format (shared-docs/TRINITY-AUTHORIZATION-PATTERN.md).
			code := m.name + ":" + a.code
			allPermDefs = append(allPermDefs, permDef{
				code:   code,
				module: m.name,
				action: a.code,
				name:   a.name + " " + m.desc,
				desc:   a.desc + " " + strings.ToLower(m.desc) + " records",
			})
		}
	}
}

// seedPermissions creates all Django-style permissions (global, not tenant-scoped).
func seedPermissions(ctx context.Context, client *ent.Client) int {
	count := 0
	for _, p := range allPermDefs {
		id := permUUID(p.code)
		exists, _ := client.MarketflowPermission.Query().
			Where(marketflowpermission.PermissionCodeEQ(p.code)).
			Exist(ctx)
		if exists {
			continue
		}

		err := client.MarketflowPermission.Create().
			SetID(id).
			SetPermissionCode(p.code).
			SetName(p.name).
			SetModule(p.module).
			SetAction(p.action).
			SetDescription(p.desc).
			Exec(ctx)
		if err != nil {
			log.Printf("  ! permission %s: %v", p.code, err)
			continue
		}
		count++
	}
	if count > 0 {
		log.Printf("  seeded %d permissions (of %d total)", count, len(allPermDefs))
	}
	return count
}

// roleDef defines a system role with its permission scope.
type roleDef struct {
	code string
	name string
	desc string
}

var systemRoles = []roleDef{
	{"marketflow_admin", "MarketFlow Admin", "Full administrative access to all modules"},
	{"fbo_owner", "FBO Owner", "Manage own leads, contacts, knowledge, campaigns, funnels, shortlinks, chat"},
	{"viewer", "Viewer", "Read-only access to all data"},
}

// seedRoles creates system roles for a tenant.
func seedRoles(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
	count := 0
	for _, r := range systemRoles {
		id := roleUUID(tenantID, r.code)
		exists, _ := client.MarketflowRole.Query().
			Where(
				marketflowrole.TenantID(tenantID),
				marketflowrole.RoleCodeEQ(r.code),
			).Exist(ctx)
		if exists {
			continue
		}

		desc := r.desc
		err := client.MarketflowRole.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetRoleCode(r.code).
			SetName(r.name).
			SetDescription(desc).
			SetIsSystemRole(true).
			Exec(ctx)
		if err != nil {
			log.Printf("  ! role %s: %v", r.code, err)
			continue
		}
		count++
	}
	if count > 0 {
		log.Printf("  seeded %d roles for tenant %s", count, tenantID)
	}
	return count
}

// Role → permission mappings
// marketflow_admin: ALL permissions
// fbo_owner: all except providers.manage, config.manage
// viewer: only *.view and *.view_own
var rolePermissionMap = map[string]func(code string) bool{
	"marketflow_admin": func(code string) bool {
		return true // all permissions
	},
	"fbo_owner": func(code string) bool {
		// Exclude provider/config management
		if strings.Contains(code, "providers.manage") || strings.Contains(code, "config.manage") {
			return false
		}
		return true
	},
	"viewer": func(code string) bool {
		// Only view and view_own
		return strings.HasSuffix(code, ".view") || strings.HasSuffix(code, ".view_own")
	},
}

// seedRolePermissionMappings assigns permissions to roles for a tenant.
func seedRolePermissionMappings(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
	count := 0
	for _, r := range systemRoles {
		matcher, ok := rolePermissionMap[r.code]
		if !ok {
			continue
		}

		roleID := roleUUID(tenantID, r.code)

		for _, p := range allPermDefs {
			if !matcher(p.code) {
				continue
			}

			permID := permUUID(p.code)

			// Check if mapping already exists
			exists, _ := client.RolePermission.Query().
				Where(
					rolepermission.RoleID(roleID),
					rolepermission.PermissionID(permID),
				).Exist(ctx)
			if exists {
				continue
			}

			err := client.RolePermission.Create().
				SetRoleID(roleID).
				SetPermissionID(permID).
				Exec(ctx)
			if err != nil {
				continue // likely role/perm not found yet
			}
			count++
		}
	}
	if count > 0 {
		log.Printf("  seeded %d role-permission mappings", count)
	}
	return count
}

// ── Default nurture sequence templates ───────────────────────────────────
// Five canonical FBO flows derived from docs/use-cases/fbo-workflows.md.
// Seeded with scope='platform' so every tenant auto-enrols leads into them
// via worker.LeadCreatedHandler without any per-tenant configuration.
// Idempotent — dedupe on (scope='platform', name).

type seqStep struct {
	DelayHours int    `json:"delay_hours"`
	Channel    string `json:"channel"` // email | whatsapp | sms
	Template   string `json:"template,omitempty"`
	Subject    string `json:"subject,omitempty"`
	Body       string `json:"body,omitempty"`
	Branch     string `json:"branch,omitempty"` // optional next-sequence slug
}

type seqDef struct {
	Name         string
	TriggerEvent string
	LeadTypeFilter string
	Description  string
	Steps        []seqStep
}

func defaultSequences() []seqDef {
	return []seqDef{
		{
			Name:         "Welcome",
			TriggerEvent: "lead.created",
			Description:  "Convert new leads into first-order customers within 10 days.",
			Steps: []seqStep{
				{DelayHours: 0, Channel: "whatsapp",
					Body: "Hi {{first_name}}, I'm {{fbo_name}} — thanks for reaching out about Forever products! Happy to answer any questions."},
				{DelayHours: 24, Channel: "email",
					Subject: "5 aloe vera tips to get you started",
					Body:    "Welcome to the Forever family! Here are five starter tips for getting the most out of our aloe range.\n\nHave questions? <a href=\"{{whatsapp_link}}\">Chat with me on WhatsApp</a>."},
				{DelayHours: 72, Channel: "email",
					Subject: "Meet Clean 9 — our 9-day cleanse programme",
					Body:    "If you'd like to start with a focused wellness reset, C9 is the best-selling intro kit. Want me to send you the details?\n\n<a href=\"{{whatsapp_link}}\">Reply on WhatsApp</a> | <a href=\"{{shop_link}}\">Browse products</a>",
					Branch:  "C9 Offer"},
				{DelayHours: 168, Channel: "whatsapp",
					Body: "Any questions so far, {{first_name}}? I can jump on a 15-min call whenever works for you."},
			},
		},
		{
			Name:         "C9 Offer",
			TriggerEvent: "lead.qualified",
			Description:  "Enrol lead in the 9-day Clean 9 educational drip + final purchase CTA on day 10.",
			Steps: []seqStep{
				{DelayHours: 0, Channel: "email",
					Subject: "What to expect from your 9 days",
					Body:    "C9 is a 9-day programme combining supplements, aloe gel and a meal plan. Here's your day-by-day schedule.\n\nQuestions? <a href=\"{{whatsapp_link}}\">Ask me on WhatsApp</a>."},
				{DelayHours: 48, Channel: "whatsapp",
					Body: "Day-2 check-in — how are you feeling? A lot of people notice better energy around day 3."},
				{DelayHours: 120, Channel: "email",
					Subject: "Your most common C9 questions answered",
					Body:    "Hunger, timings, recipes — the FAQ from 20,000+ Forever customers.\n\nStill have questions? <a href=\"{{whatsapp_link}}\">Chat with me on WhatsApp</a>."},
				{DelayHours: 192, Channel: "whatsapp",
					Body: "Open spots for the next C9 start date. Want me to reserve yours?"},
				{DelayHours: 240, Channel: "email",
					Subject: "Ready to start C9? Here's your link.",
					Body:    "Tap the button below to purchase with my distributor ID already applied.\n\n<a href=\"{{shop_link}}\">Order C9 now</a> | <a href=\"{{whatsapp_link}}\">Need help? WhatsApp me</a>"},
			},
		},
		{
			Name:         "Reorder",
			TriggerEvent: "order.placed",
			LeadTypeFilter: "customer",
			Description:  "Lift reorder rate from ~18% to 35% by nudging consumables at day 25/28/35.",
			Steps: []seqStep{
				{DelayHours: 25 * 24, Channel: "whatsapp",
					Body: "Hey {{first_name}}, your aloe gel bottle should be running low — want me to drop a reorder link?"},
				{DelayHours: 28 * 24, Channel: "email",
					Subject: "Your basket is ready to go",
					Body:    "We've pre-filled a basket with your last order plus one complementary product. Ready when you are.\n\n<a href=\"{{shop_link}}\">Reorder now</a> | <a href=\"{{whatsapp_link}}\">Chat with me</a>"},
				{DelayHours: 35 * 24, Channel: "whatsapp",
					Body: "Final nudge — happy to schedule a 15-min wellness check-in if you'd like."},
			},
		},
		{
			Name:         "Recruitment",
			TriggerEvent: "lead.qualified",
			LeadTypeFilter: "recruit",
			Description:  "Move curious leads toward an Opportunity Call + FBO signup.",
			Steps: []seqStep{
				{DelayHours: 0, Channel: "whatsapp",
					Body: "Hi {{first_name}} — happy you're curious about the business. Here's a short PDF that covers how it works."},
				{DelayHours: 24, Channel: "email",
					Subject: "3 common myths about building an FLP business",
					Body:    "Plus an income disclaimer and the honest numbers. Results vary — I'll share what I actually see from my team.\n\n<a href=\"{{whatsapp_link}}\">Ask me anything on WhatsApp</a> | <a href=\"{{recruit_link}}\">Join my team</a>"},
				{DelayHours: 72, Channel: "whatsapp",
					Body: "Want to join this week's Opportunity Call? 30 min, zero pressure."},
				{DelayHours: 168, Channel: "email",
					Subject: "The rank ladder, explained",
					Body:    "From Assistant Supervisor to Eagle Manager — the steps and the Forever2Drive incentive. Results vary; no income is guaranteed.\n\n<a href=\"{{recruit_link}}\">Start your journey</a> | <a href=\"{{whatsapp_link}}\">Questions? WhatsApp me</a>"},
				{DelayHours: 336, Channel: "whatsapp",
					Body: "Soft follow-up — any questions I can answer?"},
			},
		},
		{
			Name:         "Re-engagement",
			TriggerEvent: "contact.dormant",
			Description:  "Reactivate 5-10% of 60-day dormant leads; clean the rest.",
			Steps: []seqStep{
				{DelayHours: 0, Channel: "email",
					Subject: "We miss you",
					Body:    "Here's what's new at Forever this quarter — products launched since we last spoke.\n\n<a href=\"{{shop_link}}\">See what's new</a> | <a href=\"{{whatsapp_link}}\">Chat with me</a>"},
				{DelayHours: 72, Channel: "whatsapp",
					Body: "Special starter offer — free product with your next purchase."},
				{DelayHours: 240, Channel: "email",
					Subject: "Should we keep sending?",
					Body:    "Update your preferences or unsubscribe in one click. We'll respect whichever you choose."},
			},
		},
	}
}

// ── Default campaign templates ───────────────────────────────────────────
// Starter campaigns per advertising platform. Scope='platform' means every
// tenant sees them in /{orgSlug}/campaigns as clonable blueprints. The
// listing endpoint already merges platform + tenant rows; tenants customise
// by cloning into scope='tenant'.

type campaignTemplate struct {
	Name         string
	Type         string // lead_gen | retargeting | recruitment | re_engagement
	Platform     string // meta | tiktok | google | linkedin | organic
	BudgetDaily  float64
	Currency     string
	AudienceMeta map[string]any
	Description  string
}

func defaultCampaignTemplates() []campaignTemplate {
	return []campaignTemplate{
		{
			Name:        "Meta Lead Ads — Starter",
			Type:        "lead_gen",
			Platform:    "meta",
			BudgetDaily: 10.0,
			Currency:    "USD",
			Description: "Facebook + Instagram lead-ads baseline. Clone, link a Page, attach a funnel, and go live.",
			AudienceMeta: map[string]any{
				"age_min": 25, "age_max": 55,
				"interests": []string{"wellness", "aloe vera", "direct sales"},
				"gender":    "all",
				"locations": []string{"KE", "UG", "TZ"},
			},
		},
		{
			Name:        "Meta Retargeting — Website visitors (30d)",
			Type:        "retargeting",
			Platform:    "meta",
			BudgetDaily: 5.0,
			Currency:    "USD",
			Description: "Re-engage recent website or funnel visitors with the C9 offer.",
			AudienceMeta: map[string]any{
				"custom_audience": "website_visitors_30d",
				"exclude":         "purchased_last_60d",
			},
		},
		{
			Name:        "TikTok — Gen-Z wellness awareness",
			Type:        "lead_gen",
			Platform:    "tiktok",
			BudgetDaily: 15.0,
			Currency:    "USD",
			Description: "TikTok For Business prospecting — short-form video creative, autofill lead form.",
			AudienceMeta: map[string]any{
				"age_min":   18,
				"age_max":   34,
				"interests": []string{"fitness", "beauty", "side hustle"},
			},
		},
		{
			Name:        "Google Ads — Search (brand + category)",
			Type:        "lead_gen",
			Platform:    "google",
			BudgetDaily: 20.0,
			Currency:    "USD",
			Description: "Search campaign capturing intent on 'forever living', 'aloe vera gel', 'C9 cleanse'.",
			AudienceMeta: map[string]any{
				"keywords":       []string{"forever living kenya", "aloe vera gel", "clean 9 cleanse"},
				"match_type":     "phrase",
				"conversion_key": "lead_submit",
			},
		},
		{
			Name:        "Google Ads — Performance Max (retargeting)",
			Type:        "retargeting",
			Platform:    "google",
			BudgetDaily: 10.0,
			Currency:    "USD",
			Description: "PMax with customer-list + website audiences; leverages Google Ads Conversion Upload for closed-loop attribution.",
			AudienceMeta: map[string]any{
				"audience_types": []string{"customer_match", "website_visitors"},
			},
		},
		{
			Name:        "FBO Recruitment — LinkedIn sponsored",
			Type:        "recruitment",
			Platform:    "linkedin",
			BudgetDaily: 15.0,
			Currency:    "USD",
			Description: "Target network-marketing-curious professionals — 1:1 Opportunity Call CTA.",
			AudienceMeta: map[string]any{
				"job_titles": []string{"Sales Representative", "Entrepreneur", "Wellness Coach"},
				"seniority":  []string{"entry", "mid"},
			},
		},
		{
			Name:        "Organic — Re-engagement blast",
			Type:        "re_engagement",
			Platform:    "organic",
			BudgetDaily: 0,
			Currency:    "USD",
			Description: "Non-paid baseline: WhatsApp + email re-engagement for dormant leads (pairs with the daily dormancy cron).",
			AudienceMeta: map[string]any{
				"segment": "dormant_60d",
			},
		},
	}
}

func seedCampaignTemplates(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	if scope != "platform" {
		return 0
	}
	count := 0
	for _, t := range defaultCampaignTemplates() {
		exists, _ := client.Campaign.Query().
			Where(campaign.ScopeEQ("platform"), campaign.NameEQ(t.Name)).
			Exist(ctx)
		if exists {
			continue
		}
		b := client.Campaign.Create().
			SetTenantID(tenantID).
			SetScope("platform").
			SetName(t.Name).
			SetCampaignType(t.Type).
			SetPlatform(t.Platform).
			SetStatus("draft").
			SetCurrency(t.Currency).
			SetMetadata(map[string]any{
				"description":       t.Description,
				"platform_template": true,
			})
		if t.BudgetDaily > 0 {
			b.SetBudgetDaily(t.BudgetDaily)
		}
		if t.AudienceMeta != nil {
			b.SetTargetAudience(t.AudienceMeta)
		}
		if _, err := b.Save(ctx); err != nil {
			log.Printf("  failed to seed campaign template %q: %v", t.Name, err)
			continue
		}
		count++
		log.Printf("  seeded campaign template: %s (%s)", t.Name, t.Platform)
	}
	return count
}

func seedNurtureSequences(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	// Platform templates only — tenant-scoped sequences should be created
	// from the /sequences UI, not the seed.
	if scope != "platform" {
		return 0
	}
	count := 0
	for _, s := range defaultSequences() {
		exists, _ := client.NurtureSequence.Query().
			Where(
				nurturesequence.ScopeEQ("platform"),
				nurturesequence.NameEQ(s.Name),
			).Exist(ctx)
		if exists {
			continue
		}
		steps := make([]map[string]any, 0, len(s.Steps))
		for _, st := range s.Steps {
			steps = append(steps, map[string]any{
				"delay_hours": st.DelayHours,
				"channel":     st.Channel,
				"template":    st.Template,
				"subject":     st.Subject,
				"body":        st.Body,
				"branch":      st.Branch,
			})
		}
		builder := client.NurtureSequence.Create().
			SetTenantID(tenantID).
			SetScope("platform").
			SetName(s.Name).
			SetTriggerEvent(s.TriggerEvent).
			SetSteps(steps).
			SetActive(true).
			SetMetadata(map[string]any{
				"description":       s.Description,
				"platform_template": true,
			})
		if s.LeadTypeFilter != "" {
			builder.SetLeadTypeFilter(s.LeadTypeFilter)
		}
		if _, err := builder.Save(ctx); err != nil {
			log.Printf("  failed to seed sequence %q: %v", s.Name, err)
			continue
		}
		count++
		log.Printf("  seeded sequence: %s (%d steps)", s.Name, len(s.Steps))
	}
	return count
}
