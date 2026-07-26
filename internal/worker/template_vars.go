package worker

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/google/uuid"
)

// splitName is unused now that Lead has first_name/last_name natively; kept
// for reference in case future lead sources supply only a full name string.
var _ = splitName

// templatePlaceholder matches {{var_name}} with optional whitespace and
// dot-separated nested paths (e.g. {{ metadata.city }}).
var templatePlaceholder = regexp.MustCompile(`\{\{\s*([a-zA-Z0-9_.]+)\s*\}\}`)

// resolveNurtureVariables returns the full variable map that a nurture step
// should ship to notifications-api. It augments the step's own `variables`
// map (static copy) with dynamic per-lead / per-tenant attributes so
// templates like "Hi {{first_name}}" and "Order here: {{product_link}}"
// actually render the recipient's name and the FBO's distribID-embedded
// shop link — not literal curly braces.
//
// Keys live in a flat namespace so the same substitution works whether the
// downstream renderer is notifications-api (which supports templating) or a
// bare fallback — we also return a rendered `body`/`subject` pair below.
func resolveNurtureVariables(
	ctx context.Context,
	db *ent.Client,
	stepVars map[string]any,
	ld *ent.Lead,
	tenantID uuid.UUID,
) map[string]any {
	vars := map[string]any{}
	for k, v := range stepVars {
		vars[k] = v
	}

	if ld != nil {
		first := ""
		last := ""
		if ld.FirstName != nil {
			first = strings.TrimSpace(*ld.FirstName)
		}
		if ld.LastName != nil {
			last = strings.TrimSpace(*ld.LastName)
		}
		full := strings.TrimSpace(first + " " + last)
		putIfEmpty(vars, "first_name", first)
		putIfEmpty(vars, "last_name", last)
		putIfEmpty(vars, "full_name", full)
		putIfEmpty(vars, "lead_id", ld.ID.String())
		if ld.Email != nil {
			putIfEmpty(vars, "email", *ld.Email)
		}
		if ld.Phone != nil {
			putIfEmpty(vars, "phone", *ld.Phone)
		}
		if ld.LeadType != "" {
			putIfEmpty(vars, "lead_type", ld.LeadType)
		}
	}

	// Per-tenant FLP + branding vars. These only need resolving when the
	// step actually references them; cheap enough to always fetch.
	tn, err := db.Tenant.Query().Where(tenant.IDEQ(tenantID)).Only(ctx)
	if err == nil {
		putIfEmpty(vars, "fbo_name", tn.Name)
		putIfEmpty(vars, "tenant_slug", tn.Slug)
		profileURL := "https://marketflow.codevertexafrica.com/p/" + tn.Slug
		putIfEmpty(vars, "profile_link", profileURL)
		if tn.WhatsappNumber != nil && *tn.WhatsappNumber != "" {
			waNumber := onlyDigits(*tn.WhatsappNumber)
			putIfEmpty(vars, "whatsapp_number", waNumber)
			putIfEmpty(vars, "whatsapp_link", buildWhatsAppLink(waNumber, tn.Name))
		}
		if tn.FboDistribID != nil && *tn.FboDistribID != "" {
			store := "KEN"
			if tn.FlpStoreCode != nil && *tn.FlpStoreCode != "" {
				store = *tn.FlpStoreCode
			}
			lang := "en"
			if tn.FlpLanguage != nil && *tn.FlpLanguage != "" {
				lang = *tn.FlpLanguage
			}
			shopURL := fmt.Sprintf(
				"https://shop.foreverliving.com/retail/entry/Shop.do?store=%s&language=%s&distribID=%s",
				store, lang, *tn.FboDistribID,
			)
			recruitURL := fmt.Sprintf(
				"https://join.foreverliving.com/marketing/joinnow/applicationForm.do?action=display&store=%s&distribID=%s&menuoff=n&langID=%s",
				store, *tn.FboDistribID, lang,
			)
			putIfEmpty(vars, "product_link", shopURL)
			putIfEmpty(vars, "shop_link", shopURL)
			putIfEmpty(vars, "recruit_link", recruitURL)
			putIfEmpty(vars, "join_link", recruitURL)
		}
	}

	return vars
}

// renderTemplate replaces {{var}} placeholders in s with values from vars.
// Missing placeholders are left intact (visible in the final message) so we
// never swallow a configuration mistake silently.
func renderTemplate(s string, vars map[string]any) string {
	if s == "" || vars == nil {
		return s
	}
	return templatePlaceholder.ReplaceAllStringFunc(s, func(match string) string {
		m := templatePlaceholder.FindStringSubmatch(match)
		if len(m) < 2 {
			return match
		}
		if v, ok := lookupPath(vars, m[1]); ok {
			return fmt.Sprintf("%v", v)
		}
		return match
	})
}

func lookupPath(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	var cur any = m
	for _, p := range parts {
		asMap, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, ok := asMap[p]
		if !ok {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

func putIfEmpty(m map[string]any, k, v string) {
	if v == "" {
		return
	}
	if existing, ok := m[k]; !ok || existing == "" || existing == nil {
		m[k] = v
	}
}

func splitName(full string) (first, last string) {
	trimmed := strings.TrimSpace(full)
	if trimmed == "" {
		return "", ""
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], strings.Join(parts[1:], " ")
}

func onlyDigits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func buildWhatsAppLink(number, fboName string) string {
	msg := "Hi"
	if fboName != "" {
		msg += " " + fboName
	}
	msg += ", I'm interested in learning more."
	return fmt.Sprintf("https://wa.me/%s?text=%s", number, urlEncode(msg))
}

func urlEncode(s string) string {
	// Minimal encoder sufficient for our canned greeting — keeps the
	// helper local and avoids pulling net/url just for two chars.
	repl := map[string]string{
		" ":  "%20",
		",":  "%2C",
		"'":  "%27",
		"\"": "%22",
		"\n": "%0A",
	}
	out := s
	for k, v := range repl {
		out = strings.ReplaceAll(out, k, v)
	}
	return out
}
