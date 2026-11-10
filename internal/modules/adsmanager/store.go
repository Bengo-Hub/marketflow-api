package adsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/campaign"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Campaign statuses a boost moves through (Campaign.status).
const (
	// CampaignPending is a boost saved before its launch credits are charged; the launch worker
	// ignores it. It becomes launching once paid, or is deleted when the charge is refused.
	CampaignPending   = "pending"
	CampaignLaunching = "launching"
	CampaignActive    = "active"
	CampaignFailed    = "failed"
	CampaignPaused    = "paused"
)

// stored is Campaign.metadata.launch: the confirmed plan (no token) and the progress.
type stored struct {
	Plan  storedPlan `json:"plan"`
	State State      `json:"state"`
}

type storedPlan struct {
	AdAccountConnID string    `json:"ad_account_connected_id"`
	PageConnID      string    `json:"page_connected_id"`
	InstagramID     string    `json:"instagram_user_id,omitempty"`
	Currency        string    `json:"currency"`
	Budget          int64     `json:"budget"`
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	Objective       string    `json:"objective"`
	WhatsAppNumber  string    `json:"whatsapp_number,omitempty"`
	Countries       []string  `json:"countries"`
	AgeMin          int       `json:"age_min,omitempty"`
	AgeMax          int       `json:"age_max,omitempty"`
	AssetID         string    `json:"asset_id"`
	ExtraAssetIDs   []string  `json:"extra_asset_ids,omitempty"`
	Message         string    `json:"message"`
	Headline        string    `json:"headline,omitempty"`
	Link            string    `json:"link,omitempty"`
	Welcome         string    `json:"welcome,omitempty"`
	PrivacyURL      string    `json:"privacy_url,omitempty"` // leads: shown on the instant form
	LaunchCharged   bool      `json:"launch_charged,omitempty"`
}

// Worker runs due launches. One pod per period (ClaimPeriod), at most Batch launches a tick.
type Worker struct {
	DB     *ent.Client
	Keys   *encryption.KeyProvider
	Meta   *meta.Client
	Logger *zap.Logger
	Every  time.Duration
	Batch  int
	Now    func() time.Time
}

func NewWorker(db *ent.Client, keys *encryption.KeyProvider, m *meta.Client, logger *zap.Logger) *Worker {
	return &Worker{DB: db, Keys: keys, Meta: m, Logger: logger.Named("ads-launch"), Every: 30 * time.Second, Batch: 20, Now: time.Now}
}

func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.Every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if sharedcache.ClaimPeriod(ctx, "marketflow:ads-launch", w.Every) {
				w.Tick(ctx)
			}
		}
	}
}

// Tick steps every launching campaign whose next_at has come, through the partial
// campaigns_launching index.
func (w *Worker) Tick(ctx context.Context) {
	rows, err := w.DB.Campaign.Query().
		// The literal predicate of the campaigns_launching index (a bind parameter would stop
		// Postgres from using the partial index).
		Where(func(s *entsql.Selector) {
			s.Where(entsql.ExprP("status = 'launching'"))
			s.Where(entsql.ExprP("COALESCE((" + s.C(campaign.FieldMetadata) + "->'launch'->'state'->>'next_at')::timestamptz, 'epoch') <= now()"))
		}).
		Order(ent.Asc(campaign.FieldUpdatedAt)).
		Limit(w.Batch).
		All(ctx)
	if err != nil {
		w.Logger.Error("load launches", zap.Error(err))
		return
	}
	for _, c := range rows {
		if err := w.step(ctx, c); err != nil {
			w.Logger.Warn("launch step", zap.String("campaign", c.ID.String()), zap.Error(err))
		}
	}
}

func (w *Worker) step(ctx context.Context, c *ent.Campaign) error {
	var l stored
	if b, err := json.Marshal(c.Metadata["launch"]); err == nil {
		_ = json.Unmarshal(b, &l)
	}
	plan, err := w.plan(ctx, c, l.Plan)
	if err != nil {
		l.State.Stage, l.State.Error = StageFailed, err.Error()
		return w.save(ctx, c.ID, l, CampaignFailed)
	}
	actID := plan.AdAccountID
	funded := func(ctx context.Context) (bool, error) {
		a, err := w.Meta.AdAccountHealth(ctx, plan.Token, actID)
		if err != nil {
			return false, err
		}
		return BuildQuote(a, plan.Budget, int(plan.End.Sub(plan.Start).Hours()/24)+1, 0).Ready(l.State.PaidOK), nil
	}
	save := func(ctx context.Context, st State) error {
		l.State = st
		status := CampaignLaunching
		switch st.Stage {
		case StageLive:
			status = CampaignActive
		case StageFailed:
			status = CampaignFailed
		}
		return w.save(ctx, c.ID, l, status)
	}
	_, err = Step(ctx, w.Meta, plan, l.State, w.Now().UTC(), funded, save)
	return err
}

