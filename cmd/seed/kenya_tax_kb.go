package main

import (
	"context"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
)

// seedKenyaTaxKB adds platform-scope Kenya tax/compliance knowledge so Vera AI can answer
// tax/legal-advisory questions from the RAG base (complements the live `tax_advisor` tool
// in marketflow-ai, which fetches authoritative rates from treasury). Mirrors
// finance-service/treasury-api/docs/kenya-tax-reference.md — keep the two in sync.
// Topic-chunked: one focused entry per subject for sharper retrieval. Idempotent.
func seedKenyaTaxKB(ctx context.Context, client *ent.Client, tenantID uuid.UUID, scope string) int {
	type entry struct {
		title   string
		content string
		tags    []string
	}
	entries := []entry{
		{
			title: "Kenya tax types and rates (2026)",
			tags:  []string{"kenya", "tax", "rates", "overview"},
			content: "Kenya business taxes (as of 2026): Corporate Income Tax (CIT) 30%, filed annually (return by 30 June, balance 30 April). " +
				"PAYE on employees: 10% (0–24,000), 25% (24,001–32,333), 30% (32,334–500,000), 32.5% (500,001–800,000), 35% (>800,000) KES/month; personal relief KES 2,400/month; remit by 9th. " +
				"VAT 16% standard (8% petroleum, 0% zero-rated, exempt), monthly by 20th; registration threshold KES 5,000,000 turnover/12 months. " +
				"Turnover Tax (TOT) 3% of gross for turnover KES 1M–25M (not VAT-registered), now paid DAILY (from March 2026). " +
				"SEP Tax 3% on gross from Kenyan users by non-residents (replaced Digital Service Tax). Capital Gains Tax 15%. " +
				"This is product guidance, not formal tax advice — verify current figures with KRA before filing.",
		},
		{
			title: "Kenya VAT, eTIMS and 'No eTIMS, No Expense'",
			tags:  []string{"kenya", "vat", "etims", "compliance"},
			content: "VAT in Kenya is 16% standard, 8% petroleum, 0% zero-rated (exports/specified essentials), or exempt (financial services, education, healthcare, unprocessed agriculture — no input recovery). " +
				"Input VAT must be claimed within 6 months with valid eTIMS invoices. eTIMS tax bands transmitted to KRA: A=16%, B=0%, C=Exempt, D=8%/other. " +
				"Since 1 January 2026 the 'No eTIMS, No Expense' rule means an expense WITHOUT an eTIMS-compliant invoice is NOT tax-deductible, and KRA cross-validates declared income/expenses against eTIMS and customs records. " +
				"Always capture the supplier's eTIMS invoice (and the buyer KRA PIN for B2B) so the cost is deductible and input VAT is claimable.",
		},
		{
			title: "Kenya withholding tax (WHT) schedule",
			tags:  []string{"kenya", "withholding", "wht"},
			content: "Resident WHT rates (remit by 20th via PRN): professional/management/consultancy fees 5%; contractual fees (building/civil works) 3%; dividends 5%; interest 15% (qualifying housing bonds 10%); royalties 5%; " +
				"commercial rent via agent 10%; residential rent (MRI) 7.5%; digital content 5%; sales promotion/advertising 5%; supply of goods to public entities ~3%; betting/gaming winnings 20% (final). " +
				"Non-resident rates are generally higher and treaty-dependent. Penalty for non-deduction/late remittance: 10% of the tax plus interest.",
		},
		{
			title: "Kenya statutory payroll deductions (2026)",
			tags:  []string{"kenya", "payroll", "nssf", "shif", "housing-levy"},
			content: "Statutory payroll deductions remitted by the 9th: " +
				"NSSF Tier I 6% employee + 6% employer up to LEL KES 9,000 (max 540 each); Tier II 6% + 6% on pay between 9,000 and UEL KES 108,000 (max 5,940 each) — new LEL/UEL effective February 2026. " +
				"SHIF (Social Health Insurance Fund, replaced NHIF) 2.75% of gross, minimum KES 300/month, no cap. " +
				"Affordable Housing Levy 1.5% employee + 1.5% employer, no cap. These (SHIF, AHL, NSSF, pension up to 30,000/month) are allowable before PAYE. Payroll taxes are owned by the ERP/payroll system.",
		},
		{
			title: "Kenya tax efficiency: deductible expenses and capital allowances",
			tags:  []string{"kenya", "deductions", "capital-allowances", "tax-efficiency"},
			content: "Reduce tax legally by claiming everything deductible (wholly & exclusively for business): rent, utilities, business insurance, professional/audit/legal fees, advertising, repairs, staff training, bank charges, " +
				"freight/shipping (import freight is part of landed cost/COGS), inter-branch stock-transfer logistics, and last-mile delivery — all require eTIMS-compliant invoices from 1 Jan 2026. " +
				"Kenya disallows accounting depreciation — claim capital allowances instead: Class 1 (heavy machinery, lorries >3t) 37.5%; Class 3 (cars, vans, motorcycles) 25%; computers & software 10%; industrial buildings 2.5%; commercial buildings 10%; furniture 10%; " +
				"investment deduction 100% (manufacturing machinery/buildings) or 150% (>KES 200M outside Nairobi/Mombasa/Kisumu, cumulative ≥ KES 1B). " +
				"A company vehicle registered under the business earns capital allowances plus deductible running costs (fuel, service, insurance) — keep mileage and maintenance logs as KRA cross-checks import/insurance/finance records. " +
				"Interest on non-resident loans is capped at 30% of EBITDA (resident-lender interest is uncapped).",
		},
		{
			title: "Kenya: director loans vs capital injection vs salary (income characterisation)",
			tags:  []string{"kenya", "director-loan", "shareholder", "structuring"},
			content: "Money brought into a company is only taxed if it is income — proper documentation keeps genuine non-income out of tax. " +
				"Director's loan (director lends the company, or company lends director): with a written loan agreement (principal, arm's-length interest, repayment schedule, board resolution, bank-traceable transfers) the repayment is NOT income and not taxed. " +
				"Without documentation KRA may treat advances as a deemed dividend (5% WHT) or salary (PAYE). Red flags: no agreement, interest-free, irregular/no repayment, frequent advances then write-offs. " +
				"Shareholder capital injection: record as equity (share capital/contributed surplus) with a board resolution, share certificate and bank evidence — NOT income, not taxed, and not a company deduction. " +
				"External/third-party loans: keep the loan agreement and drawdown evidence so the inflow is not treated as taxable receipts. Always use the correct legal channel and paperwork.",
		},
		{
			title: "Kenya tax compliance calendar and penalties",
			tags:  []string{"kenya", "calendar", "deadlines", "penalties"},
			content: "Recurring obligations: by the 9th — PAYE (P10), NSSF, SHIF, Affordable Housing Levy. By the 20th — VAT (VAT3), WHT, MRI residential rent. Turnover Tax is paid daily. " +
				"Quarterly instalment tax: 20 April, 20 June, 20 September, 20 December. Annual: CIT return by 30 June (balance 30 April); BRS annual return via eCitizen. " +
				"A Tax Compliance Certificate (free via iTax) needs all returns filed and no arrears, and is required for tenders, loans and some licences. " +
				"Late-filing penalties: CIT KES 20,000 or 5%; VAT KES 10,000 or 5%; PAYE KES 10,000 or 25%; WHT 10% of tax; plus interest on unpaid amounts. File and remit on time to avoid penalties.",
		},
	}

	count := 0
	for _, e := range entries {
		if createIfNotExists(ctx, client, tenantID, scope, "compliance", e.title, e.content, e.tags) {
			count++
		}
	}
	return count
}
