// Package middleware provides HTTP middleware for marketflow-api.
package middleware

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

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
