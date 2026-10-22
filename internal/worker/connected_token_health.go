package worker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Meta tokens cannot be renewed server side: a Page or user token that expired, or lost data
// access, comes back only when someone in the business connects again. So this job checks each
// active Meta account's token with Graph debug_token, records when it must next be renewed in
// expires_at, marks dead tokens expired, and emails the tenant's admins before and after.
//
// expires_at holds the token's own expiry, or Meta's data-access expiry when the token itself
// does not expire (Page tokens), or a recheck date when Meta reports neither. TikTok Marketing API
// tokens do not expire and are not checked.
const (
	tokenHealthBatch      = 200
	tokenHealthMaxBatches = 20
	tokenRenewWindow      = 7 * 24 * time.Hour
	tokenRecheckAfter     = 30 * 24 * time.Hour
)

// tokenNoticeRoles are the roles told about a token that needs reconnecting.
var tokenNoticeRoles = []string{"marketflow_admin", "fbo_owner"}

// ConnectedTokenHealth is the daily ConnectedAccount token check.
type ConnectedTokenHealth struct {
	db        *ent.Client
	keys      *encryption.KeyProvider
	notif     *notifications.Client
	hc        *http.Client
	graphBase string
	appURL    string
	interval  time.Duration
	logger    *zap.Logger
	// appToken returns Meta's app access token (app_id|app_secret); a field so tests need not
	// write the platform's provider settings.
	appToken func(context.Context) (string, bool)
}

// metaAppToken reads the platform Meta app credentials.
func (j *ConnectedTokenHealth) metaAppToken(ctx context.Context) (string, bool) {
	app, err := loadProviderSettings(ctx, j.db, j.keys, "platform", "meta_ads", "oauth")
	if err != nil || app["app_id"] == "" || app["app_secret"] == "" {
		return "", false
	}
	return app["app_id"] + "|" + app["app_secret"], true
}

// NewConnectedTokenHealth builds the job. appURL is the portal's base URL for the reconnect link.
func NewConnectedTokenHealth(db *ent.Client, keys *encryption.KeyProvider, notif *notifications.Client, appURL string, logger *zap.Logger) *ConnectedTokenHealth {
	j := &ConnectedTokenHealth{
		db: db, keys: keys, notif: notif,
		hc:        &http.Client{Timeout: 15 * time.Second},
		graphBase: "https://graph.facebook.com/v21.0",
		appURL:    strings.TrimRight(appURL, "/"),
		interval:  24 * time.Hour,
		logger:    logger.Named("connected-token-health"),
	}
	j.appToken = j.metaAppToken
	return j
}

// Run blocks until ctx is cancelled, checking once at start and then daily.
func (j *ConnectedTokenHealth) Run(ctx context.Context) {
	t := time.NewTicker(j.interval)
	defer t.Stop()
	j.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			j.tick(ctx)
		}
	}
}

func (j *ConnectedTokenHealth) tick(ctx context.Context) {
	if !sharedcache.ClaimPeriod(ctx, "marketflow:connected-token-health", j.interval) {
		return
	}
	checked, notices, err := j.check(ctx, time.Now().UTC())
	if err != nil {
		j.logger.Error("connected token check failed", zap.Int("checked_before_error", checked), zap.Error(err))
		return
	}
	if checked > 0 {
		j.logger.Info("connected tokens checked", zap.Int("checked", checked), zap.Int("tenants_notified", notices))
	}
}

// tokenStatus is what debug_token reported for one token.
type tokenStatus struct {
	Valid     bool
	ExpiresAt time.Time // zero: never
	DataUntil time.Time // zero: none reported
}

// tokenNotice is one account a tenant must reconnect.
type tokenNotice struct {
	Name      string
	Type      string
	Expired   bool
	ExpiresAt time.Time
}

