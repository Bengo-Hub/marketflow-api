package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/bengobox/marketflow-api/internal/platform/modelcatalog"
)

// ModelWatch (owner, 2026-11-11): AI providers rename and release models, so the list MarketFlow
// routes to is confirmed against each provider's live model list once a day. A new model in a
// family MarketFlow uses is reported to the platform on Slack (auth-api's platform alert channels)
// and by email, for review: what it is and a recommendation to compare it with the current
// default. Switching stays a reviewed config change (platform providers and routing settings),
// never automatic here. The one automatic switch is the owner's own Sync models on the fal.ai or
// ElevenLabs card, which moves a media route to a newer version of its own family and reports it
// through the same alert path (admin/media_routes.go). The first run records what exists without
// alerting.

// modelSource is one provider's model list.
type modelSource struct {
	provider string
	category string // platform ProviderSetting type
	name     string // platform ProviderSetting name
	url      string
	auth     func(req *http.Request, key string)
	parse    func([]byte) (ids []string, next string)
	page     func(base, next string) string
}

func bearer(req *http.Request, key string) { req.Header.Set("Authorization", "Bearer "+key) }

var modelSources = []modelSource{
	{provider: "groq", category: "ai_llm", name: "groq", url: "https://api.groq.com/openai/v1/models", auth: bearer, parse: parseOpenAIModels},
	{provider: "openai", category: "ai_llm", name: "openai", url: "https://api.openai.com/v1/models", auth: bearer, parse: parseOpenAIModels},
	{provider: "anthropic", category: "ai_llm", name: "anthropic", url: "https://api.anthropic.com/v1/models?limit=1000",
		auth: func(req *http.Request, key string) {
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		}, parse: parseAnthropicModels,
		page: func(base, next string) string { return base + "&after_id=" + url.QueryEscape(next) }},
	{provider: "elevenlabs", category: "media", name: "elevenlabs", url: "https://api.elevenlabs.io/v1/models",
		auth: func(req *http.Request, key string) { req.Header.Set("xi-api-key", key) }, parse: parseElevenLabsModels},
	{provider: "fal", category: "media", name: "fal", url: "https://api.fal.ai/v1/models?limit=100",
		auth:  func(req *http.Request, key string) { req.Header.Set("Authorization", "Key "+key) },
		parse: parseFalModels,
		page:  func(base, next string) string { return base + "&cursor=" + url.QueryEscape(next) }},
}

// defaultModelFamilies are the families MarketFlow routes to today (marketflow-ai registry and
// media routes). The platform overrides them with ProviderSetting model_watch/families
// ({"families": {"fal": ["kling", ...]}}) without a deploy.
var defaultModelFamilies = map[string][]string{
	"groq":       {"llama", "qwen", "gpt-oss", "kimi"},
	"openai":     {"gpt-"},
	"anthropic":  {"claude-haiku", "claude-sonnet"},
	"elevenlabs": {"eleven_"},
	"fal":        {"seedance", "hailuo", "kling", "fabric", "boreal", "aurora", "seedream", "nano-banana", "elevenlabs", "pixverse", "minimax", "omnihuman", "ideogram"},
}

// The list parsers live in modelcatalog, shared with the admin Sync models action.
var (
	parseOpenAIModels     = modelcatalog.ParseOpenAI
	parseAnthropicModels  = modelcatalog.ParseAnthropic
	parseElevenLabsModels = modelcatalog.ParseElevenLabs
	parseFalModels        = modelcatalog.ParseFal
)

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// watched keeps the models in the watched families.
func watched(ids []string, families []string) []string {
	var out []string
	for _, id := range ids {
		lid := strings.ToLower(id)
		for _, f := range families {
			if f != "" && strings.Contains(lid, strings.ToLower(f)) {
				out = append(out, id)
				break
			}
		}
	}
	return out
}

