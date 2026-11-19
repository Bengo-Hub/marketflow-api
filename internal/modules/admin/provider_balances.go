package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// BalanceReading is one media provider's last balance as marketflow-ai's balance watcher
// stores it (providerbalance.Watcher, a JSON map of provider to reading in Redis, refreshed
// every 15 minutes, kept two hours). Its field names have no JSON tags on the writer's side.
type BalanceReading struct {
	Remaining float64   `json:"remaining"`
	Unit      string    `json:"unit"` // usd | characters
	LowMark   float64   `json:"low_mark"`
	Estimate  bool      `json:"estimate,omitempty"`
	At        time.Time `json:"at"`
	Low       bool      `json:"low"`
}

// WithBalances lets the handler read the balance watcher's readings from the shared Redis.
func (h *PlatformProviderHandler) WithBalances(rdb redis.UniversalClient, key string) *PlatformProviderHandler {
	h.balances, h.balancesKey = rdb, key
	return h
}

// readBalances decodes the watcher's map; an absent key means no reading yet.
func readBalances(ctx context.Context, rdb redis.UniversalClient, key string) (map[string]BalanceReading, error) {
	out := map[string]BalanceReading{}
	if rdb == nil || key == "" {
		return out, nil
	}
	raw, err := rdb.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeBalances(raw)
}

func decodeBalances(raw []byte) (map[string]BalanceReading, error) {
	out := map[string]BalanceReading{}
	var stored map[string]struct {
		Remaining float64
		Unit      string
		LowMark   float64
		Estimate  bool
		At        time.Time
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	for p, r := range stored {
		out[p] = BalanceReading{Remaining: r.Remaining, Unit: r.Unit, LowMark: r.LowMark, Estimate: r.Estimate, At: r.At,
			Low: r.LowMark > 0 && r.Remaining <= r.LowMark}
	}
	return out, nil
}

// ProviderBalances handles GET /admin/providers/balances: each media provider's last balance
// and when it was read, for the provider cards.
// @Summary Media provider balances
// @Tags Admin - Providers
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/providers/balances [get]
func (h *PlatformProviderHandler) ProviderBalances(w http.ResponseWriter, r *http.Request) {
	b, err := readBalances(r.Context(), h.balances, h.balancesKey)
	if err != nil {
		h.logger.Warn("provider balances not read", zap.Error(err))
		httpapi.JSON(w, http.StatusOK, map[string]any{"balances": map[string]BalanceReading{}, "error": "balances could not be read"})
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"balances": b})
}