// check runs one pass and returns the accounts checked and the tenants notified.
func (j *ConnectedTokenHealth) check(ctx context.Context, now time.Time) (int, int, error) {
	if j.keys == nil || !j.keys.Configured(ctx) {
		return 0, 0, nil
	}
	appToken, ok := j.appToken(ctx)
	if !ok {
		j.logger.Info("meta app credentials not configured; token check skipped")
		return 0, 0, nil
	}

	notices := map[uuid.UUID][]tokenNotice{}
	checked := 0
	var after uuid.UUID
	for batch := 0; batch < tokenHealthMaxBatches; batch++ {
		// Due: never checked, or within the renew window. Keyset by id so a long list is read
		// once, in bounded pages.
		q := j.db.ConnectedAccount.Query().
			Where(
				connectedaccount.Platform("meta"),
				connectedaccount.Status("active"),
				connectedaccount.TokenCiphertextNEQ(""),
				connectedaccount.Or(
					connectedaccount.ExpiresAtIsNil(),
					connectedaccount.ExpiresAtLT(now.Add(tokenRenewWindow)),
				),
			).
			Order(ent.Asc(connectedaccount.FieldID)).
			Limit(tokenHealthBatch)
		if after != uuid.Nil {
			q = q.Where(connectedaccount.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return checked, 0, err
		}
		for _, acc := range rows {
			after = acc.ID
			if ctx.Err() != nil {
				return checked, 0, ctx.Err()
			}
			n, ok := j.checkOne(ctx, acc, appToken, now)
			checked++
			if ok {
				notices[acc.TenantID] = append(notices[acc.TenantID], n)
			}
		}
		if len(rows) < tokenHealthBatch {
			break
		}
	}
	sent := 0
	for tid, list := range notices {
		if j.notify(ctx, tid, list) {
			sent++
		}
	}
	return checked, sent, nil
}

// checkOne updates one account from debug_token and says whether the tenant must be told.
func (j *ConnectedTokenHealth) checkOne(ctx context.Context, acc *ent.ConnectedAccount, appToken string, now time.Time) (tokenNotice, bool) {
	notice := tokenNotice{Name: acc.DisplayName, Type: acc.AccountType}
	if notice.Name == "" {
		notice.Name = acc.ExternalID
	}
	tok, err := j.keys.Decrypt(ctx, acc.TokenCiphertext)
	if err != nil || tok == "" {
		j.logger.Warn("connected token could not be decrypted", zap.String("account", acc.ID.String()), zap.Error(err))
		return notice, false
	}
	st, err := j.debugToken(ctx, tok, appToken)
	if err != nil {
		// Graph down or rate limited: try again on the next run.
		j.logger.Warn("debug_token failed", zap.String("account", acc.ID.String()), zap.Error(err))
		return notice, false
	}
	upd := j.db.ConnectedAccount.UpdateOneID(acc.ID)
	if !st.Valid {
		if err := upd.SetStatus("expired").Exec(ctx); err != nil {
			j.logger.Warn("mark token expired", zap.String("account", acc.ID.String()), zap.Error(err))
			return notice, false
		}
		notice.Expired = true
		return notice, true
	}
	renewBy := renewDate(st, now)
	if err := upd.SetExpiresAt(renewBy).Exec(ctx); err != nil {
		j.logger.Warn("record token expiry", zap.String("account", acc.ID.String()), zap.Error(err))
		return notice, false
	}
	notice.ExpiresAt = renewBy
	return notice, renewBy.Before(now.Add(tokenRenewWindow))
}

// renewDate is the earliest of the token's expiry and its data-access expiry, or a recheck date
// when Meta reports neither.
func renewDate(st tokenStatus, now time.Time) time.Time {
	var d time.Time
	for _, t := range []time.Time{st.ExpiresAt, st.DataUntil} {
		if !t.IsZero() && (d.IsZero() || t.Before(d)) {
			d = t
		}
	}
	if d.IsZero() {
		return now.Add(tokenRecheckAfter)
	}
	return d
}

func (j *ConnectedTokenHealth) debugToken(ctx context.Context, token, appToken string) (tokenStatus, error) {
	q := url.Values{"input_token": {token}, "access_token": {appToken}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.graphBase+"/debug_token?"+q.Encode(), nil)
	if err != nil {
		return tokenStatus{}, err
	}
	resp, err := j.hc.Do(req)
	if err != nil {
		return tokenStatus{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return tokenStatus{}, err
	}
	if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
		return tokenStatus{}, fmt.Errorf("graph debug_token: %s", resp.Status)
	}
	var out struct {
		Data struct {
			IsValid             bool  `json:"is_valid"`
			ExpiresAt           int64 `json:"expires_at"`
			DataAccessExpiresAt int64 `json:"data_access_expires_at"`
		} `json:"data"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return tokenStatus{}, fmt.Errorf("graph debug_token: %w", err)
	}
	// A top-level error is about the app token or the call, not the checked token: never mark
	// accounts expired on it.
	if out.Error != nil {
		return tokenStatus{}, fmt.Errorf("graph debug_token: %s", out.Error.Message)
	}
	st := tokenStatus{Valid: out.Data.IsValid}
	if out.Data.ExpiresAt > 0 {
		st.ExpiresAt = time.Unix(out.Data.ExpiresAt, 0).UTC()
	}
	if out.Data.DataAccessExpiresAt > 0 {
		st.DataUntil = time.Unix(out.Data.DataAccessExpiresAt, 0).UTC()
	}
	return st, nil
}

// notify emails the tenant's admins one message listing the accounts to reconnect. The
// idempotency key covers the exact list and dates, so a daily run inside the renew window does
// not repeat the same email.
func (j *ConnectedTokenHealth) notify(ctx context.Context, tenantID uuid.UUID, list []tokenNotice) bool {
	if j.notif == nil {
		return false
	}
	emails, err := j.adminEmails(ctx, tenantID)
	if err != nil || len(emails) == 0 {
		j.logger.Warn("no admin to tell about connected tokens", zap.String("tenant", tenantID.String()), zap.Error(err))
		return false
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Name < list[b].Name })
	var lines []string
	sig := sha1.New()
	for _, n := range list {
		when := "has stopped working"
		if !n.Expired {
			when = "needs reconnecting by " + n.ExpiresAt.Format("2 Jan 2006")
		}
		lines = append(lines, fmt.Sprintf("- %s (%s) %s", n.Name, strings.ReplaceAll(n.Type, "_", " "), when))
		fmt.Fprintf(sig, "%s|%t|%s;", n.Name, n.Expired, n.ExpiresAt.Format("2006-01-02"))
	}
	link := j.appURL + "/settings/integrations"
	if slug, err := j.tenantSlug(ctx, tenantID); err == nil && slug != "" {
		link = j.appURL + "/" + slug + "/settings/integrations"
	}
	body := "MarketFlow can no longer post, read leads or run ads for these accounts unless someone in your business connects Meta again:\n\n" +
		strings.Join(lines, "\n") + "\n\nReconnect here: " + link
	key := "conn-token-" + tenantID.String() + "-" + hex.EncodeToString(sig.Sum(nil))[:16]
	sent := false
	for _, email := range emails {
		err := j.notif.Send(ctx, tenantID.String(), notifications.Message{
			Channel:        "email",
			Recipient:      email,
			Subject:        "Reconnect your Meta accounts in MarketFlow",
			Body:           body,
			Metadata:       map[string]string{"source": "marketflow-worker", "kind": "connected_token_health"},
			IdempotencyKey: key + "-" + email,
		})
		if err != nil {
			j.logger.Warn("token notice not sent", zap.String("tenant", tenantID.String()), zap.Error(err))
			continue
		}
		sent = true
	}
	return sent
}

func (j *ConnectedTokenHealth) adminEmails(ctx context.Context, tenantID uuid.UUID) ([]string, error) {
	assignments, err := j.db.UserRoleAssignment.Query().
		Where(
			userroleassignment.TenantID(tenantID),
			userroleassignment.HasRoleWith(marketflowrole.RoleCodeIn(tokenNoticeRoles...)),
			userroleassignment.HasUserWith(marketflowuser.TenantID(tenantID), marketflowuser.Status("active")),
		).
		WithUser().
		Limit(20).
		All(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(assignments))
	for _, a := range assignments {
		if u := a.Edges.User; u != nil && u.Email != "" && !seen[u.Email] {
			seen[u.Email] = true
			out = append(out, u.Email)
		}
	}
	return out, nil
}

func (j *ConnectedTokenHealth) tenantSlug(ctx context.Context, tenantID uuid.UUID) (string, error) {
	t, err := j.db.Tenant.Get(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return t.Slug, nil
}
