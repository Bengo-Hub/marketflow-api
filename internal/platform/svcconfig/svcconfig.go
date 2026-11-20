// Package svcconfig reads typed values from the service_config table, where a tenant's row
// overrides the platform's (tenant_id NULL) row. Settings that people change belong here or in
// provider settings, never in code (owner rule 2026-11-20); code keeps only documented fallbacks.
package svcconfig

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
)

// Value returns the tenant's value for key, else the platform's, else "" (tenant uuid.Nil reads
// the platform row only).
func Value(ctx context.Context, db *ent.Client, tenantID uuid.UUID, key string) string {
	q := db.ServiceConfig.Query().Where(serviceconfig.ConfigKey(key))
	if tenantID == uuid.Nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.Or(serviceconfig.TenantID(tenantID), serviceconfig.TenantIDIsNil()))
	}
	rows, err := q.Limit(2).All(ctx)
	if err != nil {
		return ""
	}
	platform := ""
	for _, r := range rows {
		if r.TenantID != nil && *r.TenantID == tenantID {
			return strings.TrimSpace(r.ConfigValue)
		}
		if r.TenantID == nil {
			platform = strings.TrimSpace(r.ConfigValue)
		}
	}
	return platform
}

// ParseDuration reads a Go duration ("24h") or a plain number of hours ("72"); ok is false for an
// empty or unreadable value or one outside [min, max] (a zero bound is not checked).
func ParseDuration(v string, min, max time.Duration) (time.Duration, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		h, herr := strconv.ParseFloat(v, 64)
		if herr != nil {
			return 0, false
		}
		d = time.Duration(h * float64(time.Hour))
	}
	if (min > 0 && d < min) || (max > 0 && d > max) {
		return 0, false
	}
	return d, true
}

// Duration is the tenant's (else the platform's) duration for key within [min, max], or fallback.
// Unlike Value it treats an invalid tenant value as unset and falls through to the platform's.
func Duration(ctx context.Context, db *ent.Client, tenantID uuid.UUID, key string, fallback, min, max time.Duration) time.Duration {
	q := db.ServiceConfig.Query().Where(serviceconfig.ConfigKey(key))
	if tenantID == uuid.Nil {
		q = q.Where(serviceconfig.TenantIDIsNil())
	} else {
		q = q.Where(serviceconfig.Or(serviceconfig.TenantID(tenantID), serviceconfig.TenantIDIsNil()))
	}
	rows, err := q.Limit(2).All(ctx)
	if err != nil {
		return fallback
	}
	var tenant, platform time.Duration
	for _, r := range rows {
		d, ok := ParseDuration(r.ConfigValue, min, max)
		if !ok {
			continue
		}
		if r.TenantID != nil && *r.TenantID == tenantID {
			tenant = d
		} else if r.TenantID == nil {
			platform = d
		}
	}
	switch {
	case tenant > 0:
		return tenant
	case platform > 0:
		return platform
	}
	return fallback
}
