package worker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent/publishjob"
	"github.com/bengobox/marketflow-api/internal/modules/analytics"
	"github.com/bengobox/marketflow-api/internal/modules/creative"
)

// fakeWA records what the notices sent. open lists the numbers whose 24-hour window is open;
// approved says whether the template would go (SendTemplate's own approval check).
type fakeWA struct {
	open      map[string]bool
	approved  bool
	failCTA   bool
	ctas      []string // number|button|link|key
	templates []string // number|kind|params|suffix|key
}

func (f *fakeWA) InWindow(_ context.Context, to string) bool { return f.open[to] }

func (f *fakeWA) SendCTA(_ context.Context, to, _, button, link, key string) error {
	if f.failCTA {
		return errors.New("send failed")
	}
	f.ctas = append(f.ctas, to+"|"+button+"|"+link+"|"+key)
	return nil
}

func (f *fakeWA) SendTemplate(_ context.Context, to, kind string, params []string, suffix, _, key string) (bool, error) {
	if !f.approved {
		return false, nil
	}
	f.templates = append(f.templates, to+"|"+kind+"|"+strings.Join(params, ",")+"|"+suffix+"|"+key)
	return true, nil
}

func fixedRecipients(rs ...briefRecipient) func(context.Context, uuid.UUID) []briefRecipient {
	return func(context.Context, uuid.UUID) []briefRecipient { return rs }
}

// Inside the window a text with a button; outside it the template only when it has parameters and
// Meta approved it; otherwise nothing. Each person's send carries their own key.
func TestWANotifierSend(t *testing.T) {
	tid := uuid.New()
	n := waNotice{Text: "hello", Button: "Open", Link: "https://mf.example/acme/x", Kind: "weekly_summary",
		Params: []string{"Acme", "1"}, URLSuffix: "acme/x", KeyPrefix: "k"}
	wa := &fakeWA{open: map[string]bool{"254700000001": true}, approved: true}
	w := &waNotifier{wa: wa, recipients: fixedRecipients(
		briefRecipient{UserID: "u1", Number: "254700000001"}, // window open
		briefRecipient{UserID: "u2", Number: "254700000002"}, // window closed
	)}
	if sent := w.send(context.Background(), tid, n); sent != 2 {
		t.Fatalf("sent %d, want 2", sent)
	}
	if len(wa.ctas) != 1 || wa.ctas[0] != "254700000001|Open|https://mf.example/acme/x|k-u1" {
		t.Fatalf("ctas: %v", wa.ctas)
	}
	if len(wa.templates) != 1 || wa.templates[0] != "254700000002|weekly_summary|Acme,1|acme/x|k-u2" {
		t.Fatalf("templates: %v", wa.templates)
	}

	// Template not approved: the closed window gets nothing.
	wa = &fakeWA{open: map[string]bool{}, approved: false}
	w.wa = wa
	if sent := w.send(context.Background(), tid, n); sent != 0 || len(wa.templates) != 0 {
		t.Fatalf("unapproved template went: %d %v", sent, wa.templates)
	}

	// No parameters (the weekly summary without a credits figure): no template, even approved.
	wa = &fakeWA{open: map[string]bool{}, approved: true}
	w.wa = wa
	n.Params = nil
	if sent := w.send(context.Background(), tid, n); sent != 0 || len(wa.templates) != 0 {
		t.Fatalf("a template without parameters went: %d %v", sent, wa.templates)
	}

	// A failed send is not counted; nil notifier or sender sends nothing.
	w.wa = &fakeWA{open: map[string]bool{"254700000001": true}, failCTA: true}
	if sent := w.send(context.Background(), tid, waNotice{Text: "x"}); sent != 0 {
		t.Fatalf("failed send counted: %d", sent)
	}
	var none *waNotifier
	if none.send(context.Background(), tid, n) != 0 || (&waNotifier{}).send(context.Background(), tid, n) != 0 {
		t.Fatal("a notifier without a sender sent something")
	}
}

