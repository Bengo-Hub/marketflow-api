package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/httpware"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/creativeversion"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

const (
	// renderTimeout sits above marketflow-ai's own 110s poster budget.
	renderTimeout = 130 * time.Second
	// staleAfter re-queues a version whose renderer died mid-call.
	staleAfter = 5 * time.Minute
	// maxAttempts bounds re-queues; the charge key render:<version_id> keeps a
	// retry from charging twice.
	maxAttempts  = 3
	renderBatch  = 6
	renderWorker = 3
)

// Renderer makes queued poster versions by calling marketflow-ai. Every worker
// replica runs one; a conditional status update claims each version for exactly
// one of them.
type Renderer struct {
	db         *ent.Client
	aiBaseURL  string
	serviceKey string
	hc         *http.Client
	logger     *zap.Logger
	nowFn      func() time.Time
	interval   time.Duration
}

// NewRenderer builds a Renderer.
func NewRenderer(db *ent.Client, aiBaseURL, serviceKey string, logger *zap.Logger) *Renderer {
	return &Renderer{db: db, aiBaseURL: strings.TrimSuffix(aiBaseURL, "/"), serviceKey: serviceKey,
		hc: &http.Client{Timeout: renderTimeout}, logger: logger.Named("creative.render"),
		nowFn: time.Now, interval: 10 * time.Second}
}

