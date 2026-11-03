package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/connectedaccount"
	"github.com/bengobox/marketflow-api/internal/ent/contact"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Social inbox (Sprint 22). Comments and DMs on a tenant's connected Facebook Page and Instagram
// account become Activities (activity_type comment or dm) on a Contact, with the platform
// references in metadata. Payload shapes: docs/integrations/social-inbox-apis.md. The webhook
// only stores; the worker tags intent and replies, so Meta always gets its answer fast.

// InboxItem is one inbound comment or DM, whatever the platform.
type InboxItem struct {
	Platform   string // facebook | instagram
	AccountID  string // Page id or Instagram account id that received it
	Kind       string // comment | dm
	ExternalID string // comment id or message mid: the dedupe key
	PostID     string
	ParentID   string
	AdID       string
	SenderID   string // page-scoped or Instagram-scoped user id
	SenderName string
	Text       string
	At         time.Time
}

const maxInboxText = 5000

type inboxWebhook struct {
	Object string       `json:"object"`
	Entry  []inboxEntry `json:"entry"`
}

type inboxEntry struct {
	ID        string           `json:"id"`
	Time      int64            `json:"time"`
	Changes   []inboxChange    `json:"changes"`
	Messaging []inboxMessaging `json:"messaging"`
	Field     string           `json:"field"` // Business Login for Instagram puts field and value here
	Value     json.RawMessage  `json:"value"`
}

type inboxChange struct {
	Field string          `json:"field"`
	Value json.RawMessage `json:"value"`
}

type inboxMessaging struct {
	Sender struct {
		ID string `json:"id"`
	} `json:"sender"`
	Recipient struct {
		ID string `json:"id"`
	} `json:"recipient"`
	Timestamp int64 `json:"timestamp"`
	Message   *struct {
		Mid       string `json:"mid"`
		Text      string `json:"text"`
		IsEcho    bool   `json:"is_echo"`
		IsDeleted bool   `json:"is_deleted"`
	} `json:"message"`
}

type pageFeedValue struct {
	Item        string `json:"item"`
	Verb        string `json:"verb"`
	CommentID   string `json:"comment_id"`
	PostID      string `json:"post_id"`
	ParentID    string `json:"parent_id"`
	Message     string `json:"message"`
	CreatedTime int64  `json:"created_time"`
	From        struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"from"`
}

type igCommentValue struct {
	ID        string `json:"id"`
	CommentID string `json:"comment_id"`
	ParentID  string `json:"parent_id"`
	Text      string `json:"text"`
	From      struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"from"`
	Media struct {
		ID   string `json:"id"`
		AdID string `json:"ad_id"`
	} `json:"media"`
}

// ParseInbox pulls the comments and DMs out of a Meta webhook body. It skips the account's own
// comments and echoes of messages it sent, so MarketFlow never answers itself, and anything with
// no text.
func ParseInbox(body []byte) []InboxItem {
	var p inboxWebhook
	if json.Unmarshal(body, &p) != nil {
		return nil
	}
	platform := map[string]string{"page": "facebook", "instagram": "instagram"}[p.Object]
	if platform == "" {
		return nil
	}
	var out []InboxItem
	for _, e := range p.Entry {
		changes := e.Changes
		if e.Field != "" && len(e.Value) > 0 {
			changes = append(changes, inboxChange{Field: e.Field, Value: e.Value})
		}
		for _, c := range changes {
			switch {
			case platform == "facebook" && c.Field == "feed":
				var v pageFeedValue
				if json.Unmarshal(c.Value, &v) != nil || v.Item != "comment" || v.Verb != "add" || v.From.ID == e.ID {
					continue
				}
				out = append(out, InboxItem{Platform: platform, AccountID: e.ID, Kind: "comment", ExternalID: v.CommentID,
					PostID: v.PostID, ParentID: v.ParentID, SenderID: v.From.ID, SenderName: v.From.Name,
					Text: v.Message, At: unixOr(v.CreatedTime, e.Time)})
			case platform == "instagram" && c.Field == "comments":
				var v igCommentValue
				if json.Unmarshal(c.Value, &v) != nil || v.From.ID == e.ID {
					continue
				}
				id := v.CommentID
				if id == "" {
					id = v.ID
				}
				out = append(out, InboxItem{Platform: platform, AccountID: e.ID, Kind: "comment", ExternalID: id,
					PostID: v.Media.ID, ParentID: v.ParentID, AdID: v.Media.AdID, SenderID: v.From.ID,
					SenderName: v.From.Username, Text: v.Text, At: unixOr(0, e.Time)})
			}
		}
		for _, m := range e.Messaging {
			if m.Message == nil || m.Message.IsEcho || m.Message.IsDeleted || m.Sender.ID == e.ID {
				continue
			}
			out = append(out, InboxItem{Platform: platform, AccountID: e.ID, Kind: "dm", ExternalID: m.Message.Mid,
				SenderID: m.Sender.ID, Text: m.Message.Text, At: unixOr(m.Timestamp, e.Time)})
		}
	}
	kept := out[:0]
	for _, it := range out {
		if it.ExternalID != "" && it.SenderID != "" && strings.TrimSpace(it.Text) != "" {
			kept = append(kept, it)
		}
	}
	return kept
}

