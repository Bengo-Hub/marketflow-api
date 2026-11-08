// Package learning keeps what the Marketing OS learns from use (Sprint 25): how each business
// words its requests, so the next message with the same wording is understood with no model call.
// marketflow-ai decides and normalises; this package only stores and serves, as the data owner.
//
// Storage reuses service_config (metadata first, no new table): one row per (tenant, phrase) under
// the key "learned.intent.<intent>.<hash>". The unique (tenant_id, config_key) index makes the
// write an atomic upsert. A phrase is shared with every tenant once three tenants have used it,
// worked out on read.
package learning

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/serviceconfig"
	"github.com/google/uuid"
)

const (
	keyPrefix = "learned.intent."
	// MaxPhrasesPerIntent bounds one tenant's learned phrases for one intent.
	MaxPhrasesPerIntent = 200
	// SharedAfterTenants is how many tenants must use a phrase before every tenant gets it.
	SharedAfterTenants = 3
	maxShared          = 500
	maxPhraseLen       = 120
)

// ErrInvalid is a phrase or intent this store refuses.
var ErrInvalid = errors.New("invalid phrase or intent")

var (
	intentRe = regexp.MustCompile(`^[a-z][a-z_]{1,30}$`)
	// phraseRe is what marketflow-ai's normaliser produces: lowercase letters, digits and single
	// spaces. Anything else (an email, a phone number with symbols) was not normalised and is
	// refused, so nothing personal is stored.
	phraseRe   = regexp.MustCompile(`^[\p{Ll}\p{Lo}0-9]+( [\p{Ll}\p{Lo}0-9]+){0,11}$`)
	longDigits = regexp.MustCompile(`[0-9]{6,}`)
)

// Store keeps learned phrases.
type Store struct {
	db  *ent.Client
	now func() time.Time
}

func NewStore(db *ent.Client) *Store { return &Store{db: db, now: time.Now} }

type phraseValue struct {
	Phrase   string `json:"phrase"`
	Intent   string `json:"intent"`
	Count    int    `json:"count"`
	LastSeen string `json:"last_seen"`
}

func key(intent, phrase string) string {
	sum := sha1.Sum([]byte(phrase))
	return keyPrefix + intent + "." + hex.EncodeToString(sum[:10])
}

func valid(intent, phrase string) bool {
	return intentRe.MatchString(intent) && len(phrase) <= maxPhraseLen && phraseRe.MatchString(phrase) && !longDigits.MatchString(phrase)
}

// Record counts one use of a phrase for an intent. A tenant at its bound learns nothing new, but
// phrases it already has keep counting.
func (s *Store) Record(ctx context.Context, tenantID uuid.UUID, intent, phrase string) error {
	if !valid(intent, phrase) {
		return ErrInvalid
	}
	k := key(intent, phrase)
	row, err := s.db.ServiceConfig.Query().Where(serviceconfig.TenantID(tenantID), serviceconfig.ConfigKey(k)).Only(ctx)
	v := phraseValue{Phrase: phrase, Intent: intent}
	switch {
	case ent.IsNotFound(err):
		n, err := s.db.ServiceConfig.Query().
			Where(serviceconfig.TenantID(tenantID), serviceconfig.ConfigKeyHasPrefix(keyPrefix+intent+".")).Count(ctx)
		if err != nil {
			return err
		}
		if n >= MaxPhrasesPerIntent {
			return nil
		}
	case err != nil:
		return err
	default:
		_ = json.Unmarshal([]byte(row.ConfigValue), &v)
	}
	v.Count++
	v.LastSeen = s.now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(v)
	return s.db.ServiceConfig.Create().
		SetTenantID(tenantID).SetConfigKey(k).SetConfigValue(string(b)).SetConfigType("json").
		SetDescription("learned phrasing for " + intent).
		OnConflict(entsql.ConflictColumns(serviceconfig.FieldTenantID, serviceconfig.FieldConfigKey)).
		UpdateConfigValue().UpdateUpdatedAt().
		Exec(ctx)
}

// Phrases returns the tenant's own phrases for an intent plus the shared ones, deduplicated.
func (s *Store) Phrases(ctx context.Context, tenantID uuid.UUID, intent string) ([]string, error) {
	if !intentRe.MatchString(intent) {
		return nil, ErrInvalid
	}
	prefix := keyPrefix + intent + "."
	own, err := s.db.ServiceConfig.Query().
		Where(serviceconfig.TenantID(tenantID), serviceconfig.ConfigKeyHasPrefix(prefix)).
		Limit(MaxPhrasesPerIntent).All(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	add := func(raw string) {
		var v phraseValue
		if json.Unmarshal([]byte(raw), &v) == nil && v.Phrase != "" && !seen[v.Phrase] {
			seen[v.Phrase] = true
			out = append(out, v.Phrase)
		}
	}
	for _, r := range own {
		add(r.ConfigValue)
	}
	shared, err := s.shared(ctx, prefix)
	if err != nil {
		return out, err
	}
	for _, raw := range shared {
		add(raw)
	}
	return out, nil
}

// shared returns one stored value for each phrase that SharedAfterTenants tenants have used, in
// one grouped query: every tenant's row for a phrase holds the same phrase, so MIN picks any.
func (s *Store) shared(ctx context.Context, prefix string) ([]string, error) {
	var rows []struct {
		Value string `json:"config_value"`
	}
	err := s.db.ServiceConfig.Query().
		Where(serviceconfig.ConfigKeyHasPrefix(prefix), serviceconfig.TenantIDNotNil()).
		Limit(maxShared).
		Modify(func(sel *entsql.Selector) {
			// The HAVING argument goes through b.Arg: an ExprP "?" is not renumbered to $n for
			// Postgres, which produced invalid SQL.
			sel.Select(entsql.As("MIN("+sel.C(serviceconfig.FieldConfigValue)+")", "config_value")).
				GroupBy(sel.C(serviceconfig.FieldConfigKey)).
				Having(entsql.P(func(b *entsql.Builder) {
					b.WriteString("COUNT(DISTINCT " + sel.C(serviceconfig.FieldTenantID) + ") >= ").Arg(SharedAfterTenants)
				}))
		}).
		Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Value)
	}
	return out, nil
}

// clean trims an intent query value.
func clean(s string) string { return strings.TrimSpace(strings.ToLower(s)) }
