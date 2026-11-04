package custom_agents

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"entgo.io/ent/dialect/sql"
	"github.com/bengobox/marketflow-api/internal/ent"
	"github.com/bengobox/marketflow-api/internal/ent/customagent"
	"github.com/bengobox/marketflow-api/internal/ent/predicate"
	"github.com/bengobox/marketflow-api/internal/ent/tenant"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Agent templates (Sprint 20): premade agents a tenant hires from a store. A template is a
// CustomAgent row owned by the platform tenant with metadata.template {category, industry,
// description, version}. Hiring clones it into the tenant, switched off, with
// metadata.template_ref {id, version}; a newer template version is offered, never applied.

// WithPlatformTenant sets the platform tenant's slug, the owner of the templates.
func WithPlatformTenant(slug string) func(*Service) {
	return func(s *Service) { s.platformSlug = slug }
}

func (s *Service) platformID(ctx context.Context) (uuid.UUID, error) {
	if s.platformSlug == "" {
		return uuid.Nil, errors.New("platform tenant not configured")
	}
	t, err := s.client.Tenant.Query().Where(tenant.SlugEQ(s.platformSlug)).Only(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	return t.ID, nil
}

// isTemplate matches rows that carry metadata.template.
func isTemplate() predicate.CustomAgent {
	return func(s *sql.Selector) {
		s.Where(sql.ExprP(fmt.Sprintf("jsonb_exists(COALESCE(%s, '{}'::jsonb), 'template')", s.C(customagent.FieldMetadata))))
	}
}

// notTemplate keeps template rows out of the platform tenant's own agent list.
func notTemplate() predicate.CustomAgent {
	return func(s *sql.Selector) {
		s.Where(sql.ExprP(fmt.Sprintf("NOT jsonb_exists(COALESCE(%s, '{}'::jsonb), 'template')", s.C(customagent.FieldMetadata))))
	}
}

// Template is one store entry.
type Template struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Kind        string   `json:"kind"`
	Category    string   `json:"category"`
	Industries  []string `json:"industries"`
	Version     int      `json:"version"`
	TriggerType string   `json:"trigger_type"`
	Tools       []string `json:"allowed_tools"`
}

// ListTemplates returns the store, optionally for one industry. The store is small (tens of rows),
// so it is filtered in code after one indexed read of the platform tenant.
func (s *Service) ListTemplates(ctx context.Context, industry string) ([]Template, error) {
	pid, err := s.platformID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.client.CustomAgent.Query().Where(customagent.TenantID(pid), isTemplate()).
		Order(ent.Asc(customagent.FieldName)).Limit(200).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Template, 0, len(rows))
	for _, a := range rows {
		t := toTemplate(a)
		if industry != "" && len(t.Industries) > 0 && !containsFold(t.Industries, industry) {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

func toTemplate(a *ent.CustomAgent) Template {
	meta, _ := a.Metadata["template"].(map[string]any)
	str := func(m map[string]any, k string) string { v, _ := m[k].(string); return v }
	t := Template{ID: a.ID.String(), Name: a.Name, Kind: str(a.Metadata, "kind"), Category: str(meta, "category"),
		Description: str(meta, "description"), TriggerType: a.TriggerType, Tools: a.AllowedTools}
	if v, ok := meta["version"].(float64); ok {
		t.Version = int(v)
	}
	if t.Description == "" && a.Description != nil {
		t.Description = *a.Description
	}
	if list, ok := meta["industries"].([]any); ok {
		for _, i := range list {
			if s, ok := i.(string); ok {
				t.Industries = append(t.Industries, s)
			}
		}
	}
	return t
}

func containsFold(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}

// ErrNotTemplate means the id is not a store template.
var ErrNotTemplate = errors.New("not an agent template")

// Hire clones a template into the tenant, switched off so the owner reviews it first.
func (s *Service) Hire(ctx context.Context, tenantID, templateID uuid.UUID) (*ent.CustomAgent, error) {
	pid, err := s.platformID(ctx)
	if err != nil {
		return nil, err
	}
	tpl, err := s.client.CustomAgent.Query().Where(customagent.IDEQ(templateID), customagent.TenantID(pid), isTemplate()).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, ErrNotTemplate
		}
		return nil, err
	}
	meta := map[string]any{}
	for k, v := range tpl.Metadata {
		if k != "template" && k != "versions" {
			meta[k] = v
		}
	}
	ver := toTemplate(tpl).Version
	meta["template_ref"] = map[string]any{"id": tpl.ID.String(), "version": ver}
	c := s.client.CustomAgent.Create().SetTenantID(tenantID).SetName(tpl.Name).SetTriggerType(tpl.TriggerType).
		SetMaxIterations(tpl.MaxIterations).SetIsActive(false).SetMetadata(meta)
	if tpl.Description != nil {
		c.SetNillableDescription(tpl.Description)
	}
	if tpl.TriggerConfig != nil {
		c.SetTriggerConfig(tpl.TriggerConfig)
	}
	if tpl.SystemPrompt != nil {
		c.SetNillableSystemPrompt(tpl.SystemPrompt)
	}
	if len(tpl.AllowedTools) > 0 {
		c.SetAllowedTools(tpl.AllowedTools)
	}
	return c.Save(ctx)
}

// Templates handles GET /api/v1/agents/templates?industry=.
// @Summary Agent templates to hire
// @Tags Custom Agents
// @Produce json
// @Param industry query string false "Filter by industry"
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/templates [get]
func (h *Handler) Templates(w http.ResponseWriter, r *http.Request) {
	list, err := h.service.ListTemplates(r.Context(), r.URL.Query().Get("industry"))
	if err != nil {
		h.logger.Error("list agent templates", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "list_failed", "failed to list agent templates", nil)
		return
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"data": list})
}

// HireTemplate handles POST /api/v1/agents/templates/{id}/hire: a switched-off copy for review.
// @Summary Hire an agent template
// @Tags Custom Agents
// @Param X-Tenant-ID header string true "Tenant UUID"
// @Param id path string true "Template UUID"
// @Success 201 {object} map[string]interface{}
// @Security bearerAuth
// @Router /agents/templates/{id}/hire [post]
func (h *Handler) HireTemplate(w http.ResponseWriter, r *http.Request) {
	tenantID, err := extractTenantID(r)
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_tenant", "missing or invalid X-Tenant-ID header", nil)
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_id", "invalid id", nil)
		return
	}
	a, err := h.service.Hire(r.Context(), tenantID, id)
	switch {
	case errors.Is(err, ErrNotTemplate):
		httpapi.Error(w, http.StatusNotFound, "not_found", "template not found", nil)
		return
	case err != nil:
		h.logger.Error("hire agent template", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "hire_failed", "failed to hire the template", nil)
		return
	}
	httpapi.JSON(w, http.StatusCreated, a)
}
