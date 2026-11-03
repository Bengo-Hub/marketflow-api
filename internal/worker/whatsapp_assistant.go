package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	eventslib "github.com/Bengo-Hub/shared-events"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowrole"
	"github.com/bengobox/marketflow-api/internal/ent/marketflowuser"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/ent/userroleassignment"
	"github.com/bengobox/marketflow-api/internal/notifications"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// WhatsAppAssistantHandler answers marketing requests sent to the platform WhatsApp number
// (Sprint 17). notifications-api publishes every platform-number message; this handler decides
// whether it is for the assistant, in this order, cheapest first:
//
//  1. once per message (Redis, keyed on the WhatsApp message id) and at most AssistantHourlyCap
//     messages an hour per sender;
//  2. the sender's phone is a verified phone of an auth-api user (GET /s2s/users/by-phone);
//  3. that user is an active MarketFlow user of the tenant with an approving role
//     (marketflow_admin or fbo_owner, the roles that may approve and post ads);
//  4. marketflow-ai's gate: marketing words, then Jev or the free fallback model, and the
//     tenant's AI credits; only then does a Marketing Manager turn run and spend a credit.
//
// A message that fails any step gets no reply and stays wherever notifications-api put it, so
// a business owner chatting with Codevertex is never answered by a bot by mistake.
type WhatsAppAssistantHandler struct {
	db          *ent.Client
	rdb         *redis.Client
	notif       *notifications.Client
	http        *http.Client
	authURL     string
	aiURL       string
	authKey     string // auth-api S2S key
	aiKey       string // marketflow-ai internal key
	portalURL   string
	platformTen string // the platform tenant's slug; its UUID is looked up once
	logger      *zap.Logger

	platformID uuid.UUID
}

// AssistantRoles may use the assistant: the same roles that approve an ad.
var AssistantRoles = []string{"marketflow_admin", "fbo_owner"}

const (
	AssistantHourlyCap = 30
	assistantSession   = 30 * time.Minute
	assistantPickTTL   = 10 * time.Minute
)

func NewWhatsAppAssistantHandler(db *ent.Client, rdb *redis.Client, notif *notifications.Client,
	authURL, authKey, aiURL, aiKey, portalURL, platformTenant string, logger *zap.Logger) *WhatsAppAssistantHandler {
	return &WhatsAppAssistantHandler{
		db: db, rdb: rdb, notif: notif, http: &http.Client{Timeout: 110 * time.Second},
		authURL: strings.TrimSuffix(authURL, "/"), aiURL: strings.TrimSuffix(aiURL, "/"),
		authKey: authKey, aiKey: aiKey, portalURL: strings.TrimSuffix(portalURL, "/"), platformTen: platformTenant,
		logger: logger.Named("whatsapp-assistant"),
	}
}

func (*WhatsAppAssistantHandler) Subject() string { return "notifications.whatsapp.inbound" }
func (*WhatsAppAssistantHandler) Durable() string { return "marketflow-whatsapp-assistant" }

type waInbound struct {
	PhoneNumberID string `json:"phone_number_id"`
	WaMessageID   string `json:"wa_message_id"`
	From          string `json:"from"`
	Type          string `json:"type"`
	Text          string `json:"text"`
	ContactName   string `json:"contact_name"`
}

func (h *WhatsAppAssistantHandler) Handle(ctx context.Context, msg jetstream.Msg) error {
	_, in, err := eventslib.DecodeEvent[waInbound](msg.Data())
	if err != nil || in.WaMessageID == "" || in.From == "" {
		return dropPoison(h.logger, msg, "decode whatsapp.inbound", fmt.Errorf("bad event: %v", err))
	}
	if h.rdb == nil || h.db == nil {
		return errors.New("whatsapp assistant needs redis and the database")
	}
	// 1. Once per message. The claim is short while the message is worked on and is given back
	//    on failure, so a retry can run; a finished message keeps its key for a day.
	key := "mf:wa:msg:" + in.WaMessageID
	ok, err := h.rdb.SetNX(ctx, key, "1", 10*time.Minute).Result()
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if err := h.process(ctx, in); err != nil {
		h.rdb.Del(context.WithoutCancel(ctx), key)
		return err
	}
	h.rdb.Expire(context.WithoutCancel(ctx), key, 24*time.Hour)
	return nil
}

