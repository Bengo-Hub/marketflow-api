package leadresponse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/nurturesequence"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

const (
	// TemplateFlag in a platform template's metadata marks it as the follow-up every tenant gets a
	// copy of (cmd/seed sets it on "Instant follow-up").
	TemplateFlag = "default_for_tenants"
	// CopyFlag in a tenant sequence's metadata marks the tenant's own copy of that template. Only
	// such a copy obeys the follow-up switch and the "leads after the start only" rule; a sequence
	// the tenant built itself behaves as before.
	CopyFlag = "default_follow_up"
	// ProvisionedKey in tenant metadata records when the copy was made, so a tenant that deletes
	// it is not given a new one.
	ProvisionedKey = "lead_follow_up_provisioned_at"
	// TriggerLeadCreated is the trigger of the follow-up sequences.
	TriggerLeadCreated = "lead.created"
)

// IsDefaultCopy reports whether a sequence is a tenant's copy of the default follow-up.
func IsDefaultCopy(s *ent.NurtureSequence) bool {
	if s == nil {
		return false
	}
	v, _ := s.Metadata[CopyFlag].(bool)
	return v
}

// DefaultTemplate is the active platform lead.created template marked TemplateFlag (the oldest
// when several are), or nil when there is none.
func DefaultTemplate(ctx context.Context, db *ent.Client) (*ent.NurtureSequence, error) {
	rows, err := db.NurtureSequence.Query().
		Where(nurturesequence.ScopeEQ("platform"), nurturesequence.TriggerEventEQ(TriggerLeadCreated), nurturesequence.ActiveEQ(true)).
		Order(ent.Asc(nurturesequence.FieldCreatedAt), ent.Asc(nurturesequence.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	for _, s := range rows {
		if v, _ := s.Metadata[TemplateFlag].(bool); v {
			return s, nil
		}
	}
	return nil, nil
}

// ProvisionFollowUp gives a tenant its own copy of the default follow-up template when the tenant
// has the follow-up switched on, has no lead.created sequence of its own and never had a copy. It
// is idempotent and safe to run from several replicas at once (a per-tenant advisory lock and a
// re-check inside the transaction). It returns the copy when it made one.
func ProvisionFollowUp(ctx context.Context, db *ent.Client, tenantID uuid.UUID) (*ent.NurtureSequence, error) {
	tpl, err := DefaultTemplate(ctx, db)
	if err != nil || tpl == nil {
		return nil, err
	}
	platform := Platform(ctx, db)
	tx, err := db.Tx(ctx)
	if err != nil {
		return nil, err
	}
	done := false
	defer func() {
		if !done {
			_ = tx.Rollback()
		}
	}()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "mf-followup-provision:"+tenantID.String()); err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	t, err := tx.Tenant.Get(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if !Merge(platform, t.Metadata, nil).FollowUp {
		return nil, nil
	}
	if v, ok := t.Metadata[ProvisionedKey]; ok && v != nil {
		return nil, nil
	}
	has, err := tx.NurtureSequence.Query().
		Where(nurturesequence.TenantIDEQ(tenantID), nurturesequence.ScopeEQ("tenant"), nurturesequence.TriggerEventEQ(TriggerLeadCreated)).
		Exist(ctx)
	if err != nil || has {
		return nil, err
	}
	meta := map[string]any{}
	for k, v := range tpl.Metadata {
		meta[k] = v
	}
	delete(meta, TemplateFlag)
	delete(meta, "platform_template")
	meta[CopyFlag] = true
	meta[InheritFlag] = true
	meta["copied_from"] = tpl.ID.String()
	b := tx.NurtureSequence.Create().
		SetTenantID(tenantID).
		SetScope("tenant").
		SetName(tpl.Name).
		SetTriggerEvent(TriggerLeadCreated).
		SetSteps(tpl.Steps).
		SetActive(true).
		SetMetadata(meta)
	if tpl.LeadTypeFilter != nil {
		b.SetNillableLeadTypeFilter(tpl.LeadTypeFilter)
	}
	seq, err := b.Save(ctx)
	if err != nil {
		return nil, fmt.Errorf("copy follow-up: %w", err)
	}
	if err := mergeTenantMeta(ctx, tx.Client(), tenantID, map[string]any{ProvisionedKey: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	done = true
	return seq, nil
}

// mergeTenantMeta merges keys into Tenant.metadata in the database (metadata || patch), never
// rewriting the whole object, so a concurrent settings save is not lost.
func mergeTenantMeta(ctx context.Context, db *ent.Client, tenantID uuid.UUID, patch map[string]any) error {
	raw, _ := json.Marshal(patch)
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + tenant.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(raw))
		b.WriteString("::jsonb)")
	})
	_, err := db.Tenant.Update().Where(tenant.ID(tenantID)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(tenant.FieldMetadata, value) }).
		Save(ctx)
	return err
}

// StartedAt is when lead response went live, false when it has not been recorded yet (then
// nothing is followed up or reminded about).
func StartedAt(ctx context.Context, db *ent.Client) (time.Time, bool) {
	v := svcconfig.Value(ctx, db, uuid.Nil, KeyStartedAt)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// EnsureStartedAt records the start time once (the first worker start or seed run after the
// release) and returns it. Safe from several processes at once.
func EnsureStartedAt(ctx context.Context, db *ent.Client, now time.Time) (time.Time, error) {
	if t, ok := StartedAt(ctx, db); ok {
		return t, nil
	}
	tx, err := db.Tx(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1, 0))", "mf-lead-response-started"); err != nil {
		return time.Time{}, err
	}
	row, err := tx.ServiceConfig.Query().Where(serviceconfig.ConfigKey(KeyStartedAt), serviceconfig.TenantIDIsNil()).First(ctx)
	if err == nil {
		if t, perr := time.Parse(time.RFC3339, row.ConfigValue); perr == nil {
			return t, nil
		}
	} else if !ent.IsNotFound(err) {
		return time.Time{}, err
	}
	at := now.UTC().Truncate(time.Second)
	if row != nil {
		if _, err := tx.ServiceConfig.UpdateOne(row).SetConfigValue(at.Format(time.RFC3339)).Save(ctx); err != nil {
			return time.Time{}, err
		}
	} else if _, err := tx.ServiceConfig.Create().SetConfigKey(KeyStartedAt).SetConfigValue(at.Format(time.RFC3339)).
		SetConfigType("string").
		SetDescription("When lead response went live. Leads created before it are never followed up or reminded about.").
		Save(ctx); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return at, nil
}
