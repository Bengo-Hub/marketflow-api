package inbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bengobox/marketflow-api/internal/encryption"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/google/uuid"
)

// A private reply answers a Facebook Page or Instagram comment with a message in the commenter's
// inbox (docs/integrations/social-inbox-apis.md):
//   - Facebook: POST /{page-id}/messages {recipient:{comment_id}, message:{text}}, pages_messaging;
//   - Instagram: POST /{ig-user-id}/messages with the same body, instagram_manage_comments and
//     instagram_manage_messages.
//
// The platforms allow one private reply per comment, within 7 days of it. More messages follow
// only once the person answers, and their answer arrives as an ordinary DM.

// PrivateReplyWindow is how long after a comment its private reply may be sent.
const PrivateReplyWindow = 7 * 24 * time.Hour

// PrivateReply sends the one private message a comment allows; it returns the message id.
func (g *Graph) PrivateReply(ctx context.Context, accountID, token, commentID, text string) (string, error) {
	if accountID == "" || commentID == "" {
		return "", fmt.Errorf("private reply: no account or comment id")
	}
	body, _ := json.Marshal(map[string]any{
		"recipient": map[string]string{"comment_id": commentID},
		"message":   map[string]string{"text": text},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.Base+"/"+url.PathEscape(accountID)+"/messages?access_token="+url.QueryEscape(token), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		MessageID string `json:"message_id"`
	}
	if err := g.do(req, &out); err != nil {
		return "", fmt.Errorf("private reply: %w", err)
	}
	return out.MessageID, nil
}

// privateReplyWindow says whether a comment can still get its private reply, and why not.
func privateReplyWindow(a *ent.Activity, now time.Time) (bool, string) {
	if _, _, err := moderationTarget(a); err != nil {
		return false, ""
	}
	switch {
	case str(a.Metadata, "deleted_at") != "":
		return false, "the comment was deleted"
	case str(a.Metadata, "private_replied_at") != "":
		return false, "a private message was already sent for this comment"
	case now.Sub(a.PerformedAt) > PrivateReplyWindow:
		return false, "private messages can be sent only within 7 days of the comment"
	}
	return true, ""
}

// ErrPrivateReplyClosed: the comment cannot get a private reply (see privateReplyWindow).
var ErrPrivateReplyClosed = errors.New("private reply not available")

// SendPrivate sends the private reply with the receiving account's token and records it as an
// outbound DM on the same Contact. by is nil for the automatic answer.
func SendPrivate(ctx context.Context, db *ent.Client, keys *encryption.KeyProvider, g *Graph, tid uuid.UUID, a *ent.Activity, text string, by *uuid.UUID) error {
	if ok, why := privateReplyWindow(a, time.Now()); !ok {
		return fmt.Errorf("%w: %s", ErrPrivateReplyClosed, why)
	}
	token, err := accountToken(ctx, db, keys, tid, a)
	if err != nil {
		return err
	}
	platform := str(a.Metadata, "platform")
	msgID, err := g.PrivateReply(ctx, str(a.Metadata, "account_id"), token, derefOr(a.ExternalRef), text)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	out := db.Activity.Create().
		SetTenantID(tid).SetEntityType(a.EntityType).SetEntityID(a.EntityID).
		SetActivityType("dm").SetDirection("outbound").SetBody(text).SetSource(platform).
		SetExternalRef(msgID).SetPerformedAt(now).
		SetMetadata(map[string]any{"platform": platform, "in_reply_to": a.ID.String(), "intent_status": "outbound",
			"private_reply": true, "thread_id": platform + ":" + str(a.Metadata, "sender_id"), "auto": by == nil})
	if by != nil {
		out.SetPerformedBy(*by)
	}
	if _, err := out.Save(ctx); err != nil {
		return err
	}
	return MergeMetadata(ctx, db, a.ID, map[string]any{"private_replied_at": now.Format(time.RFC3339), "replied_at": now.Format(time.RFC3339)})
}

// PrivateReply handles POST /api/v1/inbox/{id}/private-reply {text}: the one private message a
// Facebook Page or Instagram comment allows, within 7 days of it.
// @Summary Answer a comment with a private message
// @Tags Inbox
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Inbox item (Activity) UUID"
// @Success 200 {object} map[string]interface{}
// @Failure 409 {object} httpapi.ErrorResponse
// @Failure 502 {object} httpapi.ErrorResponse
// @Security bearerAuth
// @Router /inbox/{id}/private-reply [post]
func (h *Handler) PrivateReply(w http.ResponseWriter, r *http.Request) {
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
	if can, why := privateReplyWindow(a, time.Now()); !can {
		if why == "" {
			why = "only Facebook Page and Instagram comments can be answered privately"
		}
		httpapi.Error(w, http.StatusConflict, "private_reply_closed", why, nil)
		return
	}
	err := SendPrivate(r.Context(), h.db, h.keys, h.graph, tid, a, strings.TrimSpace(in.Text), userOf(r))
	switch {
	case errors.Is(err, ErrNoAccount):
		httpapi.Error(w, http.StatusConflict, "not_connected", "this page or account is no longer connected; reconnect it to reply", nil)
		return
	case err != nil:
		h.metaFailed(w, tid, "private_reply", err)
		return
	}
	httpapi.JSON(w, http.StatusOK, toItem(h.db.Activity.GetX(r.Context(), a.ID), time.Now()))
}

// applyPrivateReply fills the item's private reply fields.
func (it *Item) applyPrivateReply(a *ent.Activity, now time.Time) {
	it.PrivateRepliedAt = str(a.Metadata, "private_replied_at")
	it.CanPrivateReply, it.PrivateReplyWindow = privateReplyWindow(a, now)
}
