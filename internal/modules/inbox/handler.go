// Package inbox serves the social inbox (Sprint 22): the tenant's Facebook and Instagram
// comments and DMs, stored as Activities by the Meta webhook (webhooks/inbox.go) and tagged by
// the worker's InboxTagger. Owners filter by intent, reply, mark handled and promote a buyer to
// a Lead. API contracts: docs/integrations/social-inbox-apis.md.
package inbox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/Bengo-Hub/pagination"
	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/leads"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Handler serves /api/v1/inbox.
type Handler struct {
	db     *ent.Client
	keys   *encryption.KeyProvider
	leads  *leads.Service
	graph  *Graph
	logger *zap.Logger
	// CanReply gates replies (they speak publicly for the business); nil allows every member.
	CanReply func(ctx context.Context, tenantID uuid.UUID) bool
	// CanModerate gates hiding and deleting comments on the business's own posts; nil allows
	// every member.
	CanModerate func(ctx context.Context, tenantID uuid.UUID) bool
}

func NewHandler(db *ent.Client, keys *encryption.KeyProvider, leadSvc *leads.Service, graph *Graph, logger *zap.Logger) *Handler {
	return &Handler{db: db, keys: keys, leads: leadSvc, graph: graph, logger: logger.Named("inbox")}
}

// Item is one inbox row.
type Item struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"` // comment | dm
	Platform    string    `json:"platform"`
	ContactID   string    `json:"contact_id"`
	SenderName  string    `json:"sender_name,omitempty"`
	Text        string    `json:"text"`
	At          time.Time `json:"at"`
	Intent      string    `json:"intent,omitempty"`
	Status      string    `json:"intent_status"`
	PostID      string    `json:"post_id,omitempty"`
	AdID        string    `json:"ad_id,omitempty"`
	HandledAt   string    `json:"handled_at,omitempty"`
	RepliedAt   string    `json:"replied_at,omitempty"`
	LeadID      string    `json:"lead_id,omitempty"`
	CanReply    bool      `json:"can_reply"`
	ReplyWindow string    `json:"reply_window,omitempty"`
	// Hidden: the comment is hidden on the post (only the commenter and their friends see it).
	Hidden bool `json:"hidden"`
	// DeletedAt: the comment was deleted from the post; the item stays, marked, like handled.
	DeletedAt string `json:"deleted_at,omitempty"`
	// CanModerate: a Facebook or Instagram comment, so it can be hidden or deleted.
	CanModerate bool `json:"can_moderate"`
}