// plan rebuilds the launch plan with fresh tokens and the asset's current URL.
func (w *Worker) plan(ctx context.Context, c *ent.Campaign, sp storedPlan) (Plan, error) {
	adTok, actID, err := w.token(ctx, c.TenantID, sp.AdAccountConnID, "meta_ad_account")
	if err != nil {
		return Plan{}, fmt.Errorf("the ad account is no longer connected; reconnect it")
	}
	pageTok, pageID, err := w.token(ctx, c.TenantID, sp.PageConnID, "facebook_page")
	if err != nil {
		return Plan{}, fmt.Errorf("the Facebook Page is no longer connected; reconnect it")
	}
	assetID, _ := uuid.Parse(sp.AssetID)
	asset, err := w.DB.CreativeAsset.Get(ctx, assetID)
	if err != nil || asset.TenantID != c.TenantID || asset.PurgedAt != nil {
		return Plan{}, fmt.Errorf("the video for this boost no longer exists")
	}
	return Plan{Name: c.Name, AdAccountID: actID, Token: adTok, Currency: sp.Currency, Budget: sp.Budget,
		Start: sp.Start, End: sp.End, Objective: sp.Objective, PageID: pageID, InstagramID: sp.InstagramID,
		WhatsAppNumber: sp.WhatsAppNumber, Countries: sp.Countries, AgeMin: sp.AgeMin, AgeMax: sp.AgeMax,
		VideoURL: asset.URL, Message: sp.Message, Headline: sp.Headline, Link: sp.Link, Welcome: sp.Welcome,
		PageToken: pageTok, PrivacyURL: sp.PrivacyURL, Extras: w.extraVariants(ctx, c.TenantID, sp.ExtraAssetIDs)}, nil
}

func (w *Worker) token(ctx context.Context, tid uuid.UUID, connID, accountType string) (token, externalID string, err error) {
	id, err := uuid.Parse(connID)
	if err != nil {
		return "", "", err
	}
	a, err := w.DB.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(id), connectedaccount.TenantIDEQ(tid), connectedaccount.AccountTypeEQ(accountType), connectedaccount.StatusEQ("active")).
		Only(ctx)
	if err != nil {
		return "", "", err
	}
	if w.Keys == nil || a.TokenCiphertext == "" {
		return "", "", errors.New("no token")
	}
	tok, err := w.Keys.Decrypt(ctx, a.TokenCiphertext)
	return tok, a.ExternalID, err
}

// save writes the launch into metadata (jsonb ||, other keys kept) and mirrors the Meta ids into
// the Campaign columns the insights job already reads.
func (w *Worker) save(ctx context.Context, id uuid.UUID, l stored, status string) error {
	return SaveLaunch(ctx, w.DB, id, l, status)
}

// SaveLaunch is shared by the worker and the API (confirm paid, pause).
func SaveLaunch(ctx context.Context, db *ent.Client, id uuid.UUID, l stored, status string) error {
	b, err := json.Marshal(map[string]any{"launch": l})
	if err != nil {
		return err
	}
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + campaign.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(b))
		bd.WriteString("::jsonb)")
	})
	u := db.Campaign.Update().
		// A paused or cancelled boost is never moved back by a worker save.
		Where(campaign.IDEQ(id), campaign.StatusIn(CampaignLaunching, CampaignActive)).
		SetStatus(status).
		Modify(func(ub *entsql.UpdateBuilder) { ub.Set(campaign.FieldMetadata, value) })
	if l.State.CampaignID != "" {
		u.SetMetaCampaignID(l.State.CampaignID)
	}
	if l.State.AdSetID != "" {
		u.SetMetaAdsetID(l.State.AdSetID)
	}
	if l.State.AdID != "" {
		u.SetMetaAdID(l.State.AdID)
	}
	_, err = u.Save(ctx)
	return err
}

// extraVariants loads a boost's extra videos; one removed since the boost was made is skipped,
// so the main ad still runs.
func (w *Worker) extraVariants(ctx context.Context, tid uuid.UUID, ids []string) []Variant {
	var out []Variant
	for _, raw := range ids {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		a, err := w.DB.CreativeAsset.Get(ctx, id)
		if err != nil || a.TenantID != tid || a.PurgedAt != nil {
			continue
		}
		out = append(out, Variant{VideoURL: a.URL})
	}
	return out
}
