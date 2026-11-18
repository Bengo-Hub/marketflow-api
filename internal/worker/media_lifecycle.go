package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativeasset"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
)

// Media storage watch (Sprint 14): two jobs on the shared R2 buckets, once a day.
//
//  1. Lifecycle backstop. The hourly draft purge (creative.Purger) is the primary cleanup. R2
//     lifecycle rules are the backstop for files the purge misses: after a tenant's retention, R2
//     deletes its drafts and temporary renders itself. Keys are marketflow/<tenant>/<type>/..., and
//     R2 filters by prefix only, so there is one rule per tenant and type. Only tenants with live
//     files get rules, the tenants with the most live bytes first, up to MaxRules a bucket. Rules
//     this job owns have ids starting "mf-ttl-"; every other rule on the bucket (the
//     abort-incomplete-multipart rule from devops-k8s) is read and kept, like the object-storage
//     workflow does. video-drafts is managed only with MF_MEDIA_LIFECYCLE_VIDEO_DRAFTS=true, since
//     finished videos live there too; without it a video-drafts rule made earlier is removed.
//  2. Live-bytes alert. The bytes of every unpurged creative asset are summed, and above
//     LiveBytesAlert the platform is told once a day through PlatformAlert (auth-api's Slack
//     channel and the platform admins' email). The sum is MarketFlow's own record, so files the
//     database no longer knows about are not counted; the R2 dashboard stays the full figure.

// retentionRulePrefix marks the lifecycle rules this job owns.
const retentionRulePrefix = "mf-ttl-"

// retentionTypes are the media types that hold drafts and temporary renders, by bucket. Finished
// work the owner keeps (brand images, approved assets) is never on this list.
var retentionTypes = map[string][]string{
	"public":  {"creative-drafts", "audio-drafts", typeVideoDrafts},
	"private": {"video-sources"},
}

// typeVideoDrafts holds finished videos as well as drafts. R2 deletes by age, not by use, so a
// video an ad or post is still showing could go; it is managed only when
// MF_MEDIA_LIFECYCLE_VIDEO_DRAFTS=true says the owner accepts that.
const typeVideoDrafts = "video-drafts"

// managedTypes is retentionTypes without video-drafts unless it is switched on.
func managedTypes(videoDrafts bool) map[string][]string {
	out := make(map[string][]string, len(retentionTypes))
	for bucket, types := range retentionTypes {
		for _, t := range types {
			if t == typeVideoDrafts && !videoDrafts {
				continue
			}
			out[bucket] = append(out[bucket], t)
		}
	}
	return out
}

// keyService is the first key segment of MarketFlow's files (objectstore.New("marketflow", ...)).
const keyService = "marketflow"

// RetentionPolicy is the parsed MF_MEDIA_RETENTION: default days and per-plan days.
type RetentionPolicy struct {
	DefaultDays int            `json:"default_days"`
	Plans       map[string]int `json:"plans"`
	MinDays     int            `json:"-"`
	MaxDays     int            `json:"-"`
}

// ParseRetention reads the policy, falling back to 10 days when the JSON is unreadable.
func ParseRetention(raw string, minDays, maxDays int) (RetentionPolicy, error) {
	p := RetentionPolicy{DefaultDays: 10, MinDays: minDays, MaxDays: maxDays}
	var err error
	if strings.TrimSpace(raw) != "" {
		if e := json.Unmarshal([]byte(raw), &p); e != nil {
			err = fmt.Errorf("MF_MEDIA_RETENTION: %w", e)
			p = RetentionPolicy{DefaultDays: 10, MinDays: minDays, MaxDays: maxDays}
		}
	}
	if p.DefaultDays <= 0 {
		p.DefaultDays = 10
	}
	return p, err
}

// Days is a tenant's retention: its own override, else its plan's, else the default, held
// between MinDays and MaxDays so a typo can never delete a draft the app still shows.
func (p RetentionPolicy) Days(plan string, override int) int {
	d := p.DefaultDays
	if v, ok := p.Plans[strings.ToUpper(strings.TrimSpace(plan))]; ok && v > 0 {
		d = v
	}
	if override > 0 {
		d = override
	}
	if p.MinDays > 0 && d < p.MinDays {
		d = p.MinDays
	}
	if p.MaxDays > 0 && d > p.MaxDays {
		d = p.MaxDays
	}
	return d
}

