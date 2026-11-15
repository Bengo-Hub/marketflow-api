package marketingos

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// Social inbox settings beyond the switch and the message (Sprint 22). They live in
// Tenant.metadata like the other switches; an absent key means the default.
const (
	// InboxIntentBarKey is the confidence a tag needs; below it an item waits untagged for the owner.
	InboxIntentBarKey = "inbox_intent_threshold"
	// InboxReplyBarKey is the confidence a buyer tag needs before the automatic answer goes out.
	InboxReplyBarKey = "inbox_auto_reply_threshold"
	// InboxCommentPromptKey is the short public line under a comment whose message went privately.
	InboxCommentPromptKey = "inbox_comment_prompt"

	DefaultInboxIntentBar = 0.55
	DefaultInboxReplyBar  = 0.7
	// DefaultInboxCommentPrompt is used when the tenant saved none.
	DefaultInboxCommentPrompt = "Thanks for asking. I have sent you the details in a private message."
	MaxInboxCommentPrompt     = 200

	// The bounds an override may take. The intent bar never goes so low that guesses become tags,
	// and the automatic answer never needs less certainty than tagging does.
	MinInboxIntentBar = 0.4
	MaxInboxIntentBar = 0.95
	MinInboxReplyBar  = 0.5
	MaxInboxReplyBar  = 0.99
)

// InboxPolicy is what the inbox worker reads for one tenant.
type InboxPolicy struct {
	AutoReply     bool
	ReplyText     string
	IntentBar     float64
	ReplyBar      float64
	CommentPrompt string
}

// InboxPolicyOf reads the inbox policy from tenant metadata, with the defaults for what is unset.
func InboxPolicyOf(meta map[string]any) InboxPolicy {
	p := InboxPolicy{IntentBar: DefaultInboxIntentBar, ReplyBar: DefaultInboxReplyBar, CommentPrompt: DefaultInboxCommentPrompt}
	p.AutoReply, _ = meta[InboxAutoReplyKey].(bool)
	p.ReplyText, _ = meta[InboxReplyTextKey].(string)
	if v, ok := meta[InboxIntentBarKey].(float64); ok && v >= MinInboxIntentBar && v <= MaxInboxIntentBar {
		p.IntentBar = v
	}
	if v, ok := meta[InboxReplyBarKey].(float64); ok && v >= MinInboxReplyBar && v <= MaxInboxReplyBar {
		p.ReplyBar = v
	}
	if p.ReplyBar < p.IntentBar {
		p.ReplyBar = p.IntentBar
	}
	if s, _ := meta[InboxCommentPromptKey].(string); strings.TrimSpace(s) != "" {
		p.CommentPrompt = strings.TrimSpace(s)
	}
	return p
}

// readInboxOverrides fills the optional inbox fields of Settings from tenant metadata.
func readInboxOverrides(meta map[string]any, s *Settings) {
	if v, ok := meta[InboxIntentBarKey].(float64); ok {
		s.InboxIntentThreshold = &v
	}
	if v, ok := meta[InboxReplyBarKey].(float64); ok {
		s.InboxAutoReplyThreshold = &v
	}
	s.InboxCommentPrompt, _ = meta[InboxCommentPromptKey].(string)
}

// cleanInboxOverrides checks the thresholds and trims the public prompt. A nil threshold clears
// the override, so the default applies again.
func cleanInboxOverrides(s *Settings) error {
	round := func(p *float64) *float64 {
		if p == nil {
			return nil
		}
		v := math.Round(*p*100) / 100
		return &v
	}
	s.InboxIntentThreshold, s.InboxAutoReplyThreshold = round(s.InboxIntentThreshold), round(s.InboxAutoReplyThreshold)
	if v := s.InboxIntentThreshold; v != nil && (*v < MinInboxIntentBar || *v > MaxInboxIntentBar) {
		return fmt.Errorf("the tagging threshold must be between %.2f and %.2f", MinInboxIntentBar, MaxInboxIntentBar)
	}
	if v := s.InboxAutoReplyThreshold; v != nil && (*v < MinInboxReplyBar || *v > MaxInboxReplyBar) {
		return fmt.Errorf("the automatic reply threshold must be between %.2f and %.2f", MinInboxReplyBar, MaxInboxReplyBar)
	}
	intent, reply := DefaultInboxIntentBar, DefaultInboxReplyBar
	if s.InboxIntentThreshold != nil {
		intent = *s.InboxIntentThreshold
	}
	if s.InboxAutoReplyThreshold != nil {
		reply = *s.InboxAutoReplyThreshold
	}
	if reply < intent {
		return fmt.Errorf("the automatic reply threshold cannot be lower than the tagging threshold (%.2f)", intent)
	}
	s.InboxCommentPrompt = strings.TrimSpace(s.InboxCommentPrompt)
	if utf8.RuneCountInString(s.InboxCommentPrompt) > MaxInboxCommentPrompt {
		return fmt.Errorf("the public line under a comment is limited to %d characters", MaxInboxCommentPrompt)
	}
	return nil
}

// inboxOverrideValues are the metadata values to save; a cleared threshold is nil, which
// metastore.SetKeys removes from the metadata, so the default applies again.
func inboxOverrideValues(s Settings) map[string]any {
	val := func(p *float64) any {
		if p == nil {
			return nil
		}
		return *p
	}
	return map[string]any{
		InboxIntentBarKey: val(s.InboxIntentThreshold), InboxReplyBarKey: val(s.InboxAutoReplyThreshold),
		InboxCommentPromptKey: s.InboxCommentPrompt,
	}
}
