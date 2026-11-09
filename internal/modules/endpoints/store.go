package endpoints

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

// ConfigPrefix keys each service's list in the platform-level ServiceConfig rows
// (endpoint_discovery.<service>), so discovery needs no table of its own.
const ConfigPrefix = "endpoint_discovery."

// EntStore keeps the lists in ServiceConfig, one platform row per service.
type EntStore struct{ DB *ent.Client }

func (s EntStore) row(ctx context.Context, service string) (*ent.ServiceConfig, error) {
	return s.DB.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil(), serviceconfig.ConfigKeyEQ(ConfigPrefix+service)).
		Only(ctx)
}

func (s EntStore) Load(ctx context.Context, service string) ([]Endpoint, error) {
	row, err := s.row(ctx, service)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Endpoint
	if err := json.Unmarshal([]byte(row.ConfigValue), &list); err != nil {
		return nil, fmt.Errorf("endpoint list for %s: %w", service, err)
	}
	return list, nil
}

// errConflict means another writer changed the row between our read and write.
var errConflict = errors.New("endpoint list changed while updating")

// Update reads a service's list, applies fn and writes the result only if nobody wrote in
// between (updated_at unchanged), retrying a few times. The weekly scan and the owner's approvals
// both write the same row from different pods, so a plain read-then-write could lose one.
func (s EntStore) Update(ctx context.Context, service string, fn func([]Endpoint) ([]Endpoint, error)) error {
	for attempt := 0; attempt < 4; attempt++ {
		err := s.update(ctx, service, fn)
		if !errors.Is(err, errConflict) {
			return err
		}
	}
	return errConflict
}

func (s EntStore) update(ctx context.Context, service string, fn func([]Endpoint) ([]Endpoint, error)) error {
	row, err := s.row(ctx, service)
	if err != nil && !ent.IsNotFound(err) {
		return err
	}
	var list []Endpoint
	if row != nil {
		if err := json.Unmarshal([]byte(row.ConfigValue), &list); err != nil {
			return fmt.Errorf("endpoint list for %s: %w", service, err)
		}
	}
	next, err := fn(list)
	if err != nil {
		return err
	}
	if next == nil {
		next = []Endpoint{}
	}
	raw, err := json.Marshal(next)
	if err != nil {
		return err
	}
	if row == nil {
		err := s.DB.ServiceConfig.Create().SetConfigKey(ConfigPrefix + service).SetConfigValue(string(raw)).
			SetConfigType("json").SetDescription("Endpoint discovery: " + service + " routes offered to agents once approved").
			Exec(ctx)
		if ent.IsConstraintError(err) {
			return errConflict // another pod created it first
		}
		return err
	}
	n, err := s.DB.ServiceConfig.Update().
		Where(serviceconfig.IDEQ(row.ID), serviceconfig.UpdatedAtEQ(row.UpdatedAt)).
		SetConfigValue(string(raw)).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return errConflict
	}
	return nil
}

// All loads every service's list.
func (s EntStore) All(ctx context.Context) (map[string][]Endpoint, error) {
	rows, err := s.DB.ServiceConfig.Query().
		Where(serviceconfig.TenantIDIsNil(), serviceconfig.ConfigKeyHasPrefix(ConfigPrefix)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]Endpoint, len(rows))
	for _, r := range rows {
		var list []Endpoint
		if err := json.Unmarshal([]byte(r.ConfigValue), &list); err != nil {
			continue
		}
		out[strings.TrimPrefix(r.ConfigKey, ConfigPrefix)] = list
	}
	return out, nil
}
