package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"go.uber.org/zap"
)

// BoostOps gives the insights job a boost's own ad-account token and the auto-pause action
// (Sprint 18).
type BoostOps struct {
	db     *ent.Client
	keys   *encryption.KeyProvider
	meta   *meta.Client
	notif  *notifications.Client
	appURL string
	logger *zap.Logger
}

func NewBoostOps(db *ent.Client, keys *encryption.KeyProvider, m *meta.Client, notif *notifications.Client, appURL string, logger *zap.Logger) *BoostOps {
	return &BoostOps{db: db, keys: keys, meta: m, notif: notif, appURL: appURL, logger: logger.Named("boost-ops")}
}

// AdToken decrypts the boost's connected ad-account token.
func (b *BoostOps) AdToken(ctx context.Context, c *ent.Campaign) (string, bool) {
	if c.ConnectedAccountID == nil || b.keys == nil {
		return "", false
	}
	a, err := b.db.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(*c.ConnectedAccountID), connectedaccount.TenantIDEQ(c.TenantID), connectedaccount.StatusEQ("active")).
		Only(ctx)
	if err != nil || a.TokenCiphertext == "" {
		return "", false
	}
	tok, err := b.keys.Decrypt(ctx, a.TokenCiphertext)
	return tok, err == nil && tok != ""
}

// Pause stops the boost on Meta, marks it paused with the reason, and emails the tenant's admins
// once (the auto_paused_at mark is what stops a second pause and a second email).
func (b *BoostOps) Pause(ctx context.Context, c *ent.Campaign, reason string) error {
	if c.MetaCampaignID == nil || *c.MetaCampaignID == "" {
		return fmt.Errorf("no Meta campaign")
	}
	tok, ok := b.AdToken(ctx, c)
	if !ok {
		return fmt.Errorf("the ad account is not connected")
	}
	if err := b.meta.SetStatus(ctx, tok, *c.MetaCampaignID, "PAUSED"); err != nil {
		return err
	}
	patch, _ := json.Marshal(map[string]any{"auto_paused_at": time.Now().UTC().Format(time.RFC3339), "auto_pause_reason": reason})
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + campaign.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(patch))
		bd.WriteString("::jsonb)")
	})
	n, err := b.db.Campaign.Update().
		Where(campaign.IDEQ(c.ID), campaign.StatusEQ("active")).
		SetStatus("paused").
		Modify(func(u *entsql.UpdateBuilder) { u.Set(campaign.FieldMetadata, value) }).
		Save(ctx)
	if err != nil || n == 0 {
		return err
	}
	b.tell(ctx, c, reason)
	return nil
}

func (b *BoostOps) tell(ctx context.Context, c *ent.Campaign, reason string) {
	if b.notif == nil {
		return
	}
	emails, err := tenantAdminEmails(ctx, b.db, c.TenantID, tokenNoticeRoles)
	if err != nil || len(emails) == 0 {
		return
	}
	body := fmt.Sprintf("MarketFlow paused your boost \"%s\" because %s.\n\nYou can change the limit or start it again in MarketFlow: %s",
		c.Name, reason, b.appURL)
	for _, email := range emails {
		if err := b.notif.Send(ctx, c.TenantID.String(), notifications.Message{
			Channel: "email", Recipient: email, Subject: "We paused a boost to protect your budget", Body: body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "boost_auto_paused"},
			IdempotencyKey: "boost-paused-" + c.ID.String() + "-" + email,
		}); err != nil {
			b.logger.Warn("auto-pause notice not sent", zap.String("campaign", c.ID.String()), zap.Error(err))
		}
	}
}
