// Package shareddefaults is the one shared set of settings businesses inherit instead of each
// keeping a copy (plan item 76, owner 2026-11-25): the Forever profile defaults and products, the
// banned claims, and a business's logo and brand colour from its Codevertex account. Every reader
// of Tenant.metadata for these keys goes through Meta or Str.
//
// The owner's rule: a stored value equal to the default counts as inherited and follows it; a
// different value is the business's own and stays. Banned claims are always the shared list plus
// the business's own, so compliance never gets weaker.
package shareddefaults

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/flp"
	"github.com/bengobox/marketflow-api/internal/platform/svcconfig"
)

// Platform rows in service_config (tenant_id NULL), read by Watch.
const (
	// KeyForever overrides Forever defaults: a JSON object of the keys in codeDefaults.
	KeyForever = "shared.forever_defaults"
	// KeyClaims adds banned claims: {"all": ["..."], "pharmacy": ["..."]}.
	KeyClaims = "shared.banned_claims"
)

// AccountKey in tenant metadata mirrors the account's logo and colour (auth-api owns them), with
// the earlier values, so a stored copy of an earlier one follows the account. Only the tenant
// syncer writes it.
const AccountKey = "account_brand"

type set struct {
	forever map[string]any
	claims  map[string][]string
}

var loaded atomic.Pointer[set]

func current() *set {
	if s := loaded.Load(); s != nil {
		return s
	}
	return &set{forever: codeDefaults()}
}

// Load reads the platform rows over the code's values. A row that cannot be read is ignored.
func Load(ctx context.Context, db *ent.Client) {
	s := &set{forever: codeDefaults()}
	var over map[string]any
	if json.Unmarshal([]byte(svcconfig.Value(ctx, db, uuid.Nil, KeyForever)), &over) == nil {
		for k, v := range over {
			if def, ok := s.forever[k]; ok && usable(def, v) {
				s.forever[k] = v
			}
		}
	}
	_ = json.Unmarshal([]byte(svcconfig.Value(ctx, db, uuid.Nil, KeyClaims)), &s.claims)
	loaded.Store(s)
}

// usable: a default keeps its kind and is never emptied by a bad row.
func usable(def, v any) bool {
	switch x := v.(type) {
	case string:
		_, ok := def.(string)
		return ok && strings.TrimSpace(x) != ""
	case []any:
		_, ok := def.([]any)
		return ok && len(x) > 0
	}
	return false
}

// Watch loads the rows now and every interval until ctx ends.
func Watch(ctx context.Context, db *ent.Client, every time.Duration, logger *zap.Logger) {
	Load(ctx, db)
	go func() {
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				Load(ctx, db)
			}
		}
	}()
	logger.Info("shared defaults loaded")
}

// Forever is the current shared Forever default for a key, nil when there is none.
func Forever(key string) any { return current().forever[key] }

func isFLP(t *ent.Tenant) bool {
	return strings.EqualFold(strings.TrimSpace(t.BusinessType), flp.BusinessType)
}

// Meta is the tenant's metadata with every inherited key resolved. The stored row is not changed.
func Meta(t *ent.Tenant) map[string]any {
	if t == nil {
		return nil
	}
	out := make(map[string]any, len(t.Metadata)+8)
	for k, v := range t.Metadata {
		out[k] = v
	}
	if isFLP(t) {
		for k, def := range current().forever {
			// A key that is present, even empty, is the owner's unless it is a copy of a default.
			if v, ok := out[k]; !ok || isForeverDefault(k, v) {
				out[k] = def
			}
		}
		return out
	}
	acc := ReadAccount(t.Metadata)
	for k, v := range acc.values() {
		if cur, _ := out[k].(string); v != "" && (strings.TrimSpace(cur) == "" || acc.holds(k, cur)) {
			out[k] = v
		}
	}
	return out
}

// Str is one resolved metadata string.
func Str(t *ent.Tenant, key string) string {
	s, _ := Meta(t)[key].(string)
	return strings.TrimSpace(s)
}

// isForeverDefault: v is the current default, one the code ships, or one an earlier release shipped.
func isForeverDefault(key string, v any) bool {
	def, ok := current().forever[key]
	if !ok {
		return false
	}
	if s, isStr := v.(string); isStr && key == "logo_url" && strings.HasSuffix(strings.TrimSpace(s), "/images/logo/codevertex.png") {
		return true // the first Forever default, on either account domain
	}
	if same(v, def) || same(v, codeDefaults()[key]) {
		return true
	}
	for _, p := range past[key] {
		if same(v, p) {
			return true
		}
	}
	return false
}

func same(a, b any) bool {
	x, okX := a.(string)
	y, okY := b.(string)
	if okX && okY {
		return strings.TrimSpace(x) == strings.TrimSpace(y)
	}
	return reflect.DeepEqual(a, b)
}

// StripInherited keeps a profile save from storing copies: a value equal to what the business
// inherits is removed (it stays inherited), and clearing a Forever default stores an empty value,
// so it does not come back.
func StripInherited(t *ent.Tenant, patch map[string]any) {
	if isFLP(t) {
		for k, def := range current().forever {
			v, ok := patch[k]
			switch {
			case !ok:
			case v == nil:
				if _, list := def.([]any); list {
					patch[k] = []any{}
				} else {
					patch[k] = ""
				}
			case isForeverDefault(k, v):
				patch[k] = nil
			}
		}
		return
	}
	acc := ReadAccount(t.Metadata)
	for k := range acc.values() {
		if s, ok := patch[k].(string); ok && acc.holds(k, s) {
			patch[k] = nil
		}
	}
}

// Account is the mirror of the account's brand under AccountKey.
type Account struct {
	LogoURL      string   `json:"logo_url,omitempty"`
	PrimaryColor string   `json:"brand_primary_color,omitempty"`
	Past         []string `json:"past,omitempty"`
}

// maxPast bounds the remembered earlier values.
const maxPast = 20

func (a Account) values() map[string]string {
	return map[string]string{"logo_url": a.LogoURL, "brand_primary_color": a.PrimaryColor}
}

// holds: v is the account's value for key now, or one it had before.
func (a Account) holds(key, v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || a.values()[key] == "" {
		return false
	}
	if strings.EqualFold(v, a.values()[key]) {
		return true
	}
	for _, p := range a.Past {
		if strings.EqualFold(v, p) {
			return true
		}
	}
	return false
}

// ReadAccount reads the mirror.
func ReadAccount(meta map[string]any) Account {
	var a Account
	if raw, ok := meta[AccountKey]; ok {
		b, _ := json.Marshal(raw)
		_ = json.Unmarshal(b, &a)
	}
	return a
}

// Next is the mirror after the account shows logo and colour, and whether it changed. A value the
// account dropped leaves the mirror's last one in place, so an ad never loses its logo by accident.
func (a Account) Next(logo, colour string) (Account, bool) {
	out := a
	out.Past = append([]string(nil), a.Past...)
	move := func(cur *string, next string) {
		next = strings.TrimSpace(next)
		if next == "" || next == *cur {
			return
		}
		if *cur != "" {
			out.Past = append(out.Past, *cur)
		}
		*cur = next
	}
	move(&out.LogoURL, logo)
	move(&out.PrimaryColor, colour)
	if n := len(out.Past); n > maxPast {
		out.Past = out.Past[n-maxPast:]
	}
	return out, out.LogoURL != a.LogoURL || out.PrimaryColor != a.PrimaryColor
}
