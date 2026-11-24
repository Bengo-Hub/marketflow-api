package leads

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Bengo-Hub/pagination"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/activity"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/bengobox/marketflow-api/internal/modules/audit"
)

// Service-to-service lead API (X-API-Key, no JWT). MarketFlow owns leads; other Codevertex
// services (the company website's admin, maskani, the cafe site) read and write them here and
// keep no copy (owner, 2026-11-24).

// errTenant means the X-Tenant-ID header names no active tenant.
var errTenant = errors.New("unknown tenant")

// internalTenant reads X-Tenant-ID as the tenant's UUID or its slug. A slug lets a service
// configure "codevertex" rather than a copied UUID.
func (h *Handler) internalTenant(r *http.Request) (uuid.UUID, error) {
	v := strings.TrimSpace(r.Header.Get("X-Tenant-ID"))
	if v == "" {
		return uuid.Nil, errTenant
	}
	if id, err := uuid.Parse(v); err == nil {
		return id, nil
	}
	t, err := h.service.client.Tenant.Query().Where(tenant.SlugEQ(strings.ToLower(v)), tenant.StatusEQ("active")).Only(r.Context())
	if err != nil {
		return uuid.Nil, errTenant
	}
	return t.ID, nil
}

func (h *Handler) tenantOr400(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	tid, err := h.internalTenant(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "X-Tenant-ID must be a tenant UUID or slug", nil)
		return uuid.Nil, false
	}
	return tid, true
}

// listInput reads the shared list filter: status, source and exclude_source (comma lists),
// lead_type, q, from and to.
func listInput(r *http.Request, tid uuid.UUID) ListInput {
	q := r.URL.Query()
	in := ListInput{TenantID: tid, Search: q.Get("q")}
	in.Statuses = splitList(q.Get("status"))
	if v := q.Get("lead_type"); v != "" {
		in.LeadType = &v
	}
	in.Sources = splitList(q.Get("source"))
	in.ExcludeSources = splitList(q.Get("exclude_source"))
	if t, ok := parseDay(q.Get("from"), false); ok {
		in.From = &t
	}
	if t, ok := parseDay(q.Get("to"), true); ok {
		in.To = &t
	}
	return in
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" && len(out) < 20 {
			out = append(out, s)
		}
	}
	return out
}

// parseDay reads RFC3339 or YYYY-MM-DD; a plain day as the end of a range means its last moment.
func parseDay(v string, end bool) (time.Time, bool) {
	if v == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		if end {
			t = t.Add(24*time.Hour - time.Nanosecond)
		}
		return t, true
	}
	return time.Time{}, false
}

// ListInternal handles GET /api/v1/internal/leads.
// @Summary List leads (service to service)
// @Tags Internal
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Param status query string false "Status"
// @Param source query string false "Sources, comma separated"
// @Param exclude_source query string false "Sources to leave out, comma separated"
// @Param q query string false "Search name, email, phone or interest"
// @Success 200 {object} map[string]interface{}
// @Router /internal/leads [get]
func (h *Handler) ListInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	p := pagination.Parse(r)
	in := listInput(r, tid)
	in.Limit, in.Offset = p.Limit, p.Offset
	rows, total, err := h.service.List(r.Context(), in)
	if err != nil {
		h.logger.Error("internal list leads", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list leads", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, pagination.NewResponse(rows, total, p))
}

// StatsInternal handles GET /api/v1/internal/leads/stats: totals by status and source for the
// same filter as the list.
// @Summary Lead counts (service to service)
// @Tags Internal
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Success 200 {object} Stats
// @Router /internal/leads/stats [get]
func (h *Handler) StatsInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	st, err := h.service.Stats(r.Context(), listInput(r, tid))
	if err != nil {
		h.logger.Error("internal lead stats", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "stats_failed", "failed to count leads", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, st)
}

// GetInternal handles GET /api/v1/internal/leads/{id}: the lead and its last 50 submissions.
// @Summary Get a lead (service to service)
// @Tags Internal
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Param id path string true "Lead UUID"
// @Success 200 {object} map[string]interface{}
// @Router /internal/leads/{id} [get]
func (h *Handler) GetInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	l, ok := h.internalLead(w, r, tid)
	if !ok {
		return
	}
	subs, err := h.service.Submissions(r.Context(), tid, l.ID, 50)
	if err != nil {
		h.logger.Warn("lead submissions", zap.Error(err))
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"lead": l, "submissions": subs})
}

// Submissions returns a lead's recorded repeat submissions, newest first.
func (s *Service) Submissions(ctx context.Context, tenantID, leadID uuid.UUID, limit int) ([]*ent.Activity, error) {
	return s.client.Activity.Query().
		Where(activity.TenantID(tenantID), activity.EntityType("lead"), activity.EntityID(leadID), activity.ActivityType(ActivitySubmission)).
		Order(ent.Desc(activity.FieldPerformedAt)).
		Limit(limit).
		All(ctx)
}

func (h *Handler) internalLead(w http.ResponseWriter, r *http.Request, tid uuid.UUID) (*ent.Lead, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid lead ID", nil)
		return nil, false
	}
	l, err := h.service.Get(r.Context(), tid, id)
	if err != nil {
		httpapi.Error(w, http.StatusNotFound, "not_found", "lead not found", nil)
		return nil, false
	}
	return l, true
}

// validStatus is the lead lifecycle.
var validStatus = map[string]bool{"new": true, "contacted": true, "qualified": true, "converted": true, "lost": true}

