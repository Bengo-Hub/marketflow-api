package creative

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/bengobox/marketflow-api/internal/notifications"
)

// Reply buttons, lists and media from the platform number (Sprint 17, owner 2026-11-14). Meta
// takes these free-form messages only inside the 24-hour window the requester's last message
// opened; outside it the plain text goes as before (the template path), and the typed
// "YES <code>" in that text keeps working.

// WhatsAppConfig holds the limits for rich sends. ConfigFromEnv reads them.
type WhatsAppConfig struct {
	// Window is how long after a person's last message rich messages may be sent; a little
	// under Meta's 24 hours, so a send never lands just past it.
	Window time.Duration
	// VideoMaxBytes and ImageMaxBytes are the largest files sent natively (Meta: 16 MB video,
	// 5 MB image); a bigger or unknown size goes as a link.
	VideoMaxBytes int64
	ImageMaxBytes int64
}

// DefaultWhatsAppConfig is 23h45m, 16 MB and 5 MB.
func DefaultWhatsAppConfig() WhatsAppConfig {
	return WhatsAppConfig{Window: 23*time.Hour + 45*time.Minute, VideoMaxBytes: 16 << 20, ImageMaxBytes: 5 << 20}
}

// WhatsAppConfigFromEnv reads MF_WHATSAPP_WINDOW (a Go duration), MF_WHATSAPP_VIDEO_MAX_BYTES and
// MF_WHATSAPP_IMAGE_MAX_BYTES. A bad value keeps its default and is listed in the error.
func WhatsAppConfigFromEnv(getenv func(string) string) (WhatsAppConfig, error) {
	cfg := DefaultWhatsAppConfig()
	var bad []string
	if v := strings.TrimSpace(getenv("MF_WHATSAPP_WINDOW")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 && d <= 24*time.Hour {
			cfg.Window = d
		} else {
			bad = append(bad, "MF_WHATSAPP_WINDOW must be a duration up to 24h")
		}
	}
	size := func(key string, dst *int64) {
		v := strings.TrimSpace(getenv(key))
		if v == "" {
			return
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			*dst = n
		} else {
			bad = append(bad, key+" must be a positive number of bytes")
		}
	}
	size("MF_WHATSAPP_VIDEO_MAX_BYTES", &cfg.VideoMaxBytes)
	size("MF_WHATSAPP_IMAGE_MAX_BYTES", &cfg.ImageMaxBytes)
	if len(bad) > 0 {
		return cfg, fmt.Errorf("whatsapp: %s", strings.Join(bad, "; "))
	}
	return cfg, nil
}

// whatsAppWindowKey marks a number's open window; the key lives as long as the window.
func whatsAppWindowKey(number string) string {
	return "mf:wa:window:" + strings.TrimPrefix(strings.TrimSpace(number), "+")
}

// MarkWhatsAppInbound opens (or extends) a number's window: call it for every message the
// platform number receives.
func MarkWhatsAppInbound(ctx context.Context, rdb *redis.Client, number string, window time.Duration) {
	if rdb == nil || number == "" || window <= 0 {
		return
	}
	rdb.Set(ctx, whatsAppWindowKey(number), time.Now().UTC().Format(time.RFC3339), window)
}

// WithRich turns on reply buttons, lists and media for numbers whose window is open. Without it
// every send is plain text.
func (p *PlatformWhatsApp) WithRich(rdb *redis.Client, cfg WhatsAppConfig) *PlatformWhatsApp {
	d := DefaultWhatsAppConfig()
	if cfg.Window <= 0 {
		cfg.Window = d.Window
	}
	if cfg.VideoMaxBytes <= 0 {
		cfg.VideoMaxBytes = d.VideoMaxBytes
	}
	if cfg.ImageMaxBytes <= 0 {
		cfg.ImageMaxBytes = d.ImageMaxBytes
	}
	p.rdb, p.cfg = rdb, cfg
	p.probe = &http.Client{Timeout: 10 * time.Second}
	return p
}

// Window reports the configured window, for the worker that marks inbound messages.
func (p *PlatformWhatsApp) Window() time.Duration {
	if p == nil || p.cfg.Window <= 0 {
		return DefaultWhatsAppConfig().Window
	}
	return p.cfg.Window
}

// InWindow reports whether the number messaged the platform number within the window. Unknown
// (no Redis, an error) counts as closed, so the plain text goes.
func (p *PlatformWhatsApp) InWindow(ctx context.Context, to string) bool {
	if p == nil || p.rdb == nil {
		return false
	}
	n, err := p.rdb.Exists(ctx, whatsAppWindowKey(to)).Result()
	return err == nil && n == 1
}

