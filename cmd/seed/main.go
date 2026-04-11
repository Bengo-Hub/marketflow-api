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
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
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

	tenantID := os.Getenv("TENANT_ID")
	if tenantID == "" {
		log.Fatal("TENANT_ID is required (UUID of the target tenant)")
	}
	tid, err := uuid.Parse(tenantID)
	if err != nil {
		log.Fatalf("invalid TENANT_ID: %v", err)
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

	log.Printf("Seeding for tenant %s...", tenantID)

	// Phase 1: RBAC — permissions, roles, role-permission mappings (Django-style)
	permsSeeded := seedPermissions(ctx, client)
	rolesSeeded := seedRoles(ctx, client, tid)
	mappingsSeeded := seedRolePermissionMappings(ctx, client, tid)
	log.Printf("RBAC: %d permissions, %d roles, %d mappings seeded", permsSeeded, rolesSeeded, mappingsSeeded)

	// Phase 2: Knowledge base seed data
	productsSeeded := seedProducts(ctx, client, tid)
	faqSeeded := seedFAQ(ctx, client, tid)
	bizSeeded := seedBusinessOpportunity(ctx, client, tid)
	complianceSeeded := seedCompliance(ctx, client, tid)

	log.Printf("KB: %d products, %d FAQ, %d business, %d compliance entries",
		productsSeeded, faqSeeded, bizSeeded, complianceSeeded)
}

func seedProducts(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
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

func seedFAQ(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
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

func seedBusinessOpportunity(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
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
	if created := createIfNotExists(ctx, client, tenantID, "business_opportunity", "Company Overview", companyContent, []string{"company", "overview"}); created {
		count++
	}

	// Income streams
	for _, is := range biz.IncomeStreams {
		content := is.Description
		if is.Example != "" {
			content += "\n\nExample: " + is.Example
		}
		if created := createIfNotExists(ctx, client, tenantID, "business_opportunity", "Income Stream: "+is.Name, content, []string{"income", "business"}); created {
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
	if created := createIfNotExists(ctx, client, tenantID, "business_opportunity", "FBO Rank Ladder", rankContent, []string{"ranks", "compensation"}); created {
		count++
	}

	// How to get started
	var startContent string
	for _, s := range biz.HowToGetStarted {
		startContent += fmt.Sprintf("Step %d: %s\n", s.Step, s.Action)
	}
	if created := createIfNotExists(ctx, client, tenantID, "business_opportunity", "How to Get Started", startContent, []string{"getting-started", "join"}); created {
		count++
	}

	return count
}

func seedCompliance(ctx context.Context, client *ent.Client, tenantID uuid.UUID) int {
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
	if created := createIfNotExists(ctx, client, tenantID, "compliance", "Required Disclaimers", disclaimerContent, []string{"disclaimers", "compliance"}); created {
		count++
	}

	// Factual claims
	var factsContent string
	for _, f := range comp.FactualClaimsAllowed {
		factsContent += "- " + f + "\n"
	}
	if created := createIfNotExists(ctx, client, tenantID, "compliance", "Approved Factual Claims", factsContent, []string{"facts", "approved-claims"}); created {
		count++
	}

	return count
}

func createIfNotExists(ctx context.Context, client *ent.Client, tenantID uuid.UUID, category, title, content string, tags []string) bool {
	exists, _ := client.KnowledgeBaseEntry.Query().
		Where(
			knowledgebaseentry.TenantID(tenantID),
			knowledgebaseentry.TitleEQ(title),
			knowledgebaseentry.CategoryEQ(category),
		).Exist(ctx)
	if exists {
		return false
	}

	builder := client.KnowledgeBaseEntry.Create().
		SetTenantID(tenantID).
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

// Django-style permission definition: marketflow.{module}.{action}
type permDef struct {
	code   string // marketflow.leads.add
	module string // leads
	action string // add
	name   string // Add Leads
	desc   string // Create new leads
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
			code := "marketflow." + m.name + "." + a.code
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
