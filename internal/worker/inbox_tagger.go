package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	sharedcache "github.com/Bengo-Hub/cache"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/modules/inbox"
	"github.com/bengobox/marketflow-api/internal/modules/marketingos"
	"github.com/bengobox/marketflow-api/internal/platform/reqid"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// InboxTagger tags new social comments and DMs with a buyer intent (Sprint 22). It reads only
// rows with metadata.intent_status = 'pending', through the partial index
// activities_inbox_pending, so the cost stays with the new items however large the inbox grows.
//
// The judgment comes from marketflow-ai (the decide layer: Jev, then the free fallback model).
// Policy is here, in code:
//   - injection probability at or above InjectionBar: "flagged", never answered automatically;
//   - intent confidence below IntentBar: "untagged", left for the owner;
//   - otherwise "tagged" with the intent;
//   - no decision three times running: "untagged" rather than retried forever.
type InboxTagger struct {
	db     *ent.Client
	http   *http.Client
	aiURL  string
	aiKey  string
	every  time.Duration
	batch  int
	logger *zap.Logger
	claim  func(ctx context.Context, key string, ttl time.Duration) bool
	// reply sends an auto reply; nil turns auto replies off (tests, no Graph client).
	reply func(ctx context.Context, tid uuid.UUID, a *ent.Activity, text string) error
}

// WithAutoReply turns on the guarded auto reply through the inbox's Graph sender.
func (t *InboxTagger) WithAutoReply(keys *encryption.KeyProvider, g *inbox.Graph) *InboxTagger {
	t.reply = func(ctx context.Context, tid uuid.UUID, a *ent.Activity, text string) error {
		return inbox.Send(ctx, t.db, keys, g, tid, a, text, nil)
	}
	return t
}

// inboxPendingPredicate matches the activities_inbox_pending index condition exactly.
const inboxPendingPredicate = "activity_type IN ('comment', 'dm') AND metadata->>'intent_status' = 'pending'"

const (
	// AutoReplyBar is the intent confidence an automatic answer needs (higher than tagging's).
	AutoReplyBar     = 0.7
	autoReplyCooloff = 24 * time.Hour
	InjectionBar     = 0.6
	IntentBar        = 0.55
	maxTagAttempts   = 3
	inboxTaggerEvery = 30 * time.Second
)

func NewInboxTagger(db *ent.Client, aiURL, aiKey string, logger *zap.Logger) *InboxTagger {
	return &InboxTagger{db: db, http: reqid.Client(20 * time.Second), aiURL: strings.TrimSuffix(aiURL, "/"), aiKey: aiKey,
		every: inboxTaggerEvery, batch: 50, logger: logger.Named("inbox-tagger"), claim: sharedcache.ClaimPeriod}
}

func (t *InboxTagger) Run(ctx context.Context) {
	tick := time.NewTicker(t.every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// One pod per period does the batch, so two pods never tag the same row twice.
			if t.claim(ctx, "marketflow:inbox-tagger", t.every) {
				if _, err := t.tick(ctx); err != nil {
					t.logger.Warn("inbox tagging failed", zap.Error(err))
				}
			}
		}
	}
}

type intentAnswer struct {
	Intent     string  `json:"intent"`
	Confidence float64 `json:"confidence"`
	Injection  float64 `json:"injection"`
	Fallback   bool    `json:"fallback"`
}

