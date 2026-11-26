// Package adsetup is the ads account setup assistant (plan item 65, owner 2026-11-24): a plain
// language checklist for each connected Google Ads, Meta and TikTok ad account. It reads the
// account and, where the provider's API allows it, offers a fix. A fix is always shown first and
// runs only after the owner taps Fix; nothing here writes on its own.
package adsetup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"unicode/utf8"
)

// Item statuses.
const (
	StatusOK      = "ok"
	StatusAction  = "action"  // stops ads or tracking
	StatusWarning = "warning" // ads run, but worse than they could
	StatusInfo    = "info"    // nothing to do
	StatusUnknown = "unknown" // the provider did not let MarketFlow read it
)

// Link is a page on the provider's site.
type Link struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

// Field is one value the owner reviews before a fix runs.
type Field struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Value    string `json:"value"`
	Max      int    `json:"max,omitempty"` // characters, 0 for no limit of its own
	Required bool   `json:"required,omitempty"`
	// Picture makes the field a tick: "yes" sends the picture.
	Picture *PictureChoice `json:"picture,omitempty"`
}

// Fix is a change the provider's API allows. Changes is the exact list shown to the owner.
type Fix struct {
	Label   string   `json:"label"`
	Changes []string `json:"changes"`
	Fields  []Field  `json:"fields,omitempty"`
	// Token names this exact proposal: a fix whose proposal changed since it was shown is refused.
	Token string `json:"token"`

	run func(ctx context.Context, in map[string]string) error
}

// Item is one check.
type Item struct {
	Key    string   `json:"key"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Detail string   `json:"detail"`
	Points []string `json:"points,omitempty"`
	// Steps are what the owner does themselves, in order.
	Steps []string `json:"steps,omitempty"`
	// Support is the provider's own words, for MarketFlow support, not shown as an explanation.
	Support string `json:"support,omitempty"`
	Link    *Link  `json:"link,omitempty"`
	Fix     *Fix   `json:"fix,omitempty"`
}

// seal gives every fix its token. The proposed field values are left out: the owner sends the
// values they reviewed, so a slow website read must not make a fix look changed.
func seal(items []Item) []Item {
	items = withSteps(items)
	for i := range items {
		if f := items[i].Fix; f != nil {
			b, _ := json.Marshal([]any{items[i].Key, f.Changes})
			sum := sha256.Sum256(b)
			f.Token = hex.EncodeToString(sum[:8])
		}
	}
	return items
}

// InputError is a value the owner has to correct.
type InputError struct{ Msg string }

func (e *InputError) Error() string { return e.Msg }

func badInput(format string, a ...any) error { return &InputError{Msg: fmt.Sprintf(format, a...)} }

// cleanInput keeps the fix's own fields, trimmed and checked. Only values the owner sent are used.
func cleanInput(fields []Field, in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, f := range fields {
		v := strings.TrimSpace(in[f.Key])
		if f.Required && v == "" {
			return nil, badInput("%s is required", f.Label)
		}
		if f.Max > 0 && utf8.RuneCountInString(v) > f.Max {
			return nil, badInput("%s is longer than %d characters", f.Label, f.Max)
		}
		out[f.Key] = v
	}
	return out, nil
}

// httpsURL reports an absolute https address.
func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}

// unread is the item for a check the provider refused or did not answer.
func unread(key, title, provider string, err error, l *Link) Item {
	return Item{Key: key, Title: title, Status: StatusUnknown, Link: l,
		Detail:  provider + " did not let MarketFlow read this, so check it yourself.",
		Support: clip(safeErr(err), 200)}
}

// safeErr is an error's text without the request address: Graph calls carry the access token in
// the query string, and a network error repeats the whole address.
func safeErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return "no answer (" + ue.Err.Error() + ")"
	}
	return err.Error()
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// together runs the reads side by side and waits for all of them.
func together(fs ...func()) {
	var wg sync.WaitGroup
	for _, f := range fs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f()
		}()
	}
	wg.Wait()
}

// words turns a provider constant ("BUDGET_CONSTRAINED") into readable text.
func words(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "_", " "))
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
