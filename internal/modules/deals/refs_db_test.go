package deals

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/account"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/bengobox/marketflow-api/internal/ent/deal"
	"github.com/bengobox/marketflow-api/internal/ent/outboxevent"
	"github.com/bengobox/marketflow-api/internal/ent/pipeline"
	"github.com/bengobox/marketflow-api/internal/ent/pipelinestage"
)

type dealsFixture struct {
	tenant             uuid.UUID
	pipe, pipe2        uuid.UUID
	stage1, stage2, s3 uuid.UUID
	contactID, account uuid.UUID
}

func newDealsFixture(t *testing.T, db *ent.Client) dealsFixture {
	t.Helper()
	ctx := context.Background()
	f := dealsFixture{tenant: uuid.New()}
	db.Tenant.Create().SetID(f.tenant).SetName("D " + f.tenant.String()).SetSlug("d-" + f.tenant.String()).ExecX(ctx)
	f.pipe = db.Pipeline.Create().SetTenantID(f.tenant).SetName("Sales").SaveX(ctx).ID
	f.pipe2 = db.Pipeline.Create().SetTenantID(f.tenant).SetName("Other").SaveX(ctx).ID
	f.stage1 = db.PipelineStage.Create().SetTenantID(f.tenant).SetPipelineID(f.pipe).SetName("New").SetStageOrder(1).SaveX(ctx).ID
	f.stage2 = db.PipelineStage.Create().SetTenantID(f.tenant).SetPipelineID(f.pipe).SetName("Won").SetStageOrder(2).SaveX(ctx).ID
	f.s3 = db.PipelineStage.Create().SetTenantID(f.tenant).SetPipelineID(f.pipe2).SetName("Elsewhere").SetStageOrder(1).SaveX(ctx).ID
	f.contactID = db.Contact.Create().SetTenantID(f.tenant).SetContactType("customer").SaveX(ctx).ID
	f.account = db.Account.Create().SetTenantID(f.tenant).SetName("Acme").SaveX(ctx).ID
	t.Cleanup(func() {
		ids, _ := db.Deal.Query().Where(deal.TenantID(f.tenant)).IDs(ctx)
		if len(ids) > 0 {
			_, _ = db.OutboxEvent.Delete().Where(outboxevent.AggregateIDIn(ids...)).Exec(ctx)
		}
		_, _ = db.Deal.Delete().Where(deal.TenantID(f.tenant)).Exec(ctx)
		_, _ = db.PipelineStage.Delete().Where(pipelinestage.TenantID(f.tenant)).Exec(ctx)
		_, _ = db.Pipeline.Delete().Where(pipeline.TenantID(f.tenant)).Exec(ctx)
		_, _ = db.Contact.Delete().Where(contact.TenantID(f.tenant)).Exec(ctx)
		_, _ = db.Account.Delete().Where(account.TenantID(f.tenant)).Exec(ctx)
		_ = db.Tenant.DeleteOneID(f.tenant).Exec(ctx)
	})
	return f
}

// A deal only points at its own tenant's pipeline, stage, contact and account, and a stage of
// its own pipeline.
func TestDealReferencesStayInTenant(t *testing.T) {
	db := openDealsDB(t)
	ctx := context.Background()
	svc := NewService(db, zap.NewNop())
	a, b := newDealsFixture(t, db), newDealsFixture(t, db)
	p := func(id uuid.UUID) *uuid.UUID { return &id }

	creates := []struct {
		name string
		in   CreateInput
		ok   bool
	}{
		{"own records", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: a.stage1, ContactID: p(a.contactID), AccountID: p(a.account)}, true},
		{"another tenant's pipeline", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: b.pipe, StageID: a.stage1}, false},
		{"another tenant's stage", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: b.stage1}, false},
		{"a stage of another pipeline", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: a.s3}, false},
		{"another tenant's contact", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: a.stage1, ContactID: p(b.contactID)}, false},
		{"another tenant's account", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: a.stage1, AccountID: p(b.account)}, false},
		{"a lead that does not exist", CreateInput{TenantID: a.tenant, Title: "x", PipelineID: a.pipe, StageID: a.stage1, LeadID: p(uuid.New())}, false},
	}
	for _, c := range creates {
		_, err := svc.Create(ctx, c.in)
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if !c.ok && !errors.Is(err, ErrNotInTenant) {
			t.Errorf("%s: want ErrNotInTenant, got %v", c.name, err)
		}
	}

	d, err := svc.Create(ctx, CreateInput{TenantID: a.tenant, Title: "move me", PipelineID: a.pipe, StageID: a.stage1})
	if err != nil {
		t.Fatal(err)
	}
	moves := []struct {
		name  string
		stage uuid.UUID
		ok    bool
	}{
		{"to another tenant's stage", b.stage2, false},
		{"to a stage of another pipeline", a.s3, false},
		{"to a stage of its own pipeline", a.stage2, true},
	}
	for _, m := range moves {
		_, err := svc.Move(ctx, a.tenant, d.ID, m.stage)
		if m.ok != (err == nil) || (!m.ok && !errors.Is(err, ErrNotInTenant)) {
			t.Errorf("move %s: err %v", m.name, err)
		}
	}

	updates := []struct {
		name string
		in   UpdateInput
		ok   bool
	}{
		{"another tenant's contact", UpdateInput{ContactID: p(b.contactID)}, false},
		{"another tenant's account", UpdateInput{AccountID: p(b.account)}, false},
		{"a stage of another pipeline", UpdateInput{StageID: p(a.s3)}, false},
		{"own contact", UpdateInput{ContactID: p(a.contactID)}, true},
	}
	for _, u := range updates {
		_, err := svc.Update(ctx, a.tenant, d.ID, u.in)
		if u.ok != (err == nil) || (!u.ok && !errors.Is(err, ErrNotInTenant)) {
			t.Errorf("update %s: err %v", u.name, err)
		}
	}
}