// send is one WhatsApp message from the platform number.
func (p *PlatformWhatsApp) send(ctx context.Context, to, body, key string, rich *notifications.WhatsAppRich) error {
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
		Body:           body,
		Metadata:       map[string]string{"source": p.source, "kind": "whatsapp_assistant"},
		IdempotencyKey: key,
		WhatsApp:       rich,
	})
}

// SendCTA sends text with one link button (inside the window; plain text with the link outside).
func (p *PlatformWhatsApp) SendCTA(ctx context.Context, to, text, button, link, key string) error {
	if !p.InWindow(ctx, to) {
		return p.Send(ctx, to, text+"\n\n"+link, key)
	}
	return p.send(ctx, to, text, key, &notifications.WhatsAppRich{CTAText: button, CTAURL: link, FallbackText: text + "\n\n" + link})
}

// SendRich sends marketflow-ai's rich form of a message while the window is open: each media
// item natively (or as a link when it is too big or its size is unknown), then the body with its
// buttons or list. Outside the window, or with no rich form, text goes as plain text. Each part
// has its own idempotency key derived from key.
func (p *PlatformWhatsApp) SendRich(ctx context.Context, to, text string, ia *notifications.WhatsAppInteractive, key string) error {
	if ia == nil || !p.InWindow(ctx, to) {
		return p.Send(ctx, to, text, key)
	}
	var asLinks []string
	for i, m := range ia.Media {
		caption := strings.TrimSpace(m.Caption)
		if !p.mediaFits(ctx, m) {
			asLinks = append(asLinks, linkLine(caption, m.Link))
			continue
		}
		body := caption
		if body == "" {
			body = m.Type
		}
		if err := p.send(ctx, to, body, fmt.Sprintf("%s-m%d", key, i), &notifications.WhatsAppRich{
			MediaType: m.Type, MediaLink: m.Link, FallbackText: linkLine(caption, m.Link)}); err != nil {
			asLinks = append(asLinks, linkLine(caption, m.Link))
		}
	}
	body := strings.TrimSpace(ia.Body)
	if body == "" {
		if len(asLinks) == 0 {
			return nil // the media was the whole message
		}
		// Media that could not go natively: the plain text carries every link.
		return p.Send(ctx, to, text, key)
	}
	if len(asLinks) > 0 {
		body = strings.Join(asLinks, "\n") + "\n\n" + body
	}
	if len([]rune(body)) > maxRichBody {
		// Meta would cut the body and lose the approve line at its end: the plain text goes.
		return p.Send(ctx, to, text, key)
	}
	rich := &notifications.WhatsAppRich{Footer: ia.Footer, FallbackText: text}
	switch {
	case len(ia.Buttons) > 0:
		rich.Buttons = ia.Buttons
	case ia.List != nil && len(ia.List.Rows) > 0:
		rich.List = ia.List
	default:
		return p.Send(ctx, to, body, key)
	}
	return p.send(ctx, to, body, key, rich)
}

// maxRichBody is Meta's limit for the text of a button or list message.
const maxRichBody = 1024

// linkLine is a media item sent as text: its caption, then the link.
func linkLine(caption, link string) string {
	if caption == "" {
		return link
	}
	return caption + ": " + link
}

// mediaFits asks the file's host for its size with a one-byte range request (works on signed
// GET links, which a HEAD request would break) and sends natively only a file of a known size
// within the limit and of the right kind.
func (p *PlatformWhatsApp) mediaFits(ctx context.Context, m notifications.WhatsAppMedia) bool {
	if p.probe == nil || !strings.HasPrefix(m.Link, "https://") {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Link, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := p.probe.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return false
	}
	size := resp.ContentLength
	if resp.StatusCode == http.StatusPartialContent {
		size = contentRangeTotal(resp.Header.Get("Content-Range"))
	}
	return MediaFits(m.Type, resp.Header.Get("Content-Type"), size, p.cfg)
}

// contentRangeTotal reads the total from "bytes 0-0/12345"; -1 when unknown.
func contentRangeTotal(h string) int64 {
	i := strings.LastIndex(h, "/")
	if i < 0 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(h[i+1:]), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// MediaFits decides whether a file goes as a native WhatsApp message: an image or a video, of a
// known size within its limit, whose content type (when given) is of the same kind.
func MediaFits(mediaType, contentType string, size int64, cfg WhatsAppConfig) bool {
	var max int64
	switch mediaType {
	case "image":
		max = cfg.ImageMaxBytes
	case "video":
		max = cfg.VideoMaxBytes
	default:
		return false
	}
	if size <= 0 || size > max {
		return false
	}
	if ct := strings.ToLower(strings.TrimSpace(contentType)); ct != "" && !strings.HasPrefix(ct, mediaType+"/") {
		return false
	}
	return true
}
