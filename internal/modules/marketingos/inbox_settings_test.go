package marketingos

import (
	"strings"
	"testing"
)

func bar(v float64) *float64 { return &v }

// The tenant's thresholds stay inside their bounds, the automatic answer never needs less
// certainty than tagging, and a cleared override falls back to the default.
func TestInboxOverrides(t *testing.T) {
	cases := []struct {
		name          string
		intent, reply *float64
		err           string
	}{
		{"defaults", nil, nil, ""},
		{"stricter tagging", bar(0.7), bar(0.8), ""},
		{"looser reply alone", nil, bar(0.6), ""},
		{"tagging too low", bar(0.2), nil, "between"},
		{"reply too high", nil, bar(1.2), "between"},
		{"reply below tagging", bar(0.8), bar(0.6), "cannot be lower"},
		{"reply below the default tagging", nil, bar(0.5), "cannot be lower"},
	}
	for _, c := range cases {
		s := Settings{InboxIntentThreshold: c.intent, InboxAutoReplyThreshold: c.reply}
		err := cleanInboxOverrides(&s)
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	s := Settings{InboxCommentPrompt: strings.Repeat("a", MaxInboxCommentPrompt+1)}
	if err := cleanInboxOverrides(&s); err == nil {
		t.Error("a long public line must be refused")
	}
	vals := inboxOverrideValues(Settings{})
	if v, ok := vals[InboxIntentBarKey]; !ok || v != nil {
		t.Errorf("a cleared threshold is nil so the key is removed: %v", vals)
	}
}

func TestInboxPolicyOf(t *testing.T) {
	p := InboxPolicyOf(nil)
	if p.IntentBar != DefaultInboxIntentBar || p.ReplyBar != DefaultInboxReplyBar || p.CommentPrompt != DefaultInboxCommentPrompt || p.AutoReply {
		t.Fatalf("defaults %+v", p)
	}
	p = InboxPolicyOf(map[string]any{InboxAutoReplyKey: true, InboxReplyTextKey: "hi", InboxIntentBarKey: 0.8, InboxReplyBarKey: 0.6,
		InboxCommentPromptKey: " Check your messages "})
	if !p.AutoReply || p.ReplyText != "hi" || p.IntentBar != 0.8 || p.ReplyBar != 0.8 || p.CommentPrompt != "Check your messages" {
		t.Fatalf("overrides %+v", p)
	}
	// A value outside the bounds, however it got there, is ignored.
	if p := InboxPolicyOf(map[string]any{InboxIntentBarKey: 0.1}); p.IntentBar != DefaultInboxIntentBar {
		t.Fatalf("out of bounds %+v", p)
	}
}
