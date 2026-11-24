package leads

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/lead"
)

// MaxImportRows is the most leads one import request may carry.
const MaxImportRows = 500

// importNamespace scopes ImportLeadID's name-based UUIDs.
var importNamespace = uuid.MustParse("b4f0f1d2-6a57-4f0e-9d8c-2f6a1c3e7b51")

// ImportLeadID is the fixed id of the lead imported from one external record in one tenant
// (UUID v5), so a migration that runs again finds its own rows instead of adding new ones.
func ImportLeadID(tenantID uuid.UUID, externalRef string) uuid.UUID {
	return uuid.NewSHA1(importNamespace, []byte(tenantID.String()+"/"+externalRef))
}

// importRefPrefix marks the activity a merged import row became, so a re-run skips it.
const importRefPrefix = "import:"

// ImportRow is one lead held by another service, moved into MarketFlow. ExternalRef names the
// record where it came from ("codevertex-website:lead:12") and must be unique per tenant.
type ImportRow struct {
	ExternalRef string
	Lead        CreateInput
	CreatedAt   time.Time
}

// Import outcomes.
const (
	ImportCreated = "created"
	ImportMerged  = "merged"
	ImportSkipped = "skipped"
)

// ImportResult is what happened to one row: a new lead, a submission on the person's existing
// lead, or nothing because an earlier run already moved it.
type ImportResult struct {
	ExternalRef string    `json:"external_ref"`
	LeadID      uuid.UUID `json:"lead_id"`
	Outcome     string    `json:"outcome"`
}

// Import moves another service's leads into the tenant, one transaction per row. It keeps each
// row's source, status and created time, dedupes against the tenant's leads by email and phone
// like Capture, and writes no lead.created or lead.resubmitted: a migrated lead is history, never
// a new arrival to nurture, alert on or report to an ad network.
func (s *Service) Import(ctx context.Context, tenantID uuid.UUID, rows []ImportRow) ([]ImportResult, error) {
	out := make([]ImportResult, 0, len(rows))
	for _, row := range rows {
		res, err := s.importOne(ctx, tenantID, row)
		if err != nil {
			return out, fmt.Errorf("import %s: %w", row.ExternalRef, err)
		}
		out = append(out, res)
	}
	return out, nil
}

func (s *Service) importOne(ctx context.Context, tenantID uuid.UUID, row ImportRow) (ImportResult, error) {
	ref := strings.TrimSpace(row.ExternalRef)
	if ref == "" {
		return ImportResult{}, fmt.Errorf("external_ref is required")
	}
	id := ImportLeadID(tenantID, ref)
	res := ImportResult{ExternalRef: ref, LeadID: id}
	in := row.Lead
	in.TenantID, in.ID = tenantID, &id
	if !row.CreatedAt.IsZero() {
		at := row.CreatedAt.UTC()
		in.CreatedAt = &at
	}
	meta := map[string]any{}
	for k, v := range in.Metadata {
		meta[k] = v
	}
	meta["imported_from"] = ref
	in.Metadata = meta

	tx, err := s.client.Tx(ctx)
	if err != nil {
		return res, err
	}
	fail := func(err error) (ImportResult, error) {
		_ = tx.Rollback()
		return res, err
	}
	if ok, err := tx.Lead.Query().Where(lead.ID(id), lead.TenantID(tenantID)).Exist(ctx); err != nil {
		return fail(err)
	} else if ok {
		res.Outcome = ImportSkipped
		return res, tx.Rollback()
	}
	if prior, err := tx.Activity.Query().
		Where(activity.TenantID(tenantID), activity.EntityType("lead"), activity.ExternalRef(importRefPrefix+ref)).
		First(ctx); err == nil {
		res.LeadID, res.Outcome = prior.EntityID, ImportSkipped
		return res, tx.Rollback()
	} else if !ent.IsNotFound(err) {
		return fail(err)
	}

	email, phone := matchKeys(in.Email, in.Phone)
	if err := lockKeys(ctx, tx, tenantID, email, phone); err != nil {
		return fail(err)
	}
	existing, err := findExisting(ctx, tx.Lead, tenantID, email, phone)
	if err != nil {
		return fail(err)
	}
	if existing == nil {
		if _, err := s.insert(ctx, tx, in); err != nil {
			return fail(err)
		}
		res.Outcome = ImportCreated
		return res, tx.Commit()
	}

	at := time.Now().UTC()
	if in.CreatedAt != nil {
		at = *in.CreatedAt
	}
	l, err := fillFrom(ctx, tx, existing, in, at, false)
	if err != nil {
		return fail(err)
	}
	if _, err := tx.Activity.Create().
		SetTenantID(tenantID).
		SetEntityType("lead").
		SetEntityID(l.ID).
		SetActivityType(ActivitySubmission).
		SetDirection("inbound").
		SetSubject("Submitted via " + strings.ReplaceAll(in.Source, "_", " ")).
		SetNillableBody(in.Interest).
		SetSource("cross_service").
		SetExternalRef(importRefPrefix + ref).
		SetPerformedAt(at).
		SetCreatedAt(at).
		SetMetadata(submissionMetadata(in)).
		Save(ctx); err != nil {
		return fail(err)
	}
	res.LeadID, res.Outcome = l.ID, ImportMerged
	return res, tx.Commit()
}

// Stats are a tenant's lead counts for the filter: total, this week, and per status and source.
type Stats struct {
	Total    int            `json:"total"`
	ThisWeek int            `json:"this_week"`
	ByStatus map[string]int `json:"by_status"`
	BySource map[string]int `json:"by_source"`
}

// Stats counts the leads matching the list filter, grouped by status and by source.
func (s *Service) Stats(ctx context.Context, in ListInput) (Stats, error) {
	out := Stats{ByStatus: map[string]int{}, BySource: map[string]int{}}
	for _, g := range []struct {
		field string
		into  map[string]int
	}{{lead.FieldStatus, out.ByStatus}, {lead.FieldSource, out.BySource}} {
		var rows []struct {
			Status string `json:"status"`
			Source string `json:"source"`
			Count  int    `json:"count"`
		}
		if err := s.filtered(in).GroupBy(g.field).Aggregate(ent.As(ent.Count(), "count")).Scan(ctx, &rows); err != nil {
			return out, fmt.Errorf("lead stats: %w", err)
		}
		for _, r := range rows {
			g.into[r.Status+r.Source] += r.Count
		}
	}
	for _, n := range out.ByStatus {
		out.Total += n
	}
	week, err := s.filtered(in).Where(lead.CreatedAtGTE(time.Now().AddDate(0, 0, -7))).Count(ctx)
	if err != nil {
		return out, fmt.Errorf("lead stats: %w", err)
	}
	out.ThisWeek = week
	return out, nil
}