func (h *WhatsAppAssistantHandler) process(ctx context.Context, in waInbound) error {
	sender := "+" + strings.TrimPrefix(in.From, "+")
	if n, err := h.rdb.Incr(ctx, "mf:wa:rate:"+in.From).Result(); err == nil {
		if n == 1 {
			h.rdb.Expire(ctx, "mf:wa:rate:"+in.From, time.Hour)
		}
		if n > AssistantHourlyCap {
			h.logger.Info("whatsapp assistant: sender over the hourly cap")
			return nil
		}
	}

	// 2. Who is it.
	user, err := h.lookup(ctx, sender)
	if err != nil {
		return err
	}
	if user == nil {
		return nil // not a verified user: the message is not for the assistant
	}

	// 3. Which of their businesses use MarketFlow with an approving role.
	tenants, err := h.eligibleTenants(ctx, user)
	if err != nil {
		return err
	}
	if len(tenants) == 0 {
		return nil
	}
	chosen, handled, err := h.pickTenant(ctx, in, tenants)
	if err != nil || handled {
		return err
	}

	// 4. The gate and the turn, in marketflow-ai. The answer is kept for the message, so a retry
	//    after a failed send resends it instead of paying for a second turn.
	ansKey := "mf:wa:ans:" + in.WaMessageID
	var ans assistAnswer
	if cached, err := h.rdb.Get(ctx, ansKey).Bytes(); err == nil && json.Unmarshal(cached, &ans) == nil {
		return h.reply(ctx, in, ans.Reply)
	}
	active := h.rdb.Exists(ctx, "mf:wa:session:"+in.From).Val() == 1
	ans, err = h.ask(ctx, chosen, user.UserID, in.Text, active)
	if err != nil {
		return err
	}
	if ans.Pass && ans.Reply != "" {
		if b, err := json.Marshal(ans); err == nil {
			h.rdb.Set(ctx, ansKey, b, time.Hour)
		}
	}
	if !ans.Pass {
		h.logger.Debug("whatsapp assistant: gate said no", zap.String("reason", ans.Verdict.Reason), zap.String("source", ans.Verdict.Source))
		return nil
	}
	h.rdb.Set(ctx, "mf:wa:session:"+in.From, chosen.ID.String(), assistantSession)
	if ans.Reply == "" {
		return nil
	}
	return h.reply(ctx, in, ans.Reply)
}

type phoneUser struct {
	UserID  string `json:"user_id"`
	Tenants []struct {
		TenantID string   `json:"tenant_id"`
		Slug     string   `json:"slug"`
		Roles    []string `json:"roles"`
	} `json:"tenants"`
}

// lookup asks auth-api who holds this verified phone; nil means nobody.
func (h *WhatsAppAssistantHandler) lookup(ctx context.Context, phone string) (*phoneUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.authURL+"/api/v1/s2s/users/by-phone?phone="+url.QueryEscape(phone), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", h.authKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("auth by-phone: %w", err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest:
		return nil, nil
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("auth by-phone: status %d", resp.StatusCode)
	}
	var u phoneUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil || u.UserID == "" {
		return nil, fmt.Errorf("auth by-phone: bad answer")
	}
	return &u, nil
}

type assistantTenant struct {
	ID   uuid.UUID
	Slug string
	Name string
}

