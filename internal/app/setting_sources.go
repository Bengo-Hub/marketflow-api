package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/modules/tenant"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/fxrate"
	"github.com/bengobox/marketflow-api/internal/platform/leadresponse"
	"github.com/bengobox/marketflow-api/internal/platform/subscriptions"
)

var errSourceUnavailable = errors.New("source did not answer")

// settingSources are the settings other services own (plan item 75), read through the same clients
// the rest of the API uses, so what the page shows is what MarketFlow works with.
func settingSources(db *ent.Client, rates *fxrate.Rates, syncer *tenant.Syncer, subs *subscriptions.Client,
	notif *notifications.Client, templateTTL time.Duration) []tenant.SettingSource {
	return []tenant.SettingSource{
		{Key: "profile", Name: "Business details and branding", From: "Codevertex account",
			Updates: "Changes on your account arrive on their own.",
			State: func(ctx context.Context, t *ent.Tenant, fresh bool) (tenant.SourceState, error) {
				if fresh {
					if err := syncer.Refresh(ctx, t.Slug); err != nil {
						return tenant.SourceState{}, err
					}
				}
				p, err := syncer.AuthProfile(ctx, t.Slug)
				if err != nil {
					return tenant.SourceState{}, err
				}
				at, _ := syncer.ProfileReadAt(t.Slug)
				var has []string
				for _, f := range []struct{ label, v string }{{"logo", p.LogoURL}, {"brand colour", p.PrimaryColor},
					{"website", p.Website}, {"address", p.Address}} {
					if f.v != "" {
						has = append(has, f.label)
					}
				}
				detail := p.Name
				if len(has) > 0 {
					detail += ", with " + strings.Join(has, ", ")
				}
				return tenant.SourceState{Detail: detail, LastSync: at}, nil
			}},
		{Key: "currency_rates", Name: "Exchange rate", From: "Books",
			Updates: "Read again a few times a day.",
			State: func(ctx context.Context, t *ent.Tenant, fresh bool) (tenant.SourceState, error) {
				// The tenant's own currency to KES; a KES business uses the US dollar rate for ads.
				cur := tenant.CurrencyOf(ctx, db, t.ID, tenant.PlatformCurrency)
				if cur == "KES" {
					cur = "USD"
				}
				if fresh {
					if err := rates.Refresh(ctx, cur); err != nil {
						return tenant.SourceState{}, err
					}
				}
				v, err := rates.KESPer(ctx, cur)
				if err != nil {
					return tenant.SourceState{}, err
				}
				at, _ := rates.ReadAt(ctx, cur)
				return tenant.SourceState{Detail: fmt.Sprintf("1 %s is KES %.4g", cur, v), LastSync: at}, nil
			}},
		{Key: "plan", Name: "Plan and features", From: "Billing",
			Updates: "Plan changes apply within a minute.",
			State: func(ctx context.Context, t *ent.Tenant, fresh bool) (tenant.SourceState, error) {
				e, at := subs.Snapshot(ctx, t.ID.String(), fresh)
				if e == nil {
					return tenant.SourceState{}, errSourceUnavailable
				}
				return tenant.SourceState{Detail: fmt.Sprintf("%s, %d features", strings.ToLower(e.Status), len(e.Features)), LastSync: at}, nil
			}},
		{Key: "whatsapp_templates", Name: "WhatsApp templates", From: "Notifications",
			Updates: "Approvals are read again on their own.",
			State:   templateState(db, notif, templateTTL)},
	}
}

// templateState reads Meta's review status of the templates the shared lead follow-up uses, on the
// business's own WhatsApp account, and keeps each answer for ttl.
func templateState(db *ent.Client, notif *notifications.Client, ttl time.Duration) func(context.Context, *ent.Tenant, bool) (tenant.SourceState, error) {
	var kept sync.Map // tenant id -> tenant.SourceState
	return func(ctx context.Context, t *ent.Tenant, fresh bool) (tenant.SourceState, error) {
		if v, ok := kept.Load(t.ID); ok && !fresh {
			if st := v.(tenant.SourceState); time.Since(st.LastSync) < ttl {
				return st, nil
			}
		}
		tpl, err := leadresponse.DefaultTemplate(ctx, db)
		if err != nil {
			return tenant.SourceState{}, err
		}
		var names []string
		if tpl != nil {
			for _, s := range tpl.Steps {
				if n, _ := s["wa_template"].(string); n != "" {
					names = append(names, n)
				}
			}
		}
		if len(names) == 0 {
			return tenant.SourceState{Detail: "no template in use"}, nil
		}
		got, err := notif.TemplateStatuses(ctx, t.ID.String(), names)
		if err != nil {
			return tenant.SourceState{}, err
		}
		var parts []string
		for _, n := range names {
			status := strings.ToLower(got[n].Status)
			if status == "" || status == "missing" {
				status = "not on your WhatsApp account"
			}
			parts = append(parts, n+": "+status)
		}
		st := tenant.SourceState{Detail: strings.Join(parts, ", "), LastSync: time.Now()}
		kept.Store(t.ID, st)
		return st, nil
	}
}
