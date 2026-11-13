package creative

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Videos made from a Marketing Manager turn (Sprint 15 #134/#138, Sprint 17). marketflow-ai keeps
// the turn and its approval codes; it makes the video here through Studio's own routes with its
// service key: a brief with metadata {plan, source: "os_turn", run_id, requested_by, channel},
// then a video ad with the script the team already wrote and reviewed (VideoAdInput.Script), then
// the storyboard. For a turn asked on WhatsApp, the storyboard stills and the finished ad go back
// to the requester's number; marketflow-ai writes those messages (with the second approval code)
// and they are sent here through notifications-api, like the assistant's replies.

// Brief metadata written by marketflow-ai for a turn's video.
const (
	sourceOSTurn    = "os_turn"
	channelWhatsApp = "whatsapp"
)

// OS turn events marketflow-ai turns into WhatsApp messages.
const (
	OSTurnStoryboardReady = "storyboard_ready"
	OSTurnRenderDone      = "render_done"
	OSTurnRenderFailed    = "render_failed"
)

// osTurnNotifyTimeout bounds one message: marketflow-ai's reply plus the send.
const osTurnNotifyTimeout = 30 * time.Second

// insertScriptedVideoAd stores a video ad whose script was written by the Marketing Manager's
// team. The script gets the same checks as an owner's edit (scenes, photos, footage, the kind)
// and the version starts at script_ready, so no script job runs and no second script credit is
// spent. The storyboard, render quote, render charge and daily cap all stay as in Studio.
func (s *Service) insertScriptedVideoAd(ctx context.Context, tenantID uuid.UUID, brief *ent.CreativeBrief, in VideoAdInput,
	copyFields, meta map[string]any) (*ent.CreativeVersion, error) {
	if err := in.Script.validate(adPhotoCount(meta), len(in.FootageURLs), in.Kind); err != nil {
		return nil, err
	}
	copyFields["script"] = toMap(in.Script)
	meta["script_source"] = sourceOSTurn
	if cats := restrictedCategories(append(scriptText(copyFields), brief.Title, str(brief.Offering["name"]))...); len(cats) > 0 {
		meta["restricted_categories"] = cats
	}
	return s.insertVersionAs(ctx, tenantID, brief.ID, nil, KindVideoAd, in.Tier, in.Format, StatusScriptReady, copyFields, meta)
}

// briefCreator is who a new brief is recorded as made by: the signed-in user, or, for a service
// acting for a tenant (marketflow-ai making a turn's video), the metadata.requested_by user.
func briefCreator(r *http.Request, meta map[string]any) *uuid.UUID {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && c.IsService {
		if id, err := uuid.Parse(str(meta["requested_by"])); err == nil {
			return &id
		}
		return nil
	}
	return userOf(r)
}

// requireBrandImagesForTenant is requireBrandImages where a service acts for a tenant: a service
// key's claims carry the key's roles, not the tenant's exemptions, so the tenant's own brand
// images decide.
func (h *Handler) requireBrandImagesForTenant(w http.ResponseWriter, r *http.Request, tenantID uuid.UUID) bool {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil && c.IsService {
		plain := &authclient.Claims{IsService: true, ServiceName: c.ServiceName, TenantID: c.TenantID}
		r = r.WithContext(authclient.ContextWithClaims(r.Context(), plain))
	}
	return h.requireBrandImages(w, r, tenantID)
}

// OSTurnNote is one message about a WhatsApp turn's video: the storyboard (Links are the stills),
// the finished ad (Links[0] is the video), a failed render, or marketflow-ai's own background
// step failing (OSTurnMediaFailed, Error is the text to send). Key makes the send go once.
type OSTurnNote struct {
	TenantID  uuid.UUID
	RunID     string
	VersionID string
	Event     string
	Links     []string
	Error     string
	Key       string
}

// OSTurnNotifier tells the requester of a WhatsApp turn's video that it reached a step they wait for.
type OSTurnNotifier interface {
	Notify(ctx context.Context, n OSTurnNote)
}

// OSTurnMediaFailed is marketflow-ai's background make-media or render start failing (brand
// images missing, a Studio refusal, too few credits); marketflow-ai reports it here.
const OSTurnMediaFailed = "media_failed"

// WithOSTurn turns on the WhatsApp messages about turns' videos.
func (r *Renderer) WithOSTurn(n OSTurnNotifier) *Renderer {
	r.osTurn = n
	return r
}

// fromWhatsAppTurn reports whether a brief was made from a Marketing Manager turn asked on WhatsApp.
func fromWhatsAppTurn(b *ent.CreativeBrief) bool {
	return str(b.Metadata["source"]) == sourceOSTurn && str(b.Metadata["channel"]) == channelWhatsApp && str(b.Metadata["run_id"]) != ""
}

// notifyOSTurn sends the message in the background, so a job callback never waits on it. Only
// the report that settled the version calls it, so each message goes once.
func (r *Renderer) notifyOSTurn(ctx context.Context, v *ent.CreativeVersion, event string, links []string, errMsg string) {
	if r.osTurn == nil {
		return
	}
	b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx)
	if err != nil || !fromWhatsAppTurn(b) {
		return
	}
	note := OSTurnNote{TenantID: v.TenantID, RunID: str(b.Metadata["run_id"]), VersionID: v.ID.String(), Event: event,
		Links: links, Error: errMsg,
		// The version as it was before this report settled it: a storyboard made again is a new message.
		Key: fmt.Sprintf("wa-osturn-%s-%s-%d", v.ID, event, v.UpdatedAt.UnixNano())}
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), osTurnNotifyTimeout)
		defer cancel()
		r.osTurn.Notify(ctx, note)
	}()
}

