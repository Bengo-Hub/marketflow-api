package worker

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	sharedcache "github.com/Bengo-Hub/cache"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/integrations/google"
	"github.com/bengobox/marketflow-api/internal/modules/googleads"
)

// ConversionDiagnostics asks Google how the conversions sent through the Data Manager API went.
// Google answers an upload with a request id only and processes it later (half an hour to a day),
// and has no webhook for it. The timings are Google's own advice (Data Manager, diagnostics page):
// first check after 30 minutes, then 1.3 times longer up to an hour, stop after 24 hours.
type ConversionDiagnostics struct {
	db     *ent.Client
	tokens *google.Tokens
	hc     *http.Client
	base   string
	now    func() time.Time
	logger *zap.Logger
}

const (
	diagnosticsTick  = 10 * time.Minute
	diagnosticsFirst = 30 * time.Minute
	diagnosticsGrow  = 1.3
	diagnosticsMax   = time.Hour
	diagnosticsStop  = 24 * time.Hour
	diagnosticsBatch = 500
)

// NewConversionDiagnostics builds the job.
func NewConversionDiagnostics(db *ent.Client, keys *encryption.KeyProvider, logger *zap.Logger) *ConversionDiagnostics {
	return &ConversionDiagnostics{db: db, hc: &http.Client{Timeout: 20 * time.Second}, now: time.Now, logger: logger.Named("conversion-diagnostics"),
		tokens: google.NewTokensFor(db, keys, google.ProviderGoogleAds, google.ProviderGoogleAdsName)}
}

// Run blocks until ctx is cancelled.
func (j *ConversionDiagnostics) Run(ctx context.Context) {
	t := time.NewTicker(diagnosticsTick)
	defer t.Stop()
	for {
		if sharedcache.ClaimPeriod(ctx, "marketflow:conversion-diagnostics", diagnosticsTick) {
			j.check(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// firstCheck is when a request sent now is first asked about.
func firstCheck(now time.Time, event string) googleads.Pending {
	return googleads.Pending{Event: event, At: now.UTC(), Next: now.UTC().Add(diagnosticsFirst), WaitMinutes: diagnosticsFirst.Minutes()}
}

func nextCheck(p googleads.Pending, now time.Time) googleads.Pending {
	p.WaitMinutes = min(p.WaitMinutes*diagnosticsGrow, diagnosticsMax.Minutes())
	p.Next = now.UTC().Add(time.Duration(p.WaitMinutes * float64(time.Minute)))
	return p
}

// check asks about every due request. where narrows the accounts (tests).
func (j *ConversionDiagnostics) check(ctx context.Context, where ...predicate.ConnectedAccount) {
	accs, err := j.db.ConnectedAccount.Query().
		Where(connectedaccount.AccountTypeEQ(googleads.AccountType), connectedaccount.StatusEQ("active"), googleads.WithPending).
		Where(where...).
		// Least recently touched first, so a large set is worked through over several runs.
		Order(ent.Asc(connectedaccount.FieldUpdatedAt)).Limit(diagnosticsBatch).All(ctx)
	if err != nil {
		j.logger.Warn("read pending conversions", zap.Error(err))
		return
	}
	for _, a := range accs {
		if ctx.Err() != nil {
			return
		}
		j.account(ctx, a)
	}
}

func (j *ConversionDiagnostics) account(ctx context.Context, a *ent.ConnectedAccount) {
	now := j.now()
	access := ""
	var tokenErr error
	for id, p := range googleads.PendingRequests(a) {
		if p.Next.After(now) {
			continue
		}
		if access == "" && tokenErr == nil {
			access, tokenErr = j.tokens.AccessToken(ctx, a)
		}
		var st google.RequestStatus
		err := tokenErr
		if err == nil {
			st, err = google.DataManager{HTTP: j.hc, Base: j.base, AccessToken: access}.RequestStatus(ctx, id)
		}
		log := j.logger.With(zap.String("tenant", a.TenantID.String()), zap.String("account", a.ExternalID),
			zap.String("event", p.Event), zap.String("request", id))
		outcome := googleads.Upload{Event: p.Event, At: now.UTC()}
		switch {
		case err == nil && st.Status == google.DMSuccess:
			outcome.Status = googleads.UploadAccepted
			log.Info("google counted the conversion", zap.Strings("warnings", st.Warnings))
		case err == nil && (st.Status == google.DMFailed || st.Status == google.DMPartial):
			outcome.Status, outcome.Reason = googleads.UploadFailed, processingReason(st.Errors)
			log.Warn("google refused the conversion", zap.String("status", st.Status), zap.Strings("reasons", st.Errors), zap.Strings("warnings", st.Warnings))
		case now.Sub(p.At) >= diagnosticsStop:
			outcome.Status = googleads.UploadUnknown
			outcome.Reason = "Google took the conversion but did not say within 24 hours whether it counted."
			log.Warn("no final answer about the conversion", zap.String("status", st.Status), zap.Error(err))
		default:
			if err != nil {
				log.Warn("conversion status not readable", zap.Error(err))
			}
			if serr := googleads.Reschedule(ctx, j.db, a.ID, id, nextCheck(p, now)); serr != nil {
				log.Warn("save next check", zap.Error(serr))
			}
			continue
		}
		if serr := googleads.Resolve(ctx, j.db, a.ID, id, outcome); serr != nil {
			log.Warn("save conversion outcome", zap.Error(serr))
		}
	}
}

// processingReason joins Google's reasons in plain words.
func processingReason(codes []string) string {
	seen := map[string]bool{}
	var out []string
	for _, c := range codes {
		if t := google.PlainReason(c); !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return "Google refused the conversion without giving a reason."
	}
	return strings.Join(out, " ")
}