// List handles GET /api/v1/inbox?kind=&intent=&open=true&cursor=&limit=: newest first, keyset
// pagination on (performed_at, id) over the (tenant_id, activity_type, performed_at) index.
// @Summary Social inbox
// @Tags Inbox
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param kind query string false "comment or dm"
// @Param intent query string false "an intent tag, or buyer for every buyer_* tag"
// @Param open query bool false "only items not yet handled"
// @Param cursor query string false "next_cursor of the previous page"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /inbox [get]
func (h *Handler) List(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	kinds := []string{"comment", "dm"}
	if k := q.Get("kind"); k == "comment" || k == "dm" {
		kinds = []string{k}
	}
	cp := pagination.ParseCursorParams(r)
	query := h.db.Activity.Query().Where(activity.TenantID(tid), activity.ActivityTypeIn(kinds...), activity.DirectionEQ("inbound"))
	if intent := q.Get("intent"); intent != "" {
		query = query.Where(func(s *entsql.Selector) {
			col := s.C(activity.FieldMetadata) + "->>'intent'"
			if intent == "buyer" {
				s.Where(entsql.ExprP(col + " LIKE 'buyer\\_%'"))
				return
			}
			s.Where(entsql.P(func(b *entsql.Builder) { b.WriteString(col + " = ").Arg(intent) }))
		})
	}
	if q.Get("open") == "true" {
		query = query.Where(func(s *entsql.Selector) {
			s.Where(entsql.ExprP("NOT jsonb_exists(COALESCE(" + s.C(activity.FieldMetadata) + ", '{}'::jsonb), 'handled_at')"))
		})
	}
	if cp.Cursor != "" {
		cid, cat, err := pagination.DecodeCursor(cp.Cursor)
		if err != nil {
			httpapi.Error(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor", nil)
			return
		}
		query = query.Where(activity.Or(
			activity.PerformedAtLT(cat),
			activity.And(activity.PerformedAtEQ(cat), activity.IDLT(cid)),
		))
	}
	rows, err := query.Order(ent.Desc(activity.FieldPerformedAt), ent.Desc(activity.FieldID)).Limit(cp.Limit + 1).All(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	resp := pagination.CursorResponse[Item]{Data: make([]Item, 0, len(rows))}
	if len(rows) > cp.Limit {
		rows = rows[:cp.Limit]
		resp.HasMore = true
		last := rows[len(rows)-1]
		resp.NextCursor = pagination.EncodeCursor(last.ID, last.PerformedAt)
	}
	now := time.Now()
	for _, a := range rows {
		resp.Data = append(resp.Data, toItem(a, now))
	}
	httpapi.JSON(w, http.StatusOK, resp)
}

func toItem(a *ent.Activity, now time.Time) Item {
	str := func(k string) string { v, _ := a.Metadata[k].(string); return v }
	it := Item{ID: a.ID.String(), Kind: a.ActivityType, Platform: str("platform"), ContactID: a.EntityID.String(),
		SenderName: str("sender_name"), At: a.PerformedAt, Intent: str("intent"), Status: str("intent_status"),
		PostID: str("post_id"), AdID: str("ad_id"), HandledAt: str("handled_at"), RepliedAt: str("replied_at"), LeadID: str("lead_id")}
	if a.Body != nil {
		it.Text = *a.Body
	}
	it.CanReply, it.ReplyWindow = replyWindow(a.ActivityType, a.PerformedAt, now)
	it.Hidden, _ = a.Metadata["hidden"].(bool)
	it.DeletedAt = str("deleted_at")
	if it.DeletedAt != "" {
		it.CanReply, it.ReplyWindow = false, "the comment was deleted"
	}
	_, _, notComment := moderationTarget(a)
	it.CanModerate = notComment == nil && it.DeletedAt == ""
	return it
}

// replyWindow applies the platforms' rules: a DM may be answered within 24 hours of the person's
// message; a comment may get a public reply any time (the private reply's 7 days is separate).
func replyWindow(kind string, at, now time.Time) (bool, string) {
	if kind == "dm" {
		left := at.Add(24 * time.Hour).Sub(now)
		if left <= 0 {
			return false, "the 24 hour reply window has closed"
		}
		return true, ""
	}
	return true, ""
}

// MaxReply is a safe length for every platform's reply.
const MaxReply = 1000

// Reply handles POST /api/v1/inbox/{id}/reply {text}: a DM answer inside the 24 hour window, or a
// public reply under a comment. The answer is stored as an outbound Activity on the same Contact.
// @Summary Reply to a comment or DM
// @Tags Inbox
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item (Activity) UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /inbox/{id}/reply [post]
func (h *Handler) Reply(w http.ResponseWriter, r *http.Request) {
	tid, a, ok := h.load(w, r)
	if !ok {
		return
	}
	if h.CanReply != nil && !h.CanReply(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "only the account owner or an admin can reply for the business", nil)
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Text) == "" ||
		utf8.RuneCountInString(in.Text) > MaxReply {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", "text is required (at most 1,000 characters)", nil)
		return
	}
	if str(a.Metadata, "deleted_at") != "" {
		httpapi.Error(w, http.StatusConflict, "comment_deleted", "the comment was deleted", nil)
		return
	}
	if can, why := replyWindow(a.ActivityType, a.PerformedAt, time.Now()); !can {
		httpapi.Error(w, http.StatusConflict, "window_closed", why, nil)
		return
	}
	if err := h.send(r.Context(), tid, a, strings.TrimSpace(in.Text), userOf(r)); err != nil {
		if errors.Is(err, ErrNoAccount) {
			httpapi.Error(w, http.StatusConflict, "not_connected", "this page or account is no longer connected; reconnect it to reply", nil)
			return
		}
		h.logger.Warn("inbox reply failed", zap.String("tenant", tid.String()), zap.Error(err))
		httpapi.Error(w, http.StatusBadGateway, "reply_failed", err.Error(), nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, toItem(h.db.Activity.GetX(r.Context(), a.ID), time.Now()))
}

// ErrNoAccount means the receiving page or account is no longer connected.
var ErrNoAccount = errors.New("account not connected")

// send posts the reply with the connected account's token and records it. Shared by the owner's
// reply and the worker's auto reply.
func (h *Handler) send(ctx context.Context, tid uuid.UUID, a *ent.Activity, text string, by *uuid.UUID) error {
	return Send(ctx, h.db, h.keys, h.graph, tid, a, text, by)
}

// Send is the reply path, exported for the auto reply.
func Send(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, g *Graph, tid uuid.UUID, a *ent.Activity, text string, by *uuid.UUID) error {
	token, err := accountToken(ctx, db, keys, tid, a)
	if err != nil {
		return err
	}
	platform := str(a.Metadata, "platform")
	var externalID string
	if a.ActivityType == "dm" {
		externalID, err = g.SendDM(ctx, str(a.Metadata, "account_id"), token, str(a.Metadata, "sender_id"), text)
	} else {
		externalID, err = g.ReplyToComment(ctx, platform, derefOr(a.ExternalRef), token, text)
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	out := db.Activity.Create().
		SetTenantID(tid).SetEntityType(a.EntityType).SetEntityID(a.EntityID).
		SetActivityType(a.ActivityType).SetDirection("outbound").SetBody(text).SetSource(platform).
		SetExternalRef(externalID).SetPerformedAt(now).
		SetMetadata(map[string]any{"platform": platform, "in_reply_to": a.ID.String(), "intent_status": "outbound",
			"thread_id": str(a.Metadata, "thread_id"), "auto": by == nil})
	if by != nil {
		out.SetPerformedBy(*by)
	}
	if _, err := out.Save(ctx); err != nil {
		return err
	}
	return MergeMetadata(ctx, db, a.ID, map[string]any{"replied_at": now.Format(time.RFC3339)})
}

// accountToken is the token of the connected Page or Instagram account that received the item,
// the one replies, hides and deletes act with. ErrNoAccount when it is gone or unreadable.
func accountToken(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, tid uuid.UUID, a *ent.Activity) (string, error) {
	accID, _ := uuid.Parse(str(a.Metadata, "connected_account_id"))
	acc, err := db.ConnectedAccount.Query().
		Where(connectedaccount.IDEQ(accID), connectedaccount.TenantIDEQ(tid), connectedaccount.StatusEQ("active")).
		Only(ctx)
	if err != nil || acc.TokenCiphertext == "" || keys == nil {
		return "", ErrNoAccount
	}
	token, err := keys.Decrypt(ctx, acc.TokenCiphertext)
	if err != nil {
		return "", ErrNoAccount
	}
	return token, nil
}

// ErrNotComment: only Facebook Page and Instagram comments can be hidden or deleted.
var ErrNotComment = errors.New("only comments can be hidden or deleted")

// moderationTarget is the platform and comment id to hide or delete, or ErrNotComment for a DM, a
// WhatsApp item or a comment without its id.
func moderationTarget(a *ent.Activity) (platform, commentID string, err error) {
	platform = str(a.Metadata, "platform")
	if a.ActivityType != "comment" || (platform != "facebook" && platform != "instagram") || derefOr(a.ExternalRef) == "" {
		return "", "", ErrNotComment
	}
	return platform, *a.ExternalRef, nil
}

// moderate loads the item, checks the role and that it is a live comment, and returns its target
// and the account token. It answers the request itself when it returns false.
func (h *Handler) moderate(w http.ResponseWriter, r *http.Request) (uuid.UUID, *ent.Activity, string, string, string, bool) {
	tid, a, ok := h.load(w, r)
	if !ok {
		return uuid.Nil, nil, "", "", "", false
	}
	if h.CanModerate != nil && !h.CanModerate(r.Context(), tid) {
		httpapi.Error(w, http.StatusForbidden, "forbidden", "your MarketFlow role cannot hide or delete comments", nil)
		return uuid.Nil, nil, "", "", "", false
	}
	platform, commentID, err := moderationTarget(a)
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "not_a_comment", ErrNotComment.Error(), nil)
		return uuid.Nil, nil, "", "", "", false
	}
	if str(a.Metadata, "deleted_at") != "" {
		httpapi.Error(w, http.StatusConflict, "already_deleted", "this comment was already deleted", nil)
		return uuid.Nil, nil, "", "", "", false
	}
	token, err := accountToken(r.Context(), h.db, h.keys, tid, a)
	if err != nil {
		httpapi.Error(w, http.StatusConflict, "not_connected", "this page or account is no longer connected; reconnect it to moderate comments", nil)
		return uuid.Nil, nil, "", "", "", false
	}
	return tid, a, platform, commentID, token, true
}

// metaFailed answers a Graph refusal as 502 with Meta's own message.
func (h *Handler) metaFailed(w http.ResponseWriter, tid uuid.UUID, what string, err error) {
	h.logger.Warn("inbox "+what+" failed", zap.String("tenant", tid.String()), zap.Error(err))
	msg := err.Error()
	var ge *GraphError
	if errors.As(err, &ge) && ge.Message != "" {
		msg = ge.Message
	}
	httpapi.Error(w, http.StatusBadGateway, what+"_failed", msg, nil)
}

// Hide handles POST /api/v1/inbox/{id}/hide {"hidden": true|false}: hides or unhides a Facebook
// Page or Instagram comment on the business's own post, with the receiving account's token.
// @Summary Hide or unhide a comment
// @Tags Inbox
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item (Activity) UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /inbox/{id}/hide [post]
func (h *Handler) Hide(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Hidden *bool `json:"hidden"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in); err != nil || in.Hidden == nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_request", `send {"hidden": true} or {"hidden": false}`, nil)
		return
	}
	tid, a, platform, commentID, token, ok := h.moderate(w, r)
	if !ok {
		return
	}
	if err := h.graph.HideComment(r.Context(), platform, commentID, token, *in.Hidden); err != nil {
		h.metaFailed(w, tid, "hide", err)
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	patch := map[string]any{"hidden": *in.Hidden}
	if *in.Hidden {
		patch["hidden_at"] = now
	} else {
		patch["unhidden_at"] = now
	}
	if u := userOf(r); u != nil {
		patch["hidden_by"] = u.String()
	}
	if err := MergeMetadata(r.Context(), h.db, a.ID, patch); err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toItem(h.db.Activity.GetX(r.Context(), a.ID), time.Now()))
}

// Delete handles DELETE /api/v1/inbox/{id}: deletes a Facebook Page or Instagram comment from the
// business's own post. The inbox item stays, marked deleted, so the history is kept.
// @Summary Delete a comment
// @Tags Inbox
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item (Activity) UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /inbox/{id} [delete]
func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	tid, a, platform, commentID, token, ok := h.moderate(w, r)
	if !ok {
		return
	}
	if err := h.graph.DeleteComment(r.Context(), platform, commentID, token); err != nil {
		h.metaFailed(w, tid, "delete", err)
		return
	}
	patch := map[string]any{"deleted_at": time.Now().UTC().Format(time.RFC3339)}
	if u := userOf(r); u != nil {
		patch["deleted_by"] = u.String()
	}
	if err := MergeMetadata(r.Context(), h.db, a.ID, patch); err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toItem(h.db.Activity.GetX(r.Context(), a.ID), time.Now()))
}

// Handled handles POST /api/v1/inbox/{id}/handled.
// @Summary Mark an inbox item handled
// @Tags Inbox
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /inbox/{id}/handled [post]
func (h *Handler) Handled(w http.ResponseWriter, r *http.Request) {
	_, a, ok := h.load(w, r)
	if !ok {
		return
	}
	patch := map[string]any{"handled_at": time.Now().UTC().Format(time.RFC3339)}
	if u := userOf(r); u != nil {
		patch["handled_by"] = u.String()
	}
	if err := MergeMetadata(r.Context(), h.db, a.ID, patch); err != nil {
		h.fail(w, err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toItem(h.db.Activity.GetX(r.Context(), a.ID), time.Now()))
}

// Promote handles POST /api/v1/inbox/{id}/promote: the person becomes a Lead with source comment
// or dm and the post or ad they came from, then lead.created runs the follow-up (Sprint 23).
// Promoting twice returns the same lead.
// @Summary Promote an inbox item to a lead
// @Tags Inbox
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item UUID"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /inbox/{id}/promote [post]
func (h *Handler) Promote(w http.ResponseWriter, r *http.Request) {
	tid, a, ok := h.load(w, r)
	if !ok {
		return
	}
	if id := str(a.Metadata, "lead_id"); id != "" {
		httpapi.JSON(w, http.StatusOK, map[string]any{"lead_id": id})
		return
	}
	name := strings.TrimPrefix(str(a.Metadata, "sender_name"), "@")
	in := leads.CreateInput{
		TenantID: tid, Source: a.ActivityType, LeadType: "customer",
		Metadata: map[string]any{"inbox_activity_id": a.ID.String(), "contact_id": a.EntityID.String(),
			"platform": str(a.Metadata, "platform"), "post_id": str(a.Metadata, "post_id"), "ad_id": str(a.Metadata, "ad_id"),
			"intent": str(a.Metadata, "intent")},
	}
	if name != "" {
		in.FirstName = &name
	}
	lead, err := h.leads.Create(r.Context(), in)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := MergeMetadata(r.Context(), h.db, a.ID, map[string]any{"lead_id": lead.ID.String()}); err != nil {
		h.logger.Warn("lead link not saved on the inbox item", zap.Error(err))
	}
	httpapi.JSON(w, http.StatusCreated, map[string]any{"lead_id": lead.ID.String()})
}

func (h *Handler) load(w http.ResponseWriter, r *http.Request) (uuid.UUID, *ent.Activity, bool) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return uuid.Nil, nil, false
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return uuid.Nil, nil, false
	}
	a, err := h.db.Activity.Query().
		Where(activity.IDEQ(id), activity.TenantID(tid), activity.ActivityTypeIn("comment", "dm"), activity.DirectionEQ("inbound")).
		Only(r.Context())
	if err != nil {
		h.fail(w, err)
		return uuid.Nil, nil, false
	}
	return tid, a, true
}

// MergeMetadata writes patch into an Activity's metadata in one statement (jsonb ||).
func MergeMetadata(ctx context.Context, db *ent.Client, id uuid.UUID, patch map[string]any) error {
	b, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	value := entsql.ExprFunc(func(bd *entsql.Builder) {
		bd.WriteString("(COALESCE(" + activity.FieldMetadata + ", '{}'::jsonb) || ")
		bd.Arg(string(b))
		bd.WriteString("::jsonb)")
	})
	_, err = db.Activity.Update().Where(activity.IDEQ(id)).
		Modify(func(u *entsql.UpdateBuilder) { u.Set(activity.FieldMetadata, value) }).Save(ctx)
	return err
}

func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }

func derefOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func tenantOf(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.Header.Get("X-Tenant-ID"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID required", nil)
		return uuid.Nil, false
	}
	return id, true
}

func userOf(r *http.Request) *uuid.UUID {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		if id, err := uuid.Parse(c.Subject); err == nil {
			return &id
		}
	}
	return nil
}

func (h *Handler) fail(w http.ResponseWriter, err error) {
	if ent.IsNotFound(err) {
		httpapi.Error(w, http.StatusNotFound, "not_found", "not found", nil)
		return
	}
	h.logger.Error("inbox request failed", zap.Error(err))
	httpapi.Error(w, http.StatusInternalServerError, "internal_error", "something went wrong", nil)
}
