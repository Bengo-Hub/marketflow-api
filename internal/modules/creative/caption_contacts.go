package creative

import (
	"context"
	"net/http"
	"strings"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/creativebrief"
	"github.com/bengobox/marketflow-api/internal/httpapi"
)

// contactLabels name a contact in a caption; the end card shows the same ones as icons.
var contactLabels = map[string]string{"whatsapp": "WhatsApp", "phone": "Call", "web": "Website", "email": "Email", "link": "Link"}

// contactLines writes the end card's contacts as caption lines. A contact the business has not
// set is not in the list, so nothing is invented.
func contactLines(list []contact) []string {
	out := make([]string, 0, len(list))
	for _, c := range list {
		v := strings.TrimSpace(c.Value)
		if v == "" {
			continue
		}
		if l := contactLabels[c.Kind]; l != "" {
			v = l + ": " + v
		}
		out = append(out, v)
	}
	return out
}

// ContactLines are the contacts the version's end card shows, as caption lines (owner, 2026-11-25:
// a caption saying "message us" carries the real number, email and website).
func (r *Renderer) ContactLines(ctx context.Context, v *ent.CreativeVersion) []string {
	var offering map[string]any
	if b, err := r.db.CreativeBrief.Query().Where(creativebrief.ID(v.BriefID)).Only(ctx); err == nil {
		offering = b.Offering
	}
	return contactLines(r.resolvePlace(ctx, v.TenantID, offering, false).Contacts)
}

// CaptionContacts handles GET /api/v1/creative/versions/{id}/caption-contacts.
// @Summary Contact lines for a post's caption, the same as the end card's
// @Tags Creative
// @Produce json
// @Param id path string true "Version ID"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /creative/versions/{id}/caption-contacts [get]
func (h *Handler) CaptionContacts(w http.ResponseWriter, r *http.Request) {
	tid, ok := tenantOf(w, r)
	if !ok {
		return
	}
	id, ok := idParam(w, r)
	if !ok {
		return
	}
	v, err := h.svc.version(r.Context(), tid, id)
	if err != nil {
		h.fail(w, err)
		return
	}
	lines := []string{}
	if h.jobs != nil {
		lines = h.jobs.ContactLines(r.Context(), v)
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"lines": lines})
}