type patchInternalRequest struct {
	Status    *string        `json:"status"`
	Email     *string        `json:"email"`
	Phone     *string        `json:"phone"`
	FirstName *string        `json:"first_name"`
	LastName  *string        `json:"last_name"`
	Interest  *string        `json:"interest"`
	Metadata  map[string]any `json:"metadata"`
}

// PatchInternal handles PATCH /api/v1/internal/leads/{id}. Fields left out keep their values;
// metadata is merged key by key (null removes a key).
// @Summary Update a lead (service to service)
// @Tags Internal
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Param id path string true "Lead UUID"
// @Param request body patchInternalRequest true "Fields to change"
// @Success 200 {object} map[string]interface{}
// @Router /internal/leads/{id} [patch]
func (h *Handler) PatchInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	l, ok := h.internalLead(w, r, tid)
	if !ok {
		return
	}
	var req patchInternalRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if req.Status != nil && !validStatus[*req.Status] {
		httpapi.Error(w, http.StatusBadRequest, "invalid_status", "status must be new, contacted, qualified, converted or lost", nil)
		return
	}
	out, err := h.service.Update(r.Context(), tid, l.ID, UpdateInput{
		Status: req.Status, Email: req.Email, Phone: req.Phone, FirstName: req.FirstName,
		LastName: req.LastName, Interest: req.Interest, Metadata: req.Metadata,
	})
	if err != nil {
		h.logger.Error("internal update lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "update_failed", "failed to update lead", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, out)
}

// DeleteInternal handles DELETE /api/v1/internal/leads/{id}: the CRM's delete, which marks the
// lead lost and keeps its history.
// @Summary Delete a lead (service to service; marks it lost)
// @Tags Internal
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Param id path string true "Lead UUID"
// @Success 200 {object} map[string]string
// @Router /internal/leads/{id} [delete]
func (h *Handler) DeleteInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	l, ok := h.internalLead(w, r, tid)
	if !ok {
		return
	}
	if err := h.service.Delete(r.Context(), tid, l.ID); err != nil {
		h.logger.Error("internal delete lead", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "delete_failed", "failed to delete lead", nil)
		return
	}
	audit.Tenant(r.Context(), h.service.client, h.logger, tid, "lead.marked_lost", "lead", &l.ID, map[string]any{"via": "service"})
	httpapi.JSON(w, http.StatusOK, map[string]any{"status": "deleted"})
}

type importRowRequest struct {
	ExternalRef string         `json:"external_ref"`
	CreatedAt   time.Time      `json:"created_at"`
	Source      string         `json:"source"`
	Status      string         `json:"status"`
	LeadType    string         `json:"lead_type"`
	Email       *string        `json:"email"`
	Phone       *string        `json:"phone"`
	FirstName   *string        `json:"first_name"`
	LastName    *string        `json:"last_name"`
	Interest    *string        `json:"interest"`
	Metadata    map[string]any `json:"metadata"`
}

type importRequest struct {
	Leads []importRowRequest `json:"leads"`
}

// ImportInternal handles POST /api/v1/internal/leads/import: another service's leads moved into
// MarketFlow, keeping source, status and created time, deduped by email and phone, and safe to
// run again (each row's lead id is fixed by its external_ref). No lead.created goes out.
// @Summary Import leads from another service
// @Tags Internal
// @Accept json
// @Produce json
// @Param X-Tenant-ID header string true "Tenant UUID or slug"
// @Param request body importRequest true "Up to 500 leads"
// @Success 200 {object} map[string]interface{}
// @Router /internal/leads/import [post]
func (h *Handler) ImportInternal(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.tenantOr400(w, r)
	if !ok {
		return
	}
	var req importRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if len(req.Leads) == 0 || len(req.Leads) > MaxImportRows {
		httpapi.Error(w, http.StatusBadRequest, "invalid_batch", "send 1 to 500 leads", nil)
		return
	}
	rows := make([]ImportRow, 0, len(req.Leads))
	for i, l := range req.Leads {
		if strings.TrimSpace(l.ExternalRef) == "" || (blank(l.Email) && blank(l.Phone)) {
			httpapi.Error(w, http.StatusBadRequest, "invalid_row", "every lead needs external_ref and an email or phone", map[string]any{"index": i})
			return
		}
		if l.Status != "" && !validStatus[l.Status] {
			httpapi.Error(w, http.StatusBadRequest, "invalid_status", "invalid status", map[string]any{"index": i})
			return
		}
		rows = append(rows, ImportRow{ExternalRef: l.ExternalRef, CreatedAt: l.CreatedAt, Lead: CreateInput{
			Source: orDefault(l.Source, "platform_service"), Status: l.Status, LeadType: orDefault(l.LeadType, "customer"),
			Email: trimPtr(l.Email), Phone: trimPtr(l.Phone), FirstName: trimPtr(l.FirstName), LastName: trimPtr(l.LastName),
			Interest: trimPtr(l.Interest), Metadata: l.Metadata,
		}})
	}
	results, err := h.service.Import(r.Context(), tid, rows)
	counts := map[string]int{}
	for _, res := range results {
		counts[res.Outcome]++
	}
	if err != nil {
		h.logger.Error("lead import stopped", zap.Int("done", len(results)), zap.Error(err))
		httpapi.JSON(w, http.StatusInternalServerError, map[string]any{"error": "import_failed", "results": results, "counts": counts})
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"results": results, "counts": counts})
}

func blank(s *string) bool { return s == nil || strings.TrimSpace(*s) == "" }

func trimPtr(s *string) *string {
	if blank(s) {
		return nil
	}
	v := strings.TrimSpace(*s)
	return &v
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}