// The reconnect notice names the accounts (trimmed for the template), says whether one has stopped,
// links the integrations page and fills mf_integration_reconnect's parameters.
func TestReconnectWhatsApp(t *testing.T) {
	soon := []tokenNotice{{Name: "Acme Page", Type: "facebook_page", ExpiresAt: time.Now().Add(72 * time.Hour)}}
	n := reconnectWhatsApp("Acme Shop", "acme", "https://mf.example/acme/settings/integrations", "conn-token-x", soon)
	if !strings.Contains(n.Text, "needs reconnecting soon") || !strings.Contains(n.Text, "Acme Page (facebook page)") {
		t.Fatalf("text: %s", n.Text)
	}
	if n.Kind != creative.TemplateIntegrationReconnect || n.URLSuffix != "acme/settings/integrations" ||
		n.Button != "Reconnect" || n.KeyPrefix != "conn-token-wa-conn-token-x" {
		t.Fatalf("notice: %+v", n)
	}
	if len(n.Params) != 2 || n.Params[0] != "Acme Shop" || n.Params[1] != "Acme Page (facebook page)" {
		t.Fatalf("params: %v", n.Params)
	}

	var many []tokenNotice
	for i := 0; i < 20; i++ {
		many = append(many, tokenNotice{Name: "Account number " + uuid.NewString()[:8], Type: "instagram", Expired: i == 3})
	}
	n = reconnectWhatsApp("Acme Shop", "acme", "l", "k", many)
	if !strings.Contains(n.Text, "has stopped working") {
		t.Fatalf("an expired account must say stopped: %s", n.Text)
	}
	if r := []rune(n.Params[1]); len(r) != maxReconnectList || !strings.HasSuffix(n.Params[1], "...") {
		t.Fatalf("long list not trimmed to %d: %d %q", maxReconnectList, len(r), n.Params[1])
	}
}

// The weekly summary counts the week's published posts, links the dashboard, and fills the
// template only when the week's AI credits are known.
func TestWeeklySummaryWhatsApp(t *testing.T) {
	db := openWorkerTestDB(t)
	ctx := context.Background()
	tid := seedTenant(t, db, "wsum")
	t.Cleanup(func() { _, _ = db.PublishJob.Delete().Where(publishjob.TenantID(tid)).Exec(ctx) })
	tn := db.Tenant.GetX(ctx, tid)
	now := time.Now().UTC()
	d := analytics.WeekDigest{From: now.AddDate(0, 0, -7), To: now, Leads: 5, LeadsBefore: 2, Converted: 1}
	job := func(status string, at time.Time) {
		db.PublishJob.Create().SetTenantID(tid).SetAssetID(uuid.New()).SetConnectedAccountID(uuid.New()).
			SetPlatform("facebook").SetKind("organic").SetScheduledAt(at).SetStatus(status).
			SetDedupeKey(uuid.NewString()).SetPublishedAt(at).ExecX(ctx)
	}
	job("published", now.AddDate(0, 0, -2))
	job("published", now.AddDate(0, 0, -3))
	job("failed", now.AddDate(0, 0, -1))
	job("published", now.AddDate(0, 0, -20)) // the week before last

	w := NewWeeklyReport(db, nil, "https://mf.example", zap.NewNop())
	n := w.weeklySummaryWhatsApp(ctx, tn, d, "2026-47")
	if !strings.Contains(n.Text, "5 new leads (2 the week before), 1 converted, and 2 posts went out") {
		t.Fatalf("text: %s", n.Text)
	}
	if n.Link != "https://mf.example/"+tn.Slug+"/dashboard" || n.URLSuffix != tn.Slug+"/dashboard" ||
		n.Kind != creative.TemplateWeeklySummary || n.KeyPrefix != "weekly-report-wa-"+tid.String()+"-2026-47" {
		t.Fatalf("notice: %+v", n)
	}
	if n.Params != nil {
		t.Fatalf("no credits source: the template must not be filled: %v", n.Params)
	}
	w.creditsUsed = func(context.Context, uuid.UUID, time.Time, time.Time) (int, bool) { return 42, true }
	n = w.weeklySummaryWhatsApp(ctx, tn, d, "2026-47")
	if strings.Join(n.Params, ",") != tn.Name+",2,5,42" {
		t.Fatalf("params: %v", n.Params)
	}
}