// Run polls the render queue until ctx ends.
func (r *Renderer) Run(ctx context.Context) {
	if r.aiBaseURL == "" {
		r.logger.Warn("creative renderer off: no marketflow-ai URL")
		return
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Tick recovers stale renders, checks clips with no news, then renders up to
// one batch (a video version only starts its clip job).
func (r *Renderer) Tick(ctx context.Context) {
	r.recoverStale(ctx)
	r.pollClips(ctx)
	queued, err := r.db.CreativeVersion.Query().
		Where(creativeversion.StatusEQ(StatusQueued)).
		Order(ent.Asc(creativeversion.FieldCreatedAt)).
		Limit(renderBatch).
		All(ctx)
	if err != nil {
		r.logger.Warn("render queue read failed", zap.Error(err))
		return
	}
	sem := make(chan struct{}, renderWorker)
	done := make(chan struct{}, len(queued))
	for _, v := range queued {
		sem <- struct{}{}
		go func(v *ent.CreativeVersion) {
			defer func() { <-sem; done <- struct{}{} }()
			if !r.claim(ctx, v.ID) {
				return
			}
			if v.Kind == KindVideo {
				r.startClip(ctx, v)
			} else {
				r.render(ctx, v)
			}
		}(v)
	}
	for range queued {
		<-done
	}
}

// claim moves a version from queued to rendering; false when another replica won.
func (r *Renderer) claim(ctx context.Context, id uuid.UUID) bool {
	n, err := r.db.CreativeVersion.Update().
		Where(creativeversion.ID(id), creativeversion.StatusEQ(StatusQueued)).
		SetStatus(StatusRendering).
		Save(ctx)
	return err == nil && n == 1
}

// recoverStale re-queues poster renders left behind by a replica that stopped,
// and fails those that already used their attempts. Clips run as jobs in
// marketflow-ai and are settled by pollClips instead.
func (r *Renderer) recoverStale(ctx context.Context) {
	stale, err := r.db.CreativeVersion.Query().
		Where(creativeversion.StatusEQ(StatusRendering), creativeversion.KindEQ(KindPoster),
			creativeversion.UpdatedAtLT(r.nowFn().Add(-staleAfter))).
		Limit(50).
		All(ctx)
	if err != nil {
		return
	}
	for _, v := range stale {
		attempts := intOf(v.Metadata["attempts"])
		u := r.db.CreativeVersion.Update().
			Where(creativeversion.ID(v.ID), creativeversion.StatusEQ(StatusRendering), creativeversion.UpdatedAt(v.UpdatedAt))
		if attempts >= maxAttempts {
			u.SetStatus(StatusFailed).SetError("the poster took too long; try again")
		} else {
			u.SetStatus(StatusQueued)
		}
		_, _ = u.Save(ctx)
	}
}

// posterCall is marketflow-ai's POST /api/v1/internal/creative/poster body.
type posterCall struct {
	TenantID  string   `json:"tenant_id"`
	VersionID string   `json:"version_id"`
	Prompt    string   `json:"prompt"`
	PhotoURLs []string `json:"photo_urls,omitempty"`
	Format    string   `json:"format"`
	Tier      string   `json:"tier"`
	Palette   []string `json:"palette,omitempty"`
	Accent    string   `json:"accent,omitempty"`
	LogoURL   string   `json:"logo_url,omitempty"`
	Headline  string   `json:"headline,omitempty"`
	Subline   string   `json:"subline,omitempty"`
	Price     string   `json:"price,omitempty"`
	CTA       string   `json:"cta,omitempty"`
	Charge    bool     `json:"charge"`
	// SceneURL makes it a text-only revision over that stored scene (free).
	SceneURL string `json:"scene_url,omitempty"`
}

// posterReply is its 200 answer.
type posterReply struct {
	Poster struct {
		URL       string  `json:"url"`
		Key       string  `json:"key"`
		Width     int     `json:"width"`
		Height    int     `json:"height"`
		Bytes     int     `json:"bytes"`
		SHA256    string  `json:"sha256"`
		Route     string  `json:"route"`
		CostUSD   float64 `json:"cost_usd"`
		SourceURL string  `json:"source_url"`
		SceneKey  string  `json:"scene_key"`
		SceneURL  string  `json:"scene_url"`
		Attempts  int     `json:"attempts"`
	} `json:"poster"`
	CreditsCharged int `json:"credits_charged"`
}

// errNoCredits is a 402 from marketflow-ai: not retried.
var errNoCredits = errors.New("not enough AI credits for this poster")

// errSceneGone is a 410 on a text-only revision: the scene was purged.
var errSceneGone = errors.New("the original image is no longer stored; make a new poster")

func (r *Renderer) render(ctx context.Context, v *ent.CreativeVersion) {
	meta := copyMap(v.Metadata)
	meta["attempts"] = intOf(meta["attempts"]) + 1
	_, _ = r.db.CreativeVersion.UpdateOneID(v.ID).SetMetadata(meta).Save(ctx)

	call := posterCall{TenantID: v.TenantID.String(), VersionID: v.ID.String(), Format: v.Format, Tier: v.Tier,
		Prompt: str(v.Copy["prompt"]), Headline: str(v.Copy["headline"]), Subline: str(v.Copy["subline"]),
		Price: str(v.Copy["price"]), CTA: str(v.Copy["cta"]), PhotoURLs: strs(meta["photo_urls"]), Charge: true}
	if recompose, _ := meta["recompose"].(bool); recompose {
		call.SceneURL, call.Charge = str(meta["scene_url"]), false
	}
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		brand, _ := b.Metadata["brand"].(map[string]any)
		call.Accent, call.LogoURL, call.Palette = str(brand["accent"]), str(brand["logo_url"]), strs(brand["palette"])
		if len(call.PhotoURLs) == 0 {
			call.PhotoURLs = strs(b.Offering["photo_urls"])
		}
	}

	reply, err := r.call(ctx, call)
	if err != nil {
		r.fail(ctx, v.ID, meta, err)
		return
	}
	p := reply.Poster
	meta["route"], meta["cost_usd"], meta["provider_attempts"] = p.Route, p.CostUSD, p.Attempts
	if p.SourceURL != "" {
		meta["scene_url"] = p.SourceURL
	}
	expires := r.nowFn().Add(DraftTTL)
	err = withTx(ctx, r.db, func(tx *ent.Tx) error {
		if _, err := tx.CreativeAsset.Create().
			SetTenantID(v.TenantID).SetVersionID(v.ID).SetKind(AssetImage).
			SetStorageKey(p.Key).SetURL(p.URL).SetBytes(int64(p.Bytes)).
			SetWidth(p.Width).SetHeight(p.Height).SetSha256(p.SHA256).
			SetExpiresAt(expires).
			Save(ctx); err != nil {
			return err
		}
		// The clean scene lives and is purged with the draft, like the poster.
		if p.SceneKey != "" && p.SceneURL != "" {
			if _, err := tx.CreativeAsset.Create().
				SetTenantID(v.TenantID).SetVersionID(v.ID).SetKind(AssetScene).
				SetStorageKey(p.SceneKey).SetURL(p.SceneURL).SetExpiresAt(expires).
				Save(ctx); err != nil {
				return err
			}
		}
		return tx.CreativeVersion.UpdateOneID(v.ID).
			SetStatus(StatusReady).SetCredits(reply.CreditsCharged).SetMetadata(meta).ClearError().
			Exec(ctx)
	})
	if err != nil {
		r.logger.Error("poster made but not recorded", zap.String("version_id", v.ID.String()), zap.Error(err))
		return // stays rendering; recoverStale re-queues it and the charge key stops a double charge
	}
	r.logger.Info("poster ready", zap.String("version_id", v.ID.String()), zap.String("route", p.Route))
}

func (r *Renderer) call(ctx context.Context, body posterCall) (*posterReply, error) {
	buf, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.aiBaseURL+"/api/v1/internal/creative/poster", bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(httpware.HeaderAPIKey, r.serviceKey)
	req.Header.Set("X-Tenant-ID", body.TenantID)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusPaymentRequired:
		return nil, errNoCredits
	case resp.StatusCode == http.StatusGone:
		return nil, errSceneGone
	case resp.StatusCode != http.StatusOK:
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return nil, fmt.Errorf("marketflow-ai %d: %s", resp.StatusCode, e.Error)
	}
	var out posterReply
	if err := json.Unmarshal(data, &out); err != nil || out.Poster.URL == "" || out.Poster.Key == "" {
		return nil, fmt.Errorf("marketflow-ai sent an unreadable poster reply")
	}
	return &out, nil
}

// fail records why a render did not finish. marketflow-ai already refunded.
func (r *Renderer) fail(ctx context.Context, id uuid.UUID, meta map[string]any, err error) {
	msg := "the poster could not be made right now; nothing was charged"
	if errors.Is(err, errNoCredits) || errors.Is(err, errSceneGone) {
		msg = err.Error()
	}
	r.logger.Warn("poster failed", zap.String("version_id", id.String()), zap.Error(err))
	_ = r.db.CreativeVersion.UpdateOneID(id).SetStatus(StatusFailed).SetError(msg).SetMetadata(meta).Exec(ctx)
}

func withTx(ctx context.Context, db *ent.Client, fn func(*ent.Tx) error) error {
	tx, err := db.Tx(ctx)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m)+4)
	for k, v := range m {
		out[k] = v
	}
	return out
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func intOf(v any) int {
	switch t := v.(type) {
	case int:
		return t
	case float64:
		return int(t)
	}
	return 0
}