// tick tags one batch and returns how many rows it settled.
func (t *InboxTagger) tick(ctx context.Context) (int, error) {
	rows, err := t.db.Activity.Query().
		// The same literal text as the partial index predicate: with bind parameters Postgres
		// cannot prove the query implies the index condition and would scan instead.
		Where(func(s *entsql.Selector) {
			s.Where(entsql.ExprP(inboxPendingPredicate))
		}).
		Order(ent.Asc(activity.FieldPerformedAt)).
		Limit(t.batch).
		All(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	for _, a := range rows {
		patch := map[string]any{"tagged_at": time.Now().UTC().Format(time.RFC3339)}
		platform, _ := a.Metadata["platform"].(string)
		body := ""
		if a.Body != nil {
			body = *a.Body
		}
		ans, err := t.ask(ctx, body, platform, a.ActivityType)
		switch {
		case err != nil:
			attempts, _ := a.Metadata["tag_attempts"].(float64)
			if int(attempts)+1 < maxTagAttempts {
				patch = map[string]any{"tag_attempts": attempts + 1}
			} else {
				patch["intent_status"] = "untagged"
				patch["tag_error"] = "no decision available"
			}
		case ans.Injection >= InjectionBar:
			patch["intent_status"], patch["intent"] = "flagged", "injection"
			patch["intent_injection"] = ans.Injection
		case ans.Confidence < IntentBar || ans.Intent == "":
			patch["intent_status"] = "untagged"
			patch["intent_guess"], patch["intent_confidence"] = ans.Intent, ans.Confidence
		default:
			patch["intent_status"], patch["intent"], patch["intent_confidence"] = "tagged", ans.Intent, ans.Confidence
		}
		if err := inbox.MergeMetadata(ctx, t.db, a.ID, patch); err != nil {
			t.logger.Warn("inbox tag not saved", zap.String("activity", a.ID.String()), zap.Error(err))
			continue
		}
		done++
		if patch["intent_status"] == "tagged" && strings.HasPrefix(ans.Intent, "buyer_") && ans.Confidence >= AutoReplyBar {
			t.autoReply(ctx, a)
		}
	}
	return done, nil
}

func (t *InboxTagger) ask(ctx context.Context, text, platform, kind string) (intentAnswer, error) {
	body, _ := json.Marshal(map[string]string{"text": text, "platform": platform, "kind": kind})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.aiURL+"/api/v1/internal/inbox/intent", bytes.NewReader(body))
	if err != nil {
		return intentAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", t.aiKey)
	resp, err := t.http.Do(req)
	if err != nil {
		return intentAnswer{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return intentAnswer{}, fmt.Errorf("inbox intent: status %d", resp.StatusCode)
	}
	var a intentAnswer
	return a, json.NewDecoder(resp.Body).Decode(&a)
}

// autoReply answers a buyer with the tenant's approved message, inside every guardrail: the
// tenant switched it on and saved the text, the person got no automatic answer in the last 24
// hours, and a DM is still inside its 24 hour window. The outcome is kept on the item.
func (t *InboxTagger) autoReply(ctx context.Context, a *ent.Activity) {
	if t.reply == nil {
		return
	}
	tn, err := t.db.Tenant.Get(ctx, a.TenantID)
	if err != nil {
		return
	}
	on, _ := tn.Metadata[marketingos.InboxAutoReplyKey].(bool)
	text, _ := tn.Metadata[marketingos.InboxReplyTextKey].(string)
	if !on || strings.TrimSpace(text) == "" {
		return
	}
	skip := func(reason string) {
		_ = inbox.MergeMetadata(ctx, t.db, a.ID, map[string]any{"auto_reply": "skipped", "auto_reply_reason": reason})
	}
	if a.ActivityType == "dm" && time.Since(a.PerformedAt) > 23*time.Hour {
		skip("reply window closing")
		return
	}
	recent, err := t.db.Activity.Query().
		Where(activity.TenantID(a.TenantID), activity.EntityType(a.EntityType), activity.EntityID(a.EntityID),
			activity.DirectionEQ("outbound"), activity.PerformedAtGT(time.Now().Add(-autoReplyCooloff)),
			func(s *entsql.Selector) {
				s.Where(entsql.ExprP("(" + s.C(activity.FieldMetadata) + "->>'auto')::boolean IS TRUE"))
			}).
		Exist(ctx)
	if err != nil {
		return
	}
	if recent {
		skip("already answered automatically in the last 24 hours")
		return
	}
	vars := resolveNurtureVariables(ctx, t.db, nil, nil, a.TenantID)
	for k, v := range vars {
		if sv, ok := v.(string); ok {
			text = strings.ReplaceAll(text, "{{"+k+"}}", sv)
		}
	}
	if err := t.reply(ctx, a.TenantID, a, text); err != nil {
		t.logger.Warn("auto reply not sent", zap.String("activity", a.ID.String()), zap.Error(err))
		skip("send failed: " + err.Error())
		return
	}
	_ = inbox.MergeMetadata(ctx, t.db, a.ID, map[string]any{"auto_reply": "sent"})
}
