// Package middleware provides HTTP middleware for marketflow-api.
package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// UsagePublisher reports usage events to subscriptions-api so tenant
// consumption is tracked against plan limits. Fire-and-forget — a failure
// never blocks the request. Events are batched by the caller (e.g. after
// a lead is created, after a chat is answered) so this publisher is called
// at most once per action, not per middleware invocation.
type UsagePublisher struct {
	subscriptionsURL string
	apiKey           string
	http             *http.Client
	logger           *zap.Logger
}

// NewUsagePublisher creates a usage publisher.
// subscriptionsURL is the base URL of subscriptions-api (e.g. http://subscriptions-api.infra.svc.cluster.local:4000).
func NewUsagePublisher(subscriptionsURL, apiKey string, logger *zap.Logger) *UsagePublisher {
	return &UsagePublisher{
		subscriptionsURL: subscriptionsURL,
		apiKey:           apiKey,
		http:             &http.Client{Timeout: 5 * time.Second},
		logger:           logger.Named("usage"),
	}
}

// TrackUsage returns an HTTP middleware that fires a usage event after a
// successful mutation (2xx status). Wraps the ResponseWriter to capture the
// status code without buffering the body.
//
// The event is charged to the tenant the request really acted for (billing isolation, sprint
// audit item 4b):
//   - the tenant a handler recorded with SetUsageTenant, which public routes must do: they resolve
//     the tenant from the slug, and their X-Tenant-ID header is whatever the visitor sent;
//   - otherwise X-Tenant-ID, but only on a signed-in request, where tenantCheck has pinned the
//     header to the caller's own tenant (or a platform owner or service key acts for one).
//
// A public request that records no tenant is not charged, so no visitor can bill another tenant.
func (u *UsagePublisher) TrackUsage(metric string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			slot := &usageTenant{}
			r = r.WithContext(context.WithValue(r.Context(), usageTenantKey{}, slot))
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			if sw.status < 200 || sw.status >= 300 {
				return
			}
			tenantID := slot.id
			if tenantID == "" {
				if claims, ok := authclient.ClaimsFromContext(r.Context()); ok && claims != nil {
					tenantID = r.Header.Get("X-Tenant-ID")
				}
			}
			if _, err := uuid.Parse(tenantID); err != nil {
				return
			}
			u.Report(tenantID, metric, 1)
		})
	}
}

type usageTenantKey struct{}

type usageTenant struct{ id string }

// SetUsageTenant records the tenant a handler acted for, so TrackUsage charges that tenant. A
// handler behind a public route calls it with the tenant it resolved from the slug. Without
// TrackUsage on the route it does nothing.
func SetUsageTenant(ctx context.Context, tenantID string) {
	if slot, ok := ctx.Value(usageTenantKey{}).(*usageTenant); ok {
		slot.id = tenantID
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (sw *statusWriter) WriteHeader(code int) {
	sw.status = code
	sw.ResponseWriter.WriteHeader(code)
}

// Report sends a usage event to subscriptions-api. metric is one of:
// "leads_captured", "ai_chat_messages", "campaigns_active", "emails_sent".
// value is the increment (typically 1). Fire-and-forget — errors are logged.
func (u *UsagePublisher) Report(tenantID, metric string, value float64) {
	if u.subscriptionsURL == "" || tenantID == "" {
		return
	}
	go func() {
		payload, _ := json.Marshal(map[string]any{
			"metric_type":  metric,
			"service_name": "marketflow",
			"value":        value,
		})
		url := fmt.Sprintf("%s/api/v1/usage/report", u.subscriptionsURL)
		req, _ := http.NewRequest("POST", url, bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Tenant-ID", tenantID)
		if u.apiKey != "" {
			req.Header.Set("X-API-Key", u.apiKey)
		}
		resp, err := u.http.Do(req)
		if err != nil {
			u.logger.Debug("usage report failed", zap.String("metric", metric), zap.Error(err))
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			u.logger.Debug("usage report non-200", zap.String("metric", metric), zap.Int("status", resp.StatusCode))
		}
	}()
}