// TenantMedia is one tenant with live files: its plan, override and live bytes.
type TenantMedia struct {
	ID        uuid.UUID
	Plan      string
	Override  int
	LiveBytes int64
}

// lifecycleRule is one R2 lifecycle rule in the Cloudflare API's shape.
type lifecycleRule struct {
	ID         string            `json:"id"`
	Enabled    bool              `json:"enabled"`
	Conditions map[string]string `json:"conditions"`
	Delete     *ageTransition    `json:"deleteObjectsTransition,omitempty"`
}

type ageTransition struct {
	Condition struct {
		Type   string `json:"type"`
		MaxAge int64  `json:"maxAge"`
	} `json:"condition"`
}

// DesiredRules builds this job's rules for one bucket: per tenant, the largest first, one rule per
// type in types (the bucket's managed types), at most maxRules.
func DesiredRules(types []string, tenants []TenantMedia, policy RetentionPolicy, maxRules int) (rules []lifecycleRule, dropped int) {
	if len(types) == 0 {
		return nil, 0
	}
	sorted := append([]TenantMedia(nil), tenants...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].LiveBytes != sorted[j].LiveBytes {
			return sorted[i].LiveBytes > sorted[j].LiveBytes
		}
		return sorted[i].ID.String() < sorted[j].ID.String()
	})
	for _, t := range sorted {
		if maxRules > 0 && len(rules)+len(types) > maxRules {
			dropped++
			continue
		}
		age := int64(policy.Days(t.Plan, t.Override)) * 86400
		for _, typ := range types {
			r := lifecycleRule{ID: retentionRulePrefix + t.ID.String() + "-" + typ, Enabled: true,
				Conditions: map[string]string{"prefix": keyService + "/" + t.ID.String() + "/" + typ + "/"}, Delete: &ageTransition{}}
			r.Delete.Condition.Type = "Age"
			r.Delete.Condition.MaxAge = age
			rules = append(rules, r)
		}
	}
	sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
	return rules, dropped
}

// mergeRules keeps the bucket's own rules (any id without the job's prefix, unchanged) and
// replaces the job's rules with desired. changed is false when nothing would change.
func mergeRules(current []json.RawMessage, desired []lifecycleRule) (out []json.RawMessage, changed bool, err error) {
	var mine []string
	for _, raw := range current {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &head) == nil && strings.HasPrefix(head.ID, retentionRulePrefix) {
			mine = append(mine, string(raw))
			continue
		}
		out = append(out, raw)
	}
	var want []string
	for _, r := range desired {
		b, err := json.Marshal(r)
		if err != nil {
			return nil, false, err
		}
		out = append(out, b)
		want = append(want, string(b))
	}
	return out, !sameRules(mine, want), nil
}

// sameRules compares rule sets by meaning, not key order.
func sameRules(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	norm := func(list []string) []string {
		out := make([]string, 0, len(list))
		for _, s := range list {
			var v any
			if json.Unmarshal([]byte(s), &v) != nil {
				out = append(out, s)
				continue
			}
			n, _ := json.Marshal(v) // maps marshal with sorted keys
			out = append(out, string(n))
		}
		sort.Strings(out)
		return out
	}
	na, nb := norm(a), norm(b)
	for i := range na {
		if na[i] != nb[i] {
			return false
		}
	}
	return true
}

// R2Lifecycle reads and writes a bucket's lifecycle rules through Cloudflare's API.
type R2Lifecycle struct {
	Base, AccountID, Token string
	HC                     *http.Client
}

func (c R2Lifecycle) url(bucket string) string {
	return strings.TrimRight(c.Base, "/") + "/accounts/" + url.PathEscape(c.AccountID) + "/r2/buckets/" + url.PathEscape(bucket) + "/lifecycle"
}

