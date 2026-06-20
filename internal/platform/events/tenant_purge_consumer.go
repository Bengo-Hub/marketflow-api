package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// tenantPurgeSubject is the NATS subject published by subscriptions-api when a
// platform owner confirms an irreversible dormancy purge. The subject follows the
// shared-events convention {aggregate_type}.{event_type} ("tenant" + "purge").
const tenantPurgeSubject = "tenant.purge"

// tenantPurgeQueue is the core-NATS queue group / durable-equivalent for this
// consumer. Mirrors the naming used by the other marketflow consumers so that,
// with >1 replica, exactly one pod processes each purge event.
const tenantPurgeQueue = "marketflow-tenant-purge"

// TenantPurgeConsumer subscribes to tenant.purge and DELETES all of a tenant's
// data from MarketFlow's owned tables. MarketFlow is the customer system-of-record,
// so this intentionally removes that tenant's contacts/leads/campaigns/deals/etc.
//
// This is IRREVERSIBLE and is therefore heavily guarded:
//   - the publisher (subscriptions-api) only emits after a platform owner confirms
//     a dormancy purge on an already-suspended tenant, and
//   - this consumer additionally requires payload.confirmed == true AND
//     payload.reason == "dormancy" before deleting anything.
//
// Deletes run in a single transaction, child-before-parent (FKs are ON DELETE
// NO ACTION), are strictly scoped by tenant_id, and are idempotent (a re-delivered
// event simply deletes zero rows).
type TenantPurgeConsumer struct {
	db     *sql.DB
	logger *zap.Logger
	sub    *nats.Subscription
}

// NewTenantPurgeConsumer creates a new consumer. It uses the raw *sql.DB (rather
// than the ent client) so deletes can be ordered explicitly across FK constraints
// and can cover tables ent does not model (knowledge_base_embeddings) or that lack
// a direct tenant_id column (role_permissions, scoped via the tenant's roles).
func NewTenantPurgeConsumer(db *sql.DB, logger *zap.Logger) *TenantPurgeConsumer {
	return &TenantPurgeConsumer{
		db:     db,
		logger: logger.Named("tenant.purge.consumer"),
	}
}

// Start subscribes to the tenant.purge event.
func (c *TenantPurgeConsumer) Start(conn *nats.Conn) error {
	sub, err := eventslib.QueueSubscribe(c.logger, conn, tenantPurgeSubject, tenantPurgeQueue, c.handle)
	if err != nil {
		return err
	}
	c.sub = sub
	c.logger.Info("subscribed to tenant.purge", zap.String("subject", tenantPurgeSubject))
	return nil
}

// Stop drains the subscription.
func (c *TenantPurgeConsumer) Stop() {
	if c.sub != nil {
		_ = c.sub.Drain()
	}
}

// tenantPurgeEnvelope is the relevant subset of the shared-events Event envelope.
// The publisher places the authoritative tenant id at the top level (tenant_id),
// and the confirmation flags inside payload.
type tenantPurgeEnvelope struct {
	EventType string `json:"event_type"`
	TenantID  string `json:"tenant_id"`
	Payload   struct {
		TenantID  string `json:"tenant_id"`
		Reason    string `json:"reason"`
		Confirmed bool   `json:"confirmed"`
	} `json:"payload"`
}

func (c *TenantPurgeConsumer) handle(msg *nats.Msg) {
	var env tenantPurgeEnvelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		// Malformed envelope is not retryable — Ack (core NATS auto-acks on return).
		c.logger.Warn("tenant.purge: bad envelope — skipping", zap.Error(err))
		return
	}

	// Resolve tenant id: prefer top-level, fall back to payload.
	rawTenantID := env.TenantID
	if rawTenantID == "" {
		rawTenantID = env.Payload.TenantID
	}

	// SAFETY GUARD 1: empty/invalid tenant_id → log + Ack, do nothing.
	tenantID, err := uuid.Parse(rawTenantID)
	if err != nil || tenantID == uuid.Nil {
		c.logger.Warn("tenant.purge: empty/invalid tenant_id — skipping (no data deleted)",
			zap.String("raw_tenant_id", rawTenantID))
		return
	}

	// SAFETY GUARD 2: only act on a platform-owner-confirmed dormancy purge.
	if !env.Payload.Confirmed || env.Payload.Reason != "dormancy" {
		c.logger.Warn("tenant.purge: not a confirmed dormancy purge — skipping (no data deleted)",
			zap.String("tenant_id", tenantID.String()),
			zap.Bool("confirmed", env.Payload.Confirmed),
			zap.String("reason", env.Payload.Reason))
		return
	}

	c.logger.Warn("tenant.purge: CONFIRMED dormancy purge — deleting all MarketFlow data for tenant",
		zap.String("tenant_id", tenantID.String()))

	// Generous timeout: a purge touches many tables but must not hang a worker forever.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	counts, err := c.purgeTenant(ctx, tenantID)
	if err != nil {
		// Transient DB error → Nak so JetStream/core redelivers and we retry. The
		// whole purge is transactional, so a failed attempt leaves no partial state.
		c.logger.Error("tenant.purge: delete failed — requesting redelivery",
			zap.String("tenant_id", tenantID.String()),
			zap.Error(err))
		if nakErr := msg.Nak(); nakErr != nil {
			// Nak only works for JetStream messages; core NATS has no Nak. Log and
			// return so the message is not lost silently — the publisher's outbox
			// retry and operational replay remain the backstop.
			c.logger.Warn("tenant.purge: Nak not supported on this message (core NATS)",
				zap.Error(nakErr))
		}
		return
	}

	total := 0
	fields := make([]zap.Field, 0, len(counts)+2)
	fields = append(fields, zap.String("tenant_id", tenantID.String()))
	for _, tc := range counts {
		total += tc.deleted
		fields = append(fields, zap.Int(tc.table, tc.deleted))
	}
	fields = append(fields, zap.Int("total_rows_deleted", total))
	c.logger.Warn("tenant.purge: COMPLETE — tenant data deleted", fields...)

	// Ack on success (core NATS auto-acks on return; JetStream explicit Ack below).
	_ = msg.Ack()
}