// notifyRenderFailed tells a WhatsApp requester that their video's render failed for good. Call
// it only after a conditional update moved the version to failed (one row), so it goes once;
// a failure before the render stage (the script, the storyboard) sends nothing.
func (r *Renderer) notifyRenderFailed(ctx context.Context, id uuid.UUID, msg string) {
	if r.osTurn == nil {
		return
	}
	v, err := r.db.CreativeVersion.Get(ctx, id)
	if err != nil || v.Kind != KindVideoAd || stage(v) != StageRender || v.Status != StatusFailed {
		return
	}
	if msg == "" {
		msg = v.Error
	}
	r.notifyOSTurn(ctx, v, OSTurnRenderFailed, nil, msg)
}

// OSTurnEvent handles the S2S POST /api/v1/internal/creative/os-turn/events: marketflow-ai's
// background make-media or render start failed for a WhatsApp turn, and the requester is told
// through the same path as the storyboard message. Body {tenant_id, run_id, version_id, event,
// error}; only media_failed is accepted. 204 once the message was handed on.
func (h *Handler) OSTurnEvent(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID  string `json:"tenant_id"`
		RunID     string `json:"run_id"`
		VersionID string `json:"version_id"`
		Event     string `json:"event"`
		Error     string `json:"error"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid json", nil)
		return
	}
	tid, err := uuid.Parse(in.TenantID)
	if err != nil || in.RunID == "" || in.Event != OSTurnMediaFailed || strings.TrimSpace(in.Error) == "" {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "tenant_id, run_id, event media_failed and error are required", nil)
		return
	}
	if h.jobs == nil || h.jobs.osTurn == nil {
		httpapi.Error(w, http.StatusServiceUnavailable, "unavailable", "turn messages are not sent from here", nil)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), osTurnNotifyTimeout)
	defer cancel()
	h.jobs.osTurn.Notify(ctx, OSTurnNote{TenantID: tid, RunID: in.RunID, VersionID: in.VersionID, Event: in.Event, Error: in.Error,
		Key: fmt.Sprintf("wa-osturn-%s-%s-%d", in.RunID, in.Event, time.Now().UnixNano())})
	w.WriteHeader(http.StatusNoContent)
}

// stillLinks are a storyboard's still links, in scene order.
func stillLinks(stills []storyboardStill) []string {
	out := make([]string, 0, len(stills))
	for _, st := range stills {
		if st.URL != "" {
			out = append(out, st.URL)
		}
	}
	return out
}

// PlatformWhatsApp sends text from the platform WhatsApp number through notifications-api, as the
// platform tenant, inside the 24-hour window the requester's last message opened: the WhatsApp
// assistant's replies and the messages about the videos made from its turns.
type PlatformWhatsApp struct {
	db     *ent.Client
	notif  *notifications.Client
	slug   string
	source string

	mu sync.Mutex
	id uuid.UUID
}

// NewPlatformWhatsApp builds the sender; slug is the platform tenant and source the metadata
// source notifications-api records.
func NewPlatformWhatsApp(db *ent.Client, notif *notifications.Client, platformSlug, source string) *PlatformWhatsApp {
	return &PlatformWhatsApp{db: db, notif: notif, slug: platformSlug, source: source}
}

// Send sends one text; idempotencyKey makes a retried send go once. Without a notifications
// client nothing is sent.
func (p *PlatformWhatsApp) Send(ctx context.Context, to, text, idempotencyKey string) error {
	if p == nil || p.notif == nil {
		return nil
	}
	id, err := p.platformID(ctx)
	if err != nil {
		return err
	}
	return p.notif.Send(ctx, id.String(), notifications.Message{
		Channel:        "whatsapp",
		Recipient:      "+" + strings.TrimPrefix(to, "+"),
		Body:           text,
		Metadata:       map[string]string{"source": p.source, "kind": "whatsapp_assistant"},
		IdempotencyKey: idempotencyKey,
	})
}

// platformID looks the platform tenant up once.
func (p *PlatformWhatsApp) platformID(ctx context.Context) (uuid.UUID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.id != uuid.Nil {
		return p.id, nil
	}
	t, err := p.db.Tenant.Query().Where(tenant.SlugEQ(p.slug)).Only(ctx)
	if err != nil {
		return uuid.Nil, fmt.Errorf("platform tenant %q: %w", p.slug, err)
	}
	p.id = t.ID
	return p.id, nil
}

// WhatsAppTurnNotifier asks marketflow-ai for the message about a turn's video (it holds the
// requester's number and the approval codes) and sends it from the platform number.
type WhatsAppTurnNotifier struct {
	aiURL  string
	aiKey  string
	hc     *http.Client
	send   *PlatformWhatsApp
	logger *zap.Logger
}

// NewWhatsAppTurnNotifier builds the notifier; aiURL is marketflow-ai and aiKey its internal key.
func NewWhatsAppTurnNotifier(aiURL, aiKey string, send *PlatformWhatsApp, logger *zap.Logger) *WhatsAppTurnNotifier {
	return &WhatsAppTurnNotifier{aiURL: strings.TrimSuffix(aiURL, "/"), aiKey: aiKey, hc: &http.Client{Timeout: 20 * time.Second},
		send: send, logger: logger.Named("creative.os-turn")}
}

// Notify implements OSTurnNotifier. Failures are logged: the version is already settled and the
// owner can always see it in the Studio.
func (n *WhatsAppTurnNotifier) Notify(ctx context.Context, note OSTurnNote) {
	if n == nil || n.aiURL == "" || n.send == nil {
		return
	}
	body, _ := json.Marshal(map[string]any{"tenant_id": note.TenantID.String(), "run_id": note.RunID,
		"version_id": note.VersionID, "event": note.Event, "links": note.Links, "error": excerpt(note.Error, 1500)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.aiURL+"/api/v1/internal/os/whatsapp-notify", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", n.aiKey)
	resp, err := n.hc.Do(req)
	if err != nil {
		n.logger.Warn("turn video message not written", zap.String("run_id", note.RunID), zap.Error(err))
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		n.logger.Warn("turn video message refused", zap.String("run_id", note.RunID), zap.Int("status", resp.StatusCode))
		return
	}
	var msg struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if json.Unmarshal(data, &msg) != nil || msg.To == "" || strings.TrimSpace(msg.Text) == "" {
		return
	}
	if err := n.send.Send(ctx, msg.To, msg.Text, note.Key); err != nil {
		n.logger.Warn("turn video message not sent", zap.String("run_id", note.RunID), zap.Error(err))
	}
}