func (c R2Lifecycle) Get(ctx context.Context, bucket string) ([]json.RawMessage, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(bucket), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read lifecycle of %s: status %d", bucket, resp.StatusCode)
	}
	var out struct {
		Result struct {
			Rules []json.RawMessage `json:"rules"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return out.Result.Rules, nil
}

func (c R2Lifecycle) Put(ctx context.Context, bucket string, rules []json.RawMessage) error {
	if rules == nil {
		rules = []json.RawMessage{}
	}
	body, _ := json.Marshal(map[string]any{"rules": rules})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.url(bucket), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("set lifecycle on %s: status %d", bucket, resp.StatusCode)
	}
	return nil
}

// MediaUsage reads which tenants have live files and how many bytes (EntMediaUsage in production).
type MediaUsage interface {
	Tenants(ctx context.Context) ([]TenantMedia, error)
}

// Alerter is the platform alert path (PlatformAlert).
type Alerter interface {
	Send(ctx context.Context, subject, text, kind, key string)
}

// MediaLifecycle runs the two daily jobs.
type MediaLifecycle struct {
	usage          MediaUsage
	r2             *R2Lifecycle // nil: rules are not synced
	buckets        map[string]string
	types          map[string][]string
	policy         RetentionPolicy
	maxRules       int
	liveBytesAlert int64
	alert          Alerter
	claim          func(ctx context.Context, key string, ttl time.Duration) bool
	now            func() time.Time
	logger         *zap.Logger
}

// NewMediaLifecycle builds the watch. publicBucket and privateBucket are the object store's
// buckets; without an R2 account and token, or buckets, only the alert runs.
func NewMediaLifecycle(cfg MediaStorageConfig, publicBucket, privateBucket string, usage MediaUsage, alert Alerter,
	claim func(ctx context.Context, key string, ttl time.Duration) bool, logger *zap.Logger) *MediaLifecycle {
	logger = logger.Named("media-lifecycle")
	policy, err := ParseRetention(cfg.Retention, cfg.MinDays, cfg.MaxDays)
	if err != nil {
		logger.Warn("media retention: using the defaults", zap.Error(err))
	}
	m := &MediaLifecycle{usage: usage, buckets: map[string]string{"public": publicBucket, "private": privateBucket},
		types:  managedTypes(cfg.LifecycleVideoDrafts),
		policy: policy, maxRules: cfg.MaxRules, liveBytesAlert: cfg.LiveBytesAlert, alert: alert, claim: claim, now: time.Now, logger: logger}
	if !cfg.LifecycleVideoDrafts {
		logger.Info("R2 lifecycle leaves video-drafts alone: R2 deletes by age, and a finished video can still be in use by a running ad or post; set MF_MEDIA_LIFECYCLE_VIDEO_DRAFTS=true to manage it")
	}
	if cfg.R2AccountID != "" && cfg.R2LifecycleToken != "" && publicBucket != "" && privateBucket != "" {
		m.r2 = &R2Lifecycle{Base: cfg.R2APIBase, AccountID: cfg.R2AccountID, Token: cfg.R2LifecycleToken, HC: &http.Client{Timeout: 30 * time.Second}}
	} else {
		logger.Info("R2 lifecycle sync off: MF_MEDIA_R2_ACCOUNT_ID, MF_MEDIA_R2_LIFECYCLE_TOKEN or the buckets are not set")
	}
	return m
}

// Run checks hourly and works once a day (one pod at a time through claim).
func (m *MediaLifecycle) Run(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		day := m.now().UTC().Format("2006-01-02")
		if m.claim == nil || m.claim(ctx, "mf:media-lifecycle:"+day, 26*time.Hour) {
			if err := m.Once(ctx); err != nil {
				m.logger.Warn("media storage watch failed", zap.Error(err))
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Once runs both jobs one time.
func (m *MediaLifecycle) Once(ctx context.Context) error {
	tenants, err := m.usage.Tenants(ctx)
	if err != nil {
		return err
	}
	m.checkLiveBytes(ctx, tenants)
	if m.r2 == nil {
		return nil
	}
	var firstErr error
	for _, side := range []string{"public", "private"} {
		if err := m.syncBucket(ctx, side, tenants); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (m *MediaLifecycle) syncBucket(ctx context.Context, side string, tenants []TenantMedia) error {
	bucket := m.buckets[side]
	current, err := m.r2.Get(ctx, bucket)
	if err != nil {
		return err
	}
	own := 0
	for _, raw := range current {
		var head struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &head) == nil && !strings.HasPrefix(head.ID, retentionRulePrefix) {
			own++
		}
	}
	desired, dropped := DesiredRules(m.types[side], tenants, m.policy, m.maxRules-own)
	if dropped > 0 {
		m.logger.Warn("lifecycle rule cap reached; the smallest tenants rely on the hourly purge",
			zap.String("bucket", bucket), zap.Int("tenants_without_rules", dropped))
	}
	merged, changed, err := mergeRules(current, desired)
	if err != nil || !changed {
		return err
	}
	if err := m.r2.Put(ctx, bucket, merged); err != nil {
		return err
	}
	m.logger.Info("lifecycle rules synced", zap.String("bucket", bucket), zap.Int("rules", len(desired)), zap.Int("kept", own))
	return nil
}

// checkLiveBytes alerts the platform once a day while live bytes are above the level.
func (m *MediaLifecycle) checkLiveBytes(ctx context.Context, tenants []TenantMedia) {
	if m.liveBytesAlert <= 0 || m.alert == nil {
		return
	}
	var total int64
	for _, t := range tenants {
		total += t.LiveBytes
	}
	if total <= m.liveBytesAlert {
		return
	}
	top := append([]TenantMedia(nil), tenants...)
	sort.Slice(top, func(i, j int) bool { return top[i].LiveBytes > top[j].LiveBytes })
	if len(top) > 5 {
		top = top[:5]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "MarketFlow media storage is at %s of live files, above the %s alert level.\n", gib(total), gib(m.liveBytesAlert))
	b.WriteString("Largest tenants:\n")
	for _, t := range top {
		fmt.Fprintf(&b, "- %s: %s\n", t.ID, gib(t.LiveBytes))
	}
	b.WriteString("\nThe hourly draft purge and the R2 lifecycle rules remove drafts after their retention. " +
		"If the level keeps rising, check for drafts without an expiry, shorten MF_MEDIA_RETENTION for the largest plans, or raise the R2 budget.")
	m.alert.Send(ctx, "MarketFlow media storage above "+gib(m.liveBytesAlert), b.String(), "media_live_bytes", m.now().UTC().Format("2006-01-02"))
}

func gib(n int64) string { return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30)) }

// EntMediaUsage reads live files from creative_asset (unpurged rows) and each tenant's plan and
// media_retention_days override.
type EntMediaUsage struct{ DB *ent.Client }

func (u EntMediaUsage) Tenants(ctx context.Context) ([]TenantMedia, error) {
	var rows []struct {
		TenantID uuid.UUID `json:"tenant_id"`
		Sum      int64     `json:"sum"`
	}
	if err := u.DB.CreativeAsset.Query().Where(creativeasset.PurgedAtIsNil()).
		GroupBy(creativeasset.FieldTenantID).Aggregate(ent.Sum(creativeasset.FieldBytes)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = r.TenantID
	}
	ts, err := u.DB.Tenant.Query().Where(tenant.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[uuid.UUID]*ent.Tenant, len(ts))
	for _, t := range ts {
		byID[t.ID] = t
	}
	out := make([]TenantMedia, 0, len(rows))
	for _, r := range rows {
		tm := TenantMedia{ID: r.TenantID, LiveBytes: r.Sum}
		if t := byID[r.TenantID]; t != nil {
			if t.SubscriptionPlan != nil {
				tm.Plan = *t.SubscriptionPlan
			}
			if v, ok := t.Metadata["media_retention_days"].(float64); ok {
				tm.Override = int(v)
			}
		}
		out = append(out, tm)
	}
	return out, nil
}
