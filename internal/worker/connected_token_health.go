package worker

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/integrations/meta"
	"github.com/bengobox/marketflow-api/internal/integrations/tiktok"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
	"github.com/bengobox/marketflow-api/internal/modules/rbac"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Meta tokens cannot be renewed server side: a Page or user token that expired, or lost data
// access, comes back only when someone in the business connects again. So this job checks each
// active Meta account's token with Graph debug_token, records when it must next be renewed in
// expires_at, marks dead tokens expired, and emails the tenant's admins before and after.
//
// expires_at holds the token's own expiry, or Meta's data-access expiry when the token itself
// does not expire (Page tokens), or a recheck date when Meta reports neither. TikTok Marketing API
// tokens do not expire and are not checked. YouTube channels are renewed through their refresh
// token instead (checkGoogle); their expires_at stays empty.
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
	// youtube renews YouTube channel tokens; a refused renewal is the sign to reconnect.
	youtube *google.Tokens
	// tiktok renews TikTok account tokens the same way; a refused renewal is the sign to reconnect.
	tiktok *tiktok.Tokens
	// wa sends the WhatsApp copy of the reconnect notice (whatsapp_notice.go); nil sends email only.
	wa *waNotifier
}

// WithWhatsApp sends the reconnect notice on WhatsApp too (Sprint 14 and 17): to the approvers who
// use the assistant, inside their window as a text with a Reconnect button, outside it as
// mf_integration_reconnect once Meta approved it.
func (j *ConnectedTokenHealth) WithWhatsApp(rdb *redis.Client, wa waSender) *ConnectedTokenHealth {
	j.wa = newWANotifier(j.db, rdb, wa, tokenNoticeRoles, j.logger)
	return j
}

// maxReconnectList caps the account names in the WhatsApp template's parameter.
const maxReconnectList = 120

