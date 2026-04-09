package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/knowledgebaseentry"
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

	log.Printf("Seeding knowledge base for tenant %s...", tenantID)

	productsSeeded := seedProducts(ctx, client, tid)
	faqSeeded := seedFAQ(ctx, client, tid)
	bizSeeded := seedBusinessOpportunity(ctx, client, tid)
	complianceSeeded := seedCompliance(ctx, client, tid)

	log.Printf("Seeding complete: %d products, %d FAQ, %d business, %d compliance entries",
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