// unixOr reads a Meta timestamp, which is seconds on feed events and milliseconds on messaging
// and entry times, falling back to the entry time and then now.
func unixOr(ts, entry int64) time.Time {
	for _, v := range []int64{ts, entry} {
		switch {
		case v > 1e12:
			return time.UnixMilli(v).UTC()
		case v > 0:
			return time.Unix(v, 0).UTC()
		}
	}
	return time.Now().UTC()
}

// InboxStore writes inbox items.
type InboxStore struct {
	DB     *ent.Client
	Logger *zap.Logger
}

var inboxAccountTypes = map[string]string{"facebook": "facebook_page", "instagram": "instagram_business"}

// Store saves the items and returns how many were new. A redelivered event is skipped by its
// external id (indexed external_ref), so one comment is one Activity.
func (s *InboxStore) Store(ctx context.Context, items []InboxItem) int {
	saved := 0
	for _, it := range items {
		if err := s.storeOne(ctx, it); err != nil {
			s.Logger.Warn("inbox item not stored", zap.String("platform", it.Platform), zap.String("kind", it.Kind), zap.Error(err))
			continue
		}
		saved++
	}
	return saved
}

func (s *InboxStore) storeOne(ctx context.Context, it InboxItem) error {
	acc, err := s.DB.ConnectedAccount.Query().
		Where(connectedaccount.ExternalID(it.AccountID), connectedaccount.AccountType(inboxAccountTypes[it.Platform]), connectedaccount.Status("active")).
		Order(ent.Desc(connectedaccount.FieldUpdatedAt)).
		First(ctx)
	if err != nil {
		return fmt.Errorf("no tenant for %s account %s", it.Platform, it.AccountID)
	}
	tid := acc.TenantID
	dup, err := s.DB.Activity.Query().
		Where(activity.ExternalRef(it.ExternalID), activity.TenantID(tid), activity.ActivityTypeIn("comment", "dm")).
		Exist(ctx)
	if err != nil {
		return err
	}
	if dup {
		return nil
	}
	contactID, err := s.contactFor(ctx, tid, it)
	if err != nil {
		return err
	}
	text := it.Text
	if utf8.RuneCountInString(text) > maxInboxText {
		text = string([]rune(text)[:maxInboxText])
	}
	meta := map[string]any{
		"platform": it.Platform, "account_id": it.AccountID, "connected_account_id": acc.ID.String(),
		"sender_id": it.SenderID, "sender_name": it.SenderName, "intent_status": "pending",
	}
	for k, v := range map[string]string{"post_id": it.PostID, "parent_id": it.ParentID, "ad_id": it.AdID} {
		if v != "" {
			meta[k] = v
		}
	}
	// A DM thread is the person; a comment thread is the post it is on.
	if it.Kind == "dm" {
		meta["thread_id"] = it.Platform + ":" + it.SenderID
	} else if it.PostID != "" {
		meta["thread_id"] = it.Platform + ":" + it.PostID
	}
	_, err = s.DB.Activity.Create().
		SetTenantID(tid).
		SetEntityType("contact").
		SetEntityID(contactID).
		SetActivityType(it.Kind).
		SetDirection("inbound").
		SetBody(text).
		SetSource(it.Platform).
		SetExternalRef(it.ExternalID).
		SetPerformedAt(it.At).
		SetMetadata(meta).
		Save(ctx)
	return err
}

// contactFor finds the commenter by platform id (GIN index on contacts.metadata) or makes a
// lightweight Contact with every consent off. Platform ids are scoped to one Page or account, so
// the key includes the platform.
func (s *InboxStore) contactFor(ctx context.Context, tid uuid.UUID, it InboxItem) (uuid.UUID, error) {
	key := socialKey(it.Platform, it.SenderID)
	probe, _ := json.Marshal(map[string]any{"social_ids": []string{key}})
	c, err := s.DB.Contact.Query().
		Where(contact.TenantID(tid), func(sel *entsql.Selector) {
			sel.Where(entsql.P(func(b *entsql.Builder) {
				b.Ident(sel.C(contact.FieldMetadata)).WriteString(" @> ").Arg(string(probe)).WriteString("::jsonb")
			}))
		}).
		First(ctx)
	if err == nil {
		return c.ID, nil
	}
	if !ent.IsNotFound(err) {
		return uuid.Nil, err
	}
	create := s.DB.Contact.Create().
		SetTenantID(tid).
		SetContactType("prospect").
		SetSubscribedEmail(false).
		SetSubscribedSms(false).
		SetMetadata(map[string]any{"social_ids": []string{key}, "source": it.Platform + "_" + it.Kind, "social_name": it.SenderName})
	if first := strings.TrimSpace(strings.TrimPrefix(it.SenderName, "@")); first != "" {
		create.SetFirstName(first)
	}
	nc, err := create.Save(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return nc.ID, nil
}

func socialKey(platform, id string) string {
	prefix := map[string]string{"facebook": "fb", "instagram": "ig"}[platform]
	return prefix + ":" + id
}