type tableCount struct {
	table   string
	deleted int
}

// purgeOrder lists MarketFlow's tenant-owned tables in FK-safe, child-before-parent
// order. All foreign keys are ON DELETE NO ACTION, so children must be deleted
// before their parents. Tables not listed here either have no tenant_id
// (marketflow_permissions — a global catalog) or are handled specially below
// (role_permissions, knowledge_base_embeddings, provider_settings, tenants).
var purgeOrder = []string{
	// Leaf / dependent rows first.
	"ad_metrics",              // -> campaigns
	"nurture_delivery_events", // -> nurture_enrollments
	"agent_runs",              // -> custom_agents
	"custom_field_values",     // -> custom_field_defs
	"deals",                   // -> pipeline_stages, pipelines, accounts, contacts
	"pipeline_stages",         // -> pipelines
	"scheduled_meetings",      // -> contacts, leads (SET NULL, but tenant-owned)
	"chat_sessions",           // -> contacts, leads (SET NULL, but tenant-owned)
	"nurture_enrollments",     // -> nurture_sequences (via FK? no), tenant-owned
	"activities",
	"tasks",
	"user_role_assignments", // -> marketflow_roles, marketflow_users
	"audit_logs",
	"profile_events",
	"testimonials",
	"short_links",
	"behavioral_triggers",
	"segments",
	"lead_scoring_rules",
	"custom_field_defs",
	"custom_agents",
	"campaigns",            // leads.campaign_id SET NULL handles dangling refs
	"contacts",            // deals/chat/meetings already cleared above
	"leads",
	"funnels",
	"nurture_sequences",
	"knowledge_base_entries", // cascades knowledge_base_embeddings
	"pipelines",
	"accounts",
	"marketflow_users",
	"backups",
	"backup_settings",
	"service_configs",
	"outbox_events",
}

// purgeTenant deletes every tenant-owned row inside a single transaction and
// returns per-table deleted counts. Idempotent: a replay deletes zero rows.
func (c *TenantPurgeConsumer) purgeTenant(ctx context.Context, tenantID uuid.UUID) ([]tableCount, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	// Roll back on any error path; committed below on success.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	counts := make([]tableCount, 0, len(purgeOrder)+4)

	del := func(table, query string, args ...any) error {
		res, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("delete %s: %w", table, err)
		}
		n, _ := res.RowsAffected()
		counts = append(counts, tableCount{table: table, deleted: int(n)})
		return nil
	}

	// 1. role_permissions has no tenant_id; scope by the tenant's roles before the
	//    roles themselves are removed.
	if err := del("role_permissions",
		`DELETE FROM "role_permissions" WHERE "role_id" IN (SELECT "id" FROM "marketflow_roles" WHERE "tenant_id" = $1)`,
		tenantID); err != nil {
		return nil, err
	}

	// 2. knowledge_base_embeddings (ON DELETE CASCADE from kb_entries, but delete
	//    explicitly first so the count is reported and ordering is unambiguous).
	if err := del("knowledge_base_embeddings",
		`DELETE FROM "knowledge_base_embeddings" WHERE "tenant_id" = $1`, tenantID); err != nil {
		return nil, err
	}

	// 3. provider_settings.tenant_id is a varchar holding the tenant UUID string.
	if err := del("provider_settings",
		`DELETE FROM "provider_settings" WHERE "tenant_id" = $1`, tenantID.String()); err != nil {
		return nil, err
	}

	// 4. Bulk ordered deletes for all standard uuid tenant_id tables.
	for _, table := range purgeOrder {
		q := fmt.Sprintf(`DELETE FROM "%s" WHERE "tenant_id" = $1`, table)
		if err := del(table, q, tenantID); err != nil {
			return nil, err
		}
	}

	// 5. marketflow_roles after user_role_assignments + role_permissions are gone.
	if err := del("marketflow_roles",
		`DELETE FROM "marketflow_roles" WHERE "tenant_id" = $1`, tenantID); err != nil {
		return nil, err
	}

	// 6. Finally the tenant registry row itself (keyed by id == tenant_id).
	if err := del("tenants",
		`DELETE FROM "tenants" WHERE "id" = $1`, tenantID); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return counts, nil
}
