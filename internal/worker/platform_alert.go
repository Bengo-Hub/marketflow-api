package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"go.uber.org/zap"
)

// PlatformAlert is the platform's one alert path: a post to the Slack channel the platform owner
// set in auth-api (/s2s/platform-alert-settings, shared by every platform alert) and an email to
// the platform tenant's admins through notifications-api. Either half is skipped when it is not
// configured. Used by the model watch and the media storage watch.
type PlatformAlert struct {
	db       *ent.Client
	notif    *notifications.Client
	hc       *http.Client
	authURL  string
	s2sKey   string
	platform string
	logger   *zap.Logger
}

func NewPlatformAlert(db *ent.Client, notif *notifications.Client, authURL, s2sKey, platformSlug string, logger *zap.Logger) *PlatformAlert {
	return &PlatformAlert{db: db, notif: notif, hc: &http.Client{Timeout: 20 * time.Second},
		authURL: strings.TrimRight(authURL, "/"), s2sKey: s2sKey, platform: platformSlug, logger: logger.Named("platform-alert")}
}

// Send posts text to Slack and emails it with subject. kind labels the alert in the email's
// metadata; key makes each email idempotent (one per recipient and key).
func (a *PlatformAlert) Send(ctx context.Context, subject, text, kind, key string) {
	if hook := a.slackWebhook(ctx); hook != "" {
		body, _ := json.Marshal(map[string]string{"text": text})
		if req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body)); err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := a.hc.Do(req); err == nil {
				resp.Body.Close()
			} else {
				a.logger.Warn("slack post failed", zap.String("kind", kind), zap.Error(err))
			}
		}
	}
	if a.notif == nil || a.db == nil {
		return
	}
	t, err := a.db.Tenant.Query().Where(tenant.SlugEQ(a.platform)).Only(ctx)
	if err != nil {
		return
	}
	emails, err := rbac.TenantAdminEmails(ctx, a.db, t.ID, rbac.NoticeRoles)
	if err != nil {
		return
	}
	for _, e := range emails {
		_ = a.notif.Send(ctx, t.ID.String(), notifications.Message{
			Channel: "email", Recipient: e, Subject: subject, Body: text,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": kind},
			IdempotencyKey: kind + "-" + key + "-" + e,
		})
	}
}

// slackWebhook reads the platform alert channel from auth-api (admin-configured once, shared by
// every platform alert).
func (a *PlatformAlert) slackWebhook(ctx context.Context) string {
	if a.authURL == "" || a.s2sKey == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.authURL+"/api/v1/s2s/platform-alert-settings", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-API-Key", a.s2sKey)
	resp, err := a.hc.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var v struct {
		Slack string `json:"slack_webhook_url"`
	}
	if json.NewDecoder(resp.Body).Decode(&v) != nil || !strings.HasPrefix(v.Slack, "https://") {
		return ""
	}
	return v.Slack
}