// eligibleTenants keeps the user's tenants where MarketFlow itself (the owner of its roles) has
// them as an active user with an approving role. auth-api's role list is not trusted for this.
func (h *WhatsAppAssistantHandler) eligibleTenants(ctx context.Context, u *phoneUser) ([]assistantTenant, error) {
	authID, err := uuid.Parse(u.UserID)
	if err != nil {
		return nil, nil
	}
	ids := make([]uuid.UUID, 0, len(u.Tenants))
	for _, t := range u.Tenants {
		if id, err := uuid.Parse(t.TenantID); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := h.db.UserRoleAssignment.Query().
		Where(
			userroleassignment.TenantIDIn(ids...),
			userroleassignment.HasRoleWith(marketflowrole.RoleCodeIn(AssistantRoles...)),
			userroleassignment.HasUserWith(marketflowuser.AuthServiceUserID(authID), marketflowuser.Status("active")),
		).
		All(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[uuid.UUID]bool{}
	var keep []uuid.UUID
	for _, r := range rows {
		if !seen[r.TenantID] {
			seen[r.TenantID] = true
			keep = append(keep, r.TenantID)
		}
	}
	if len(keep) == 0 {
		return nil, nil
	}
	ts, err := h.db.Tenant.Query().Where(tenant.IDIn(keep...)).Order(ent.Asc(tenant.FieldName)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]assistantTenant, 0, len(ts))
	for _, t := range ts {
		out = append(out, assistantTenant{ID: t.ID, Slug: t.Slug, Name: t.Name})
	}
	return out, nil
}

// pickTenant chooses the business the message is for. One business: that one. Several: the one
// of the open session, else a numbered question, and the next message that is only a number
// answers it. handled is true when this message was the question or the answer.
func (h *WhatsAppAssistantHandler) pickTenant(ctx context.Context, in waInbound, tenants []assistantTenant) (assistantTenant, bool, error) {
	if len(tenants) == 1 {
		return tenants[0], false, nil
	}
	pickKey := "mf:wa:pick:" + in.From
	if n, err := strconv.Atoi(strings.TrimSpace(in.Text)); err == nil {
		if h.rdb.Exists(ctx, pickKey).Val() == 1 && n >= 1 && n <= len(tenants) {
			t := tenants[n-1]
			h.rdb.Del(ctx, pickKey)
			h.rdb.Set(ctx, "mf:wa:session:"+in.From, t.ID.String(), assistantSession)
			return t, true, h.reply(ctx, in, "Working for "+t.Name+" now. What would you like to make or post?")
		}
	}
	if cur, err := h.rdb.Get(ctx, "mf:wa:session:"+in.From).Result(); err == nil {
		for _, t := range tenants {
			if t.ID.String() == cur {
				return t, false, nil
			}
		}
	}
	// Only ask when the text looks like a marketing request; plain chat gets no question.
	if !looksLikeMarketing(in.Text) {
		return assistantTenant{}, true, nil
	}
	var b strings.Builder
	b.WriteString("You use MarketFlow for more than one business. Reply with the number of the one this is for, then send your request again:\n")
	for i, t := range tenants {
		fmt.Fprintf(&b, "\n%d. %s", i+1, t.Name)
	}
	h.rdb.Set(ctx, pickKey, "1", assistantPickTTL)
	return assistantTenant{}, true, h.reply(ctx, in, b.String())
}

// looksLikeMarketing is a cheap word check used only to decide whether to ask which business a
// message is for; the real gate runs in marketflow-ai.
func looksLikeMarketing(text string) bool {
	t := " " + strings.ToLower(text) + " "
	for _, w := range []string{" ad ", " ads ", "advert", "video", "poster", "tiktok", "instagram", "facebook", "campaign",
		"promo", "caption", "ugc", "boost", "marketing", "tangazo", "matangazo", " post "} {
		if strings.Contains(t, w) {
			return true
		}
	}
	return false
}

type assistAnswer struct {
	Pass    bool `json:"pass"`
	Verdict struct {
		Reason string `json:"reason"`
		Source string `json:"source"`
	} `json:"verdict"`
	Reply string `json:"reply"`
}

func (h *WhatsAppAssistantHandler) ask(ctx context.Context, t assistantTenant, userID, text string, active bool) (assistAnswer, error) {
	body, _ := json.Marshal(map[string]any{
		"tenant_id": t.ID.String(), "tenant_slug": t.Slug, "user_id": userID, "text": text,
		"session_active": active, "portal_url": h.portalURL,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.aiURL+"/api/v1/internal/os/whatsapp-ask", bytes.NewReader(body))
	if err != nil {
		return assistAnswer{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", h.aiKey)
	resp, err := h.http.Do(req)
	if err != nil {
		return assistAnswer{}, fmt.Errorf("whatsapp-ask: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return assistAnswer{}, fmt.Errorf("whatsapp-ask: status %d", resp.StatusCode)
	}
	var a assistAnswer
	if err := json.NewDecoder(resp.Body).Decode(&a); err != nil {
		return assistAnswer{}, fmt.Errorf("whatsapp-ask: %w", err)
	}
	return a, nil
}

// reply sends text from the platform number, inside the 24-hour window the inbound message
// just opened, so no template is needed. The platform tenant sends it.
func (h *WhatsAppAssistantHandler) reply(ctx context.Context, in waInbound, text string) error {
	if h.notif == nil {
		return nil
	}
	if h.platformID == uuid.Nil {
		t, err := h.db.Tenant.Query().Where(tenant.SlugEQ(h.platformTen)).Only(ctx)
		if err != nil {
			return fmt.Errorf("platform tenant %q: %w", h.platformTen, err)
		}
		h.platformID = t.ID
	}
	return h.notif.Send(ctx, h.platformID.String(), notifications.Message{
		Channel:        "whatsapp",
		Recipient:      "+" + strings.TrimPrefix(in.From, "+"),
		Body:           text,
		Metadata:       map[string]string{"source": "marketflow-worker", "kind": "whatsapp_assistant"},
		IdempotencyKey: "wa-assist-" + in.WaMessageID,
	})
}
