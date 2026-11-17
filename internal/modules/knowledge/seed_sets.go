package knowledge

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	flpseed "github.com/bengobox/marketflow-api/internal/flp/seed"
)

// The seed sets the seed binary syncs on every start, each from its own file and with its own
// source_kind marker, so each set creates, rewrites and retires only its own rows. The Kenya tax
// and FLP sets were create-only seeds in cmd/seed until 2026-11-17 (owner decision "File wins;
// move all seeds"); they keep the scope and tenant the old seed wrote them in: the run's
// SEED_SCOPE (platform by default, anchored to the platform-owner tenant) and TENANT_ID.

// Markers of the sets other than the Codevertex platform set. They must never change.
const (
	SourceKindKenyaTaxSeed      = "kenya_tax_seed"
	SourceKindFLPProductsSeed   = "flp_products_seed"
	SourceKindFLPFAQSeed        = "flp_faq_seed"
	SourceKindFLPBusinessSeed   = "flp_business_seed"
	SourceKindFLPComplianceSeed = "flp_compliance_seed"
)

// SeedSets returns every seed set in the order the seed binary runs them.
func SeedSets() []SeedSet {
	return []SeedSet{
		CodevertexSeedSet(),
		{Name: "kenya-tax", SourceKind: SourceKindKenyaTaxSeed, Entries: KenyaTaxEntries, Legacy: kenyaTaxLegacy},
		{Name: "flp-products", SourceKind: SourceKindFLPProductsSeed, Entries: FLPProductEntries, Legacy: flpProductsLegacy},
		{Name: "flp-faq", SourceKind: SourceKindFLPFAQSeed, Entries: FLPFAQEntries, Legacy: flpFAQLegacy},
		{Name: "flp-business", SourceKind: SourceKindFLPBusinessSeed, Entries: FLPBusinessEntries, Legacy: flpBusinessLegacy},
		{Name: "flp-compliance", SourceKind: SourceKindFLPComplianceSeed, Entries: FLPComplianceEntries, Legacy: flpComplianceLegacy},
	}
}

// Kenya tax and compliance guide, mirroring finance-service/treasury-api/docs/kenya-tax-reference.md
// (keep the two in sync). It complements the live tax_advisor tool in marketflow-ai, which fetches
// authoritative rates from treasury. One focused entry per subject for sharper retrieval.
//
//go:embed kenya_tax_kb.json
var kenyaTaxKBFile []byte

// KenyaTaxEntries returns the Kenya tax guide shipped in this binary.
func KenyaTaxEntries() ([]PlatformEntry, error) {
	return parseSeedEntries("kenya tax knowledge base", kenyaTaxKBFile, "")
}

// The FLP sets read the FLP catalogue files in internal/flp/seed, which stay their one source.
// Each entry is built from the file exactly as the old seed built it.

// flpFileMeta carries a file's own provenance onto its entries (source is stored as seed_source).
func flpFileMeta(source, lastUpdated string) map[string]any {
	m := map[string]any{}
	if source != "" {
		m[metaSource] = source
	}
	if lastUpdated != "" {
		m["last_updated"] = lastUpdated
	}
	if len(m) == 0 {
		return nil
	}
	return m
}

// FLPProductEntries returns one product entry per product in products.json.
func FLPProductEntries() ([]PlatformEntry, error) {
	data, err := flpseed.SeedFS.ReadFile("products.json")
	if err != nil {
		return nil, fmt.Errorf("read products.json: %w", err)
	}
	return buildFLPProducts(data)
}

