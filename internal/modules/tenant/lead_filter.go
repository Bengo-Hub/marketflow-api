package tenant

import (
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// LeadFilterKey is the tenant's own words that mark a submission as not a lead (leads.LeadFilter
// .WithTenant reads it): {attachment: [...], job: [...], service_offer: [...]}. The words are
// added to the platform lists (MF_LEAD_FILTER_*), never in place of them. Saved through the
// profile PATCH by the approving roles.
const LeadFilterKey = "lead_filter"

// LeadFilterKinds are the lists a tenant can add words to, in the order the settings page shows.
var LeadFilterKinds = []string{"attachment", "job", "service_offer"}

// Limits keep the check fast and the metadata row small.
const (
	maxLeadFilterWords   = 50
	maxLeadFilterWordLen = 80
)

// ValidateLeadFilterSetting checks and normalises metadata.lead_filter in a profile update: each
// kind is a list of words or phrases, trimmed, lower case, without repeats. Null clears the
// tenant's words; an unknown kind or a word that is too long is refused.
func ValidateLeadFilterSetting(meta map[string]any) error {
	v, ok := meta[LeadFilterKey]
	if !ok || v == nil {
		return nil
	}
	in, isMap := v.(map[string]any)
	if !isMap {
		return errors.New("lead_filter must be an object with attachment, job and service_offer lists")
	}
	out := map[string]any{}
	for kind, raw := range in {
		if !isLeadFilterKind(kind) {
			return fmt.Errorf("lead_filter.%s is not a known list; use %s", kind, strings.Join(LeadFilterKinds, ", "))
		}
		if raw == nil {
			continue
		}
		list, isList := raw.([]any)
		if !isList {
			return fmt.Errorf("lead_filter.%s must be a list of words", kind)
		}
		words := []any{}
		seen := map[string]bool{}
		for _, item := range list {
			s, isStr := item.(string)
			if !isStr {
				return fmt.Errorf("lead_filter.%s must be a list of words", kind)
			}
			s = strings.ToLower(strings.Join(strings.Fields(s), " "))
			if s == "" || seen[s] {
				continue
			}
			if len([]rune(s)) > maxLeadFilterWordLen {
				return fmt.Errorf("lead_filter.%s: %q is longer than %d characters", kind, s, maxLeadFilterWordLen)
			}
			seen[s] = true
			words = append(words, s)
		}
		if len(words) > maxLeadFilterWords {
			return fmt.Errorf("lead_filter.%s holds at most %d words", kind, maxLeadFilterWords)
		}
		if len(words) > 0 {
			out[kind] = words
		}
	}
	if len(out) == 0 {
		meta[LeadFilterKey] = nil
		return nil
	}
	meta[LeadFilterKey] = out
	return nil
}

// LeadFilterChanged reports whether the normalised value v differs from the stored one.
func LeadFilterChanged(stored map[string]any, v any) bool {
	return !reflect.DeepEqual(tenantLeadWords(stored[LeadFilterKey]), tenantLeadWords(v))
}

// tenantLeadWords reads a lead_filter value into plain lists, one per kind (empty when unset).
func tenantLeadWords(v any) map[string][]string {
	lf, _ := v.(map[string]any)
	out := map[string][]string{}
	for _, kind := range LeadFilterKinds {
		out[kind] = []string{}
		list, _ := lf[kind].([]any)
		for _, item := range list {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out[kind] = append(out[kind], s)
			}
		}
	}
	return out
}

func isLeadFilterKind(kind string) bool {
	for _, k := range LeadFilterKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// GetLeadFilter handles GET /api/v1/tenant/profile/lead-filter: the platform lists (read only)
// and the tenant's own words, so the settings page can show both.
// @Summary Lead filter words
// @Tags Tenant
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /tenant/profile/lead-filter [get]
func (h *Handler) GetLeadFilter(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := tenantHeader(w, r)
	if !ok {
		return
	}
	t, err := h.db.Tenant.Get(r.Context(), tenantID)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "tenant not found", nil)
		return
	}
	platform := map[string][]string{}
	if h.PlatformLeadFilter != nil {
		platform = h.PlatformLeadFilter()
	}
	for _, kind := range LeadFilterKinds {
		if platform[kind] == nil {
			platform[kind] = []string{}
		}
	}
	canEdit := h.CanApprove == nil || h.CanApprove(r.Context(), tenantID)
	httpapi.JSON(w, http.StatusOK, map[string]any{
		"platform": platform,
		"tenant":   tenantLeadWords(t.Metadata[LeadFilterKey]),
		"can_edit": canEdit,
	})
}