// reconnectWhatsApp is the WhatsApp copy of a reconnect notice. The template's parameters are the
// business and the accounts (mf_integration_reconnect_v1_btn, whose text is in the MarketFlow
// sprint-14 notes for the owner to submit).
func reconnectWhatsApp(business, slug, link, key string, list []tokenNotice) waNotice {
	names := make([]string, 0, len(list))
	stopped := false
	for _, n := range list {
		names = append(names, n.Name+" ("+strings.ReplaceAll(n.Type, "_", " ")+")")
		stopped = stopped || n.Expired
	}
	accounts := strings.Join(names, ", ")
	if r := []rune(accounts); len(r) > maxReconnectList {
		accounts = string(r[:maxReconnectList-3]) + "..."
	}
	what := "needs reconnecting soon"
	if stopped {
		what = "has stopped working"
	}
	text := fmt.Sprintf("Your MarketFlow connection %s: %s.\n\nUntil someone in your business connects it again, MarketFlow cannot post, read leads or run ads for it.", what, accounts)
	return waNotice{Text: text, Button: "Reconnect", Link: link,
		Kind: creative.TemplateIntegrationReconnect, Params: []string{business, accounts},
		URLSuffix: slug + "/settings/integrations", KeyPrefix: "conn-token-wa-" + key}
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
		graphBase: meta.GraphURL,
		appURL:    strings.TrimRight(appURL, "/"),
		interval:  24 * time.Hour,
		logger:    logger.Named("connected-token-health"),
	}
	j.appToken = j.metaAppToken
	j.youtube = google.NewTokens(db, keys)
	j.tiktok = tiktok.NewTokens(db, keys)
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
	notices := map[uuid.UUID][]tokenNotice{}
	checked := 0
	if appToken, ok := j.appToken(ctx); ok {
		n, err := j.checkMeta(ctx, appToken, now, notices)
		checked += n
		if err != nil {
			return checked, 0, err
		}
	} else {
		j.logger.Info("meta app credentials not configured; meta token check skipped")
	}
	if j.youtube != nil {
		n, err := j.checkGoogle(ctx, notices)
		checked += n
		if err != nil {
			return checked, 0, err
		}
	}
	if j.tiktok != nil {
		n, err := j.checkTikTok(ctx, notices)
		checked += n
		if err != nil {
			return checked, 0, err
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

// checkMeta checks the due Meta tokens with debug_token and adds the accounts to tell about.
func (j *ConnectedTokenHealth) checkMeta(ctx context.Context, appToken string, now time.Time, notices map[uuid.UUID][]tokenNotice) (int, error) {
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
			return checked, err
		}
		for _, acc := range rows {
			after = acc.ID
			if ctx.Err() != nil {
				return checked, ctx.Err()
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
	return checked, nil
}

// checkGoogle renews each active YouTube channel's access token once a day. The renewal is the
// check: Google answers invalid_grant once the person revoked access or the grant lapsed
// (google.Tokens then marks the row expired), and a daily use keeps a refresh token from
// lapsing after six months without use. Google down or slow is left for the next run.
func (j *ConnectedTokenHealth) checkGoogle(ctx context.Context, notices map[uuid.UUID][]tokenNotice) (int, error) {
	checked := 0
	var after uuid.UUID
	for batch := 0; batch < tokenHealthMaxBatches; batch++ {
		q := j.db.ConnectedAccount.Query().
			Where(
				connectedaccount.Platform("google"),
				connectedaccount.AccountType("youtube_channel"),
				connectedaccount.Status("active"),
				connectedaccount.RefreshCiphertextNEQ(""),
			).
			Order(ent.Asc(connectedaccount.FieldID)).
			Limit(tokenHealthBatch)
		if after != uuid.Nil {
			q = q.Where(connectedaccount.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return checked, err
		}
		for _, acc := range rows {
			after = acc.ID
			if ctx.Err() != nil {
				return checked, ctx.Err()
			}
			checked++
			_, err := j.youtube.Refresh(ctx, acc)
			var te *google.TokenError
			if err == nil || !errors.As(err, &te) || !te.Revoked() {
				if err != nil {
					j.logger.Warn("youtube token not renewed", zap.String("account", acc.ID.String()), zap.Error(err))
				}
				continue
			}
			name := acc.DisplayName
			if name == "" {
				name = acc.ExternalID
			}
			notices[acc.TenantID] = append(notices[acc.TenantID], tokenNotice{Name: name, Type: acc.AccountType, Expired: true})
		}
		if len(rows) < tokenHealthBatch {
			break
		}
	}
	return checked, nil
}

// checkOne updates one account from debug_token and says whether the tenant must be told.
// checkTikTok renews each active TikTok account's access token once a day, like checkGoogle:
// TikTok answers invalid_grant once the person removed MarketFlow or the refresh token lapsed
// (tiktok.Tokens then marks the row expired), and the daily use keeps the refresh token fresh.
// TikTok down or slow is left for the next run.
func (j *ConnectedTokenHealth) checkTikTok(ctx context.Context, notices map[uuid.UUID][]tokenNotice) (int, error) {
	checked := 0
	var after uuid.UUID
	for batch := 0; batch < tokenHealthMaxBatches; batch++ {
		q := j.db.ConnectedAccount.Query().
			Where(
				connectedaccount.Platform("tiktok"),
				connectedaccount.AccountType(tiktok.AccountCreator),
				connectedaccount.Status("active"),
				connectedaccount.RefreshCiphertextNEQ(""),
			).
			Order(ent.Asc(connectedaccount.FieldID)).
			Limit(tokenHealthBatch)
		if after != uuid.Nil {
			q = q.Where(connectedaccount.IDGT(after))
		}
		rows, err := q.All(ctx)
		if err != nil {
			return checked, err
		}
		for _, acc := range rows {
			after = acc.ID
			if ctx.Err() != nil {
				return checked, ctx.Err()
			}
			checked++
			_, err := j.tiktok.Refresh(ctx, acc)
			var te *tiktok.TokenError
			if err == nil || !errors.As(err, &te) || !te.Revoked() {
				if err != nil {
					j.logger.Warn("tiktok token not renewed", zap.String("account", acc.ID.String()), zap.Error(err))
				}
				continue
			}
			name := acc.DisplayName
			if name == "" {
				name = acc.ExternalID
			}
			notices[acc.TenantID] = append(notices[acc.TenantID], tokenNotice{Name: name, Type: acc.AccountType, Expired: true})
		}
		if len(rows) < tokenHealthBatch {
			break
		}
	}
	return checked, nil
}

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
	renewBy, basis := renewDate(st, now)
	// expiry_basis tells the integrations page whether expires_at is a real expiry or only the
	// next recheck, so a recheck date never shows as "expires soon" on the card. Merged with
	// jsonb || so the keys written at connect time stay.
	patch, _ := json.Marshal(map[string]string{"expiry_basis": basis})
	value := entsql.ExprFunc(func(b *entsql.Builder) {
		b.WriteString("(COALESCE(" + connectedaccount.FieldMetadata + ", '{}'::jsonb) || ")
		b.Arg(string(patch))
		b.WriteString("::jsonb)")
	})
	upd = upd.SetExpiresAt(renewBy).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(connectedaccount.FieldMetadata, value) })
	if err := upd.Exec(ctx); err != nil {
		j.logger.Warn("record token expiry", zap.String("account", acc.ID.String()), zap.Error(err))
		return notice, false
	}
	notice.ExpiresAt = renewBy
	return notice, renewBy.Before(now.Add(tokenRenewWindow))
}

// renewDate is the earliest of the token's expiry and its data-access expiry, or a recheck date
// when Meta reports neither, with what the date is: "token", "data_access" or "recheck".
func renewDate(st tokenStatus, now time.Time) (time.Time, string) {
	var d time.Time
	basis := ""
	if !st.ExpiresAt.IsZero() {
		d, basis = st.ExpiresAt, "token"
	}
	if !st.DataUntil.IsZero() && (d.IsZero() || st.DataUntil.Before(d)) {
		d, basis = st.DataUntil, "data_access"
	}
	if d.IsZero() {
		return now.Add(tokenRecheckAfter), "recheck"
	}
	return d, basis
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
	slug, business := "", "your business"
	if t, err := j.db.Tenant.Get(ctx, tenantID); err == nil {
		slug = t.Slug
		if strings.TrimSpace(t.Name) != "" {
			business = t.Name
		}
	}
	if slug != "" {
		link = j.appURL + "/" + slug + "/settings/integrations"
	}
	body := "MarketFlow can no longer post, read leads or run ads for these accounts unless someone in your business connects them again:\n\n" +
		strings.Join(lines, "\n") + "\n\nReconnect here: " + link
	key := "conn-token-" + tenantID.String() + "-" + hex.EncodeToString(sig.Sum(nil))[:16]
	// The WhatsApp copy uses the same list key, so it too goes once per list and dates.
	if j.wa != nil {
		j.wa.send(ctx, tenantID, reconnectWhatsApp(business, slug, link, key, list))
	}
	sent := false
	for _, email := range emails {
		err := j.notif.Send(ctx, tenantID.String(), notifications.Message{
			Channel:        "email",
			Recipient:      email,
			Subject:        "Reconnect your accounts in MarketFlow",
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
	return tenantAdminEmails(ctx, j.db, tenantID, tokenNoticeRoles)
}

// tenantAdminEmails is rbac.TenantAdminEmails, kept short for the worker jobs that email admins.
func tenantAdminEmails(ctx context.Context, db *ent.Client, tenantID uuid.UUID, roles []string) ([]string, error) {
	return rbac.TenantAdminEmails(ctx, db, tenantID, roles)
}