// newWatchedModels are watched models not seen before.
func newWatchedModels(live []string, seen map[string]bool, families []string) []string {
	var out []string
	for _, id := range watched(live, families) {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// ModelWatch runs the daily check.
type ModelWatch struct {
	db       *ent.Client
	keys     *encryption.KeyProvider
	rdb      *redis.Client
	notif    *notifications.Client
	hc       *http.Client
	authURL  string
	s2sKey   string
	platform string
	claim    func(ctx context.Context, key string, ttl time.Duration) bool
	alert    *PlatformAlert
	logger   *zap.Logger
}

func NewModelWatch(db *ent.Client, keys *encryption.KeyProvider, rdb *redis.Client, notif *notifications.Client,
	authURL, s2sKey, platformSlug string, claim func(ctx context.Context, key string, ttl time.Duration) bool, logger *zap.Logger) *ModelWatch {
	return &ModelWatch{db: db, keys: keys, rdb: rdb, notif: notif, hc: &http.Client{Timeout: 20 * time.Second},
		authURL: strings.TrimRight(authURL, "/"), s2sKey: s2sKey, platform: platformSlug, claim: claim,
		alert: NewPlatformAlert(db, notif, authURL, s2sKey, platformSlug, logger), logger: logger.Named("model-watch")}
}

// Run checks once a day (one pod at a time through claim).
func (w *ModelWatch) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if w.claim == nil || w.claim(ctx, "mf:model-watch:"+time.Now().UTC().Format("2006-01-02"), 26*time.Hour) {
			w.check(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w *ModelWatch) families(ctx context.Context) map[string][]string {
	out := map[string][]string{}
	for k, v := range defaultModelFamilies {
		out[k] = v
	}
	cfg, err := loadProviderSettings(ctx, w.db, w.keys, "platform", "model_watch", "families")
	if err != nil || cfg["families"] == "" {
		return out
	}
	var over map[string][]string
	if json.Unmarshal([]byte(cfg["families"]), &over) == nil {
		for k, v := range over {
			out[k] = v
		}
	}
	return out
}

func (w *ModelWatch) check(ctx context.Context) {
	fams := w.families(ctx)
	found := map[string][]string{}
	for _, src := range modelSources {
		cfg, err := loadProviderSettings(ctx, w.db, w.keys, "platform", src.category, src.name)
		key := strings.TrimSpace(cfg["api_key"])
		if err != nil || key == "" {
			continue // provider not configured: nothing to watch
		}
		live, err := w.list(ctx, src, key)
		if err != nil {
			w.logger.Warn("model list failed", zap.String("provider", src.provider), zap.Error(err))
			continue
		}
		seenKey := "mf:model-watch:seen:" + src.provider
		seen, baseline := w.seen(ctx, seenKey)
		if fresh := newWatchedModels(live, seen, fams[src.provider]); len(fresh) > 0 && !baseline {
			found[src.provider] = fresh
		}
		w.remember(ctx, seenKey, watched(live, fams[src.provider]))
	}
	if len(found) > 0 {
		w.tell(ctx, found)
	}
}

func (w *ModelWatch) list(ctx context.Context, src modelSource, key string) ([]string, error) {
	var all []string
	next := ""
	for page := 0; page < 30; page++ {
		u := src.url
		if next != "" && src.page != nil {
			u = src.page(src.url, next)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		src.auth(req, key)
		resp, err := w.hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s models: status %d", src.provider, resp.StatusCode)
		}
		ids, n := src.parse(body)
		all = append(all, ids...)
		if n == "" || src.page == nil {
			break
		}
		next = n
	}
	return all, nil
}

// seen returns the models recorded before; baseline is true on the first run.
func (w *ModelWatch) seen(ctx context.Context, key string) (map[string]bool, bool) {
	out := map[string]bool{}
	if w.rdb == nil {
		return out, true
	}
	ids, err := w.rdb.SMembers(ctx, key).Result()
	if err != nil || len(ids) == 0 {
		return out, true
	}
	for _, id := range ids {
		out[id] = true
	}
	return out, false
}

func (w *ModelWatch) remember(ctx context.Context, key string, ids []string) {
	if w.rdb == nil || len(ids) == 0 {
		return
	}
	members := make([]any, len(ids))
	for i, id := range ids {
		members[i] = id
	}
	_ = w.rdb.SAdd(ctx, key, members...).Err()
}

// tell posts to the platform's Slack channel and emails the platform admins.
func (w *ModelWatch) tell(ctx context.Context, found map[string][]string) {
	providers := make([]string, 0, len(found))
	for p := range found {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	var b strings.Builder
	b.WriteString("New AI models from MarketFlow's providers, for review:\n")
	for _, p := range providers {
		fmt.Fprintf(&b, "- %s: %s\n", p, strings.Join(found[p], ", "))
	}
	b.WriteString("\nRecommendation: compare each with the model MarketFlow uses today for the same job (quality on a short test, price per output, limits). " +
		"If one is better or cheaper, switch it in Platform > Providers and the AI routing or output registry settings; no deploy is needed. " +
		"Old model names can be retired by the provider, so also check that the current defaults still appear in the lists.")
	text := b.String()
	w.logger.Info("new provider models", zap.Any("models", found))
	w.alert.Send(ctx, "New AI models to review", text, "model_watch", time.Now().UTC().Format("2006-01-02"))
}