func buildFLPProducts(data []byte) ([]PlatformEntry, error) {
	var catalog struct {
		Source      string `json:"source"`
		LastUpdated string `json:"last_updated"`
		Products    []struct {
			ItemCode    string   `json:"item_code"`
			Name        string   `json:"name"`
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
		return nil, fmt.Errorf("parse products.json: %w", err)
	}
	out := make([]PlatformEntry, 0, len(catalog.Products))
	for _, p := range catalog.Products {
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
		code, cc := p.ItemCode, p.CC
		out = append(out, PlatformEntry{
			Category:    "product",
			Title:       p.Name,
			Content:     content,
			Tags:        p.Tags,
			ProductCode: &code,
			ProductCC:   &cc,
			Metadata:    flpFileMeta(catalog.Source, catalog.LastUpdated),
		})
	}
	return validateSeedEntries("FLP products", out, "")
}

// FLPFAQEntries returns one FAQ entry per question in faq.json.
func FLPFAQEntries() ([]PlatformEntry, error) {
	data, err := flpseed.SeedFS.ReadFile("faq.json")
	if err != nil {
		return nil, fmt.Errorf("read faq.json: %w", err)
	}
	return buildFLPFAQ(data)
}

func buildFLPFAQ(data []byte) ([]PlatformEntry, error) {
	var faq struct {
		Source      string `json:"source"`
		LastUpdated string `json:"last_updated"`
		Entries     []struct {
			Question string   `json:"question"`
			Answer   string   `json:"answer"`
			Tags     []string `json:"tags"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(data, &faq); err != nil {
		return nil, fmt.Errorf("parse faq.json: %w", err)
	}
	out := make([]PlatformEntry, 0, len(faq.Entries))
	for _, f := range faq.Entries {
		out = append(out, PlatformEntry{
			Category: "faq",
			Title:    f.Question,
			Content:  f.Answer,
			Tags:     f.Tags,
			Metadata: flpFileMeta(faq.Source, faq.LastUpdated),
		})
	}
	return validateSeedEntries("FLP FAQ", out, "")
}

// FLPBusinessEntries returns the business opportunity entries built from
// business_opportunity.json: company overview, one entry per income stream, the rank ladder and
// how to get started.
func FLPBusinessEntries() ([]PlatformEntry, error) {
	data, err := flpseed.SeedFS.ReadFile("business_opportunity.json")
	if err != nil {
		return nil, fmt.Errorf("read business_opportunity.json: %w", err)
	}
	return buildFLPBusiness(data)
}

func buildFLPBusiness(data []byte) ([]PlatformEntry, error) {
	var biz struct {
		Source      string `json:"source"`
		LastUpdated string `json:"last_updated"`
		Company     struct {
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
		return nil, fmt.Errorf("parse business_opportunity.json: %w", err)
	}
	meta := flpFileMeta(biz.Source, biz.LastUpdated)
	entry := func(title, content string, tags ...string) PlatformEntry {
		return PlatformEntry{Category: "business_opportunity", Title: title, Content: content, Tags: tags, Metadata: meta}
	}

	var out []PlatformEntry
	// The old seed printed the certifications as a Go list ("[a b c]"); they are now comma separated.
	out = append(out, entry("Company Overview",
		fmt.Sprintf("%s was founded in %d and operates in %d+ countries with %d+ years of uninterrupted growth. %s\n\nCertifications: %s",
			biz.Company.Name, biz.Company.Founded, biz.Company.Countries, biz.Company.YearsInBusiness,
			biz.Company.KeyDifferentiator, strings.Join(biz.Company.Certifications, ", ")),
		"company", "overview"))

	for _, is := range biz.IncomeStreams {
		content := is.Description
		if is.Example != "" {
			content += "\n\nExample: " + is.Example
		}
		out = append(out, entry("Income Stream: "+is.Name, content, "income", "business"))
	}

	var rank strings.Builder
	for _, r := range biz.RankLadder {
		req := r.CCRequirement
		if req == "" {
			req = r.Requirement
		}
		income := "N/A"
		if r.MonthlyIncomeIllustration != nil {
			income = *r.MonthlyIncomeIllustration
		}
		fmt.Fprintf(&rank, "- %s: %s (illustration: %s)\n", r.Rank, req, income)
	}
	out = append(out, entry("FBO Rank Ladder", rank.String(), "ranks", "compensation"))

	var start strings.Builder
	for _, s := range biz.HowToGetStarted {
		fmt.Fprintf(&start, "Step %d: %s\n", s.Step, s.Action)
	}
	out = append(out, entry("How to Get Started", start.String(), "getting-started", "join"))

	return validateSeedEntries("FLP business opportunity", out, "")
}

// FLPComplianceEntries returns the required disclaimers and the approved factual claims from
// compliance.json.
func FLPComplianceEntries() ([]PlatformEntry, error) {
	data, err := flpseed.SeedFS.ReadFile("compliance.json")
	if err != nil {
		return nil, fmt.Errorf("read compliance.json: %w", err)
	}
	return buildFLPCompliance(data)
}

func buildFLPCompliance(data []byte) ([]PlatformEntry, error) {
	var comp struct {
		Source               string            `json:"source"`
		LastUpdated          string            `json:"last_updated"`
		Disclaimers          map[string]string `json:"disclaimers"`
		FactualClaimsAllowed []string          `json:"factual_claims_allowed"`
	}
	if err := json.Unmarshal(data, &comp); err != nil {
		return nil, fmt.Errorf("parse compliance.json: %w", err)
	}
	meta := flpFileMeta(comp.Source, comp.LastUpdated)

	// The old seed walked the map in random order; sorted keys keep the words stable between
	// runs, so an unchanged file never rewrites the entry.
	keys := make([]string, 0, len(comp.Disclaimers))
	for k := range comp.Disclaimers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var disclaimers strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&disclaimers, "[%s]: %s\n\n", k, comp.Disclaimers[k])
	}

	var facts strings.Builder
	for _, f := range comp.FactualClaimsAllowed {
		facts.WriteString("- " + f + "\n")
	}

	out := []PlatformEntry{
		{Category: "compliance", Title: "Required Disclaimers", Content: disclaimers.String(),
			Tags: []string{"disclaimers", "compliance"}, Metadata: meta},
		{Category: "compliance", Title: "Approved Factual Claims", Content: facts.String(),
			Tags: []string{"facts", "approved-claims"}, Metadata: meta},
	}
	return validateSeedEntries("FLP compliance", out, "")
}

// Rows the old create-only seeds made, retired once by their set before it writes its own. All
// of their content now lives in the set files above. Kept as titles (and product codes) only.

var kenyaTaxLegacy = []LegacySeedRow{
	{Category: "compliance", Title: "Kenya tax types and rates (2026)"},
	{Category: "compliance", Title: "Kenya VAT, eTIMS and 'No eTIMS, No Expense'"},
	{Category: "compliance", Title: "Kenya withholding tax (WHT) schedule"},
	{Category: "compliance", Title: "Kenya statutory payroll deductions (2026)"},
	{Category: "compliance", Title: "Kenya tax efficiency: deductible expenses and capital allowances"},
	{Category: "compliance", Title: "Kenya: director loans vs capital injection vs salary (income characterisation)"},
	{Category: "compliance", Title: "Kenya tax compliance calendar and penalties"},
}

// flpProductsLegacy matches on the product code, as the old product seed did.
var flpProductsLegacy = func() []LegacySeedRow {
	codes := []string{
		"015", "034", "196", "036", "071", "624", "439", "068", "354", "188",
		"037", "072", "215", "376", "374", "375", "048", "206", "355", "051",
		"026", "064", "028", "475", "456", "528", "547", "553", "609", "238",
		"061", "067", "022", "617", "646", "647", "070", "284", "063", "633",
	}
	out := make([]LegacySeedRow, len(codes))
	for i, c := range codes {
		out[i] = LegacySeedRow{Category: "product", ProductCode: c}
	}
	return out
}()

var flpFAQLegacy = []LegacySeedRow{
	{Category: "faq", Title: "What is Forever Living Products?"},
	{Category: "faq", Title: "What makes Forever Living different from other companies?"},
	{Category: "faq", Title: "What is aloe vera and why is it beneficial?"},
	{Category: "faq", Title: "What is the C9 Clean 9 program?"},
	{Category: "faq", Title: "Are Forever products organic?"},
	{Category: "faq", Title: "What is the Vital5 pack?"},
	{Category: "faq", Title: "How does the Forever business opportunity work?"},
	{Category: "faq", Title: "How much can I earn with Forever?"},
	{Category: "faq", Title: "What are Case Credits (CC)?"},
	{Category: "faq", Title: "What is the Forever2Drive program?"},
	{Category: "faq", Title: "How do I join Forever Living?"},
	{Category: "faq", Title: "What does it cost to start a Forever business?"},
	{Category: "faq", Title: "Is Forever Living a pyramid scheme?"},
	{Category: "faq", Title: "I don't have time for this."},
	{Category: "faq", Title: "The products are too expensive."},
}

var flpBusinessLegacy = []LegacySeedRow{
	{Category: "business_opportunity", Title: "Company Overview"},
	{Category: "business_opportunity", Title: "Income Stream: Retail Business"},
	{Category: "business_opportunity", Title: "Income Stream: Team Building"},
	{Category: "business_opportunity", Title: "Income Stream: Leadership Bonus"},
	{Category: "business_opportunity", Title: "FBO Rank Ladder"},
	{Category: "business_opportunity", Title: "How to Get Started"},
}

var flpComplianceLegacy = []LegacySeedRow{
	{Category: "compliance", Title: "Required Disclaimers"},
	{Category: "compliance", Title: "Approved Factual Claims"},
}
