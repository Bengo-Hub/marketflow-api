package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"go.uber.org/zap"
)

// GraphVersionWatch tells the platform owner when Meta releases a Graph API version newer than
// the one MarketFlow pins (meta.GraphVersion). Upgrades stay a reviewed, tested change (owner,
// 2026-11-10): Meta keeps each version for about two years, and a new one can rename or drop
// fields MarketFlow reads, so the job only alerts.
type GraphVersionWatch struct {
	graphRoot string // https://graph.facebook.com
	current   string
	hc        *http.Client
	logger    *zap.Logger
	appToken  func(context.Context) (string, bool)
	tell      func(ctx context.Context, current, newer string)
	claim     func(ctx context.Context, key string, ttl time.Duration) bool
	now       func() time.Time
}

func NewGraphVersionWatch(db *ent.Client, keys *encryption.KeyProvider, notif *notifications.Client, platformSlug string,
	claim func(ctx context.Context, key string, ttl time.Duration) bool, logger *zap.Logger) *GraphVersionWatch {
	w := &GraphVersionWatch{graphRoot: "https://graph.facebook.com", current: meta.GraphVersion,
		hc: &http.Client{Timeout: 15 * time.Second}, logger: logger.Named("graph-version-watch"), claim: claim, now: time.Now}
	w.appToken = func(ctx context.Context) (string, bool) {
		app, err := loadProviderSettings(ctx, db, keys, "platform", "meta_ads", "oauth")
		if err != nil || app["app_id"] == "" || app["app_secret"] == "" {
			return "", false
		}
		return app["app_id"] + "|" + app["app_secret"], true
	}
	w.tell = func(ctx context.Context, current, newer string) {
		w.logger.Warn("a newer Meta Graph API version is out", zap.String("pinned", current), zap.String("newer", newer))
		if notif == nil {
			return
		}
		t, err := db.Tenant.Query().Where(tenant.SlugEQ(platformSlug)).Only(ctx)
		if err != nil {
			return
		}
		emails, err := rbac.TenantAdminEmails(ctx, db, t.ID, rbac.NoticeRoles)
		if err != nil {
			return
		}
		body := fmt.Sprintf("Meta has released Graph API %s. MarketFlow still calls %s (meta.GraphVersion in marketflow-api).\n\n"+
			"Nothing breaks today: Meta keeps each version for about two years. Plan the upgrade as a reviewed change: read the changelog "+
			"(https://developers.facebook.com/docs/graph-api/changelog/versions and the Marketing API changelog), check the endpoints "+
			"MarketFlow calls, run the tests, then bump the version.", newer, current)
		for _, e := range emails {
			_ = notif.Send(ctx, t.ID.String(), notifications.Message{Channel: "email", Recipient: e,
				Subject: "Meta Graph API " + newer + " is out (MarketFlow uses " + current + ")", Body: body,
				Metadata:       map[string]string{"source": "marketflow-worker", "kind": "graph_version"},
				IdempotencyKey: "graph-version-" + newer + "-" + e})
		}
	}
	return w
}

// nextGraphVersion turns "v27.0" into "v28.0"; "" when the version is not in that form.
func nextGraphVersion(v string) string {
	major, ok := strings.CutPrefix(strings.TrimSuffix(v, ".0"), "v")
	n, err := strconv.Atoi(major)
	if !ok || err != nil || n <= 0 {
		return ""
	}
	return fmt.Sprintf("v%d.0", n+1)
}

// check returns the next version when Meta answers for it, "" when MarketFlow is on the newest.
func (w *GraphVersionWatch) check(ctx context.Context) (string, error) {
	next := nextGraphVersion(w.current)
	if next == "" {
		return "", fmt.Errorf("pinned version %q is not vNN.0", w.current)
	}
	tok, ok := w.appToken(ctx)
	if !ok {
		return "", errors.New("the platform Meta app is not configured")
	}
	u := strings.TrimSuffix(w.graphRoot, "/") + "/" + next + "/app?" + url.Values{"access_token": {tok}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := w.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return next, nil
	}
	return "", nil
}

func (w *GraphVersionWatch) tick(ctx context.Context) {
	newer, err := w.check(ctx)
	if err != nil {
		w.logger.Warn("graph version check failed", zap.Error(err))
		return
	}
	if newer != "" {
		w.tell(ctx, w.current, newer)
	}
}

// Run checks once per ISO week (claimed, so a restart or a second pod never repeats it).
func (w *GraphVersionWatch) Run(ctx context.Context) {
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		year, week := w.now().UTC().ISOWeek()
		if w.claim == nil || w.claim(ctx, fmt.Sprintf("marketflow:graph-version-watch:%d-%02d", year, week), 8*24*time.Hour) {
			w.tick(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
