package adsmanager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/google/uuid"
)

// MetaEnabled reads the platform switch ProviderSetting tenant "platform", provider_type "ads",
// key "meta_enabled", value "true". No row means off: paid ads wait for Meta App Review.
func MetaEnabled(db *ent.Client) func(ctx context.Context) bool {
	return func(ctx context.Context) bool {
		row, err := db.ProviderSetting.Query().
			Where(providersetting.TenantIDEQ("platform"), providersetting.ProviderTypeEQ("ads"),
				providersetting.KeyEQ("meta_enabled"), providersetting.IsActiveEQ(true)).
			First(ctx)
		return err == nil && strings.EqualFold(strings.TrimSpace(row.Value), "true")
	}
}

// AICharge spends credits through marketflow-ai's internal charge route, the one owner of the
// subscriptions-api credit client.
func AICharge(aiURL, key string) func(ctx context.Context, tid uuid.UUID, units int64, idem, desc string) (bool, error) {
	hc := &http.Client{Timeout: 15 * time.Second}
	return func(ctx context.Context, tid uuid.UUID, units int64, idem, desc string) (bool, error) {
		body, _ := json.Marshal(map[string]any{"tenant_id": tid.String(), "units": units, "key": idem, "description": desc})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(aiURL, "/")+"/api/v1/internal/credits/charge", bytes.NewReader(body))
		if err != nil {
			return false, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-API-Key", key)
		resp, err := hc.Do(req)
		if err != nil {
			return false, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false, fmt.Errorf("credits charge: status %d", resp.StatusCode)
		}
		var dec struct {
			Allowed bool `json:"allowed"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&dec); err != nil {
			return false, err
		}
		return dec.Allowed, nil
	}
}
