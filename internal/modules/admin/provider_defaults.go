package admin

import (
	"context"
	"net/http"

	authclient "github.com/Bengo-Hub/shared-auth-client"
	"github.com/bengobox/marketflow-api/internal/ent/providersetting"
	"github.com/bengobox/marketflow-api/internal/httpapi"
	"go.uber.org/zap"
)

// ProviderGroup is a set of providers that do the same job. Exactly one is the group's default,
// set with "Set as default" in Platform > Providers and read by every service that uses the job
// (marketflow-ai reads provider_defaults/platform through /admin/providers/resolve). Members are
// listed in fallback order: with no default saved, the first configured member answers.
type ProviderGroup struct {
	ID      string   `json:"id"`
	Label   string   `json:"label"`
	Members []string `json:"members"`
	// Type is the ProviderSetting type whose legacy "_preferred" row mirrors the default.
	Type string `json:"type,omitempty"`
}

// ProviderGroups are the platform's provider groups.
var ProviderGroups = []ProviderGroup{
	{ID: "web_search", Label: "Web search", Members: []string{"tavily", "vertex_search"}},
	{ID: "llm", Label: "Language models", Members: []string{"groq", "anthropic", "openai"}, Type: "ai_llm"},
	{ID: "embeddings", Label: "Embeddings and local model", Members: []string{"ollama"}},
	{ID: "decisions", Label: "Typed decisions", Members: []string{"typesafe"}},
	{ID: "image", Label: "Image generation", Members: []string{"fal", "higgsfield"}},
	{ID: "video", Label: "Video generation", Members: []string{"higgsfield", "fal"}},
	{ID: "voice", Label: "Voice", Members: []string{"elevenlabs", "fal"}},
	{ID: "video_assembly", Label: "Video templates", Members: []string{"creatomate"}},
}

// defaultsType and defaultsName key the stored defaults: one row per group, key = group id.
const (
	defaultsType = "provider_defaults"
	defaultsName = "platform"
)

func groupByID(id string) (ProviderGroup, bool) {
	for _, g := range ProviderGroups {
		if g.ID == id {
			return g, true
		}
	}
	return ProviderGroup{}, false
}

// GroupDefault returns a group's default: the saved one if it is still a member, else the first
// member.
func GroupDefault(g ProviderGroup, saved map[string]string) string {
	if v := saved[g.ID]; v != "" {
		for _, m := range g.Members {
			if m == v {
				return v
			}
		}
	}
	return g.Members[0]
}

func (h *PlatformProviderHandler) savedDefaults(ctx context.Context) map[string]string {
	rows, err := h.client.ProviderSetting.Query().
		Where(providersetting.TenantIDEQ(platformTenantID), providersetting.ProviderTypeEQ(defaultsType),
			providersetting.ProviderNameEQ(defaultsName), providersetting.IsActiveEQ(true)).All(ctx)
	out := map[string]string{}
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Key] = r.Value
	}
	return out
}

// ListProviderDefaults handles GET /admin/providers/defaults: every group with its members and
// its default.
// @Summary Provider groups and their defaults
// @Tags Admin - Providers
// @Produce json
// @Success 200 {object} map[string]interface{}
// @Security bearerAuth
// @Router /admin/providers/defaults [get]
func (h *PlatformProviderHandler) ListProviderDefaults(w http.ResponseWriter, r *http.Request) {
	saved := h.savedDefaults(r.Context())
	type row struct {
		ProviderGroup
		Default string `json:"default"`
	}
	out := make([]row, 0, len(ProviderGroups))
	for _, g := range ProviderGroups {
		out = append(out, row{ProviderGroup: g, Default: GroupDefault(g, saved)})
	}
	httpapi.JSON(w, http.StatusOK, map[string]any{"groups": out})
}

type setDefaultRequest struct {
	Group    string `json:"group"`
	Provider string `json:"provider"`
}

// SetProviderDefault handles POST /admin/providers/defaults {group, provider}: makes provider
// the group's one default. Only that group's row changes.
// @Summary Set a provider group's default
// @Tags Admin - Providers
// @Accept json
// @Produce json
// @Param request body setDefaultRequest true "Group and provider"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Security bearerAuth
// @Router /admin/providers/defaults [post]
func (h *PlatformProviderHandler) SetProviderDefault(w http.ResponseWriter, r *http.Request) {
	var req setDefaultRequest
	if err := httpapi.DecodeJSON(r, &req); err != nil {
		httpapi.Error(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	g, ok := groupByID(req.Group)
	if !ok {
		httpapi.Error(w, http.StatusBadRequest, "unknown_group", "no such provider group", nil)
		return
	}
	member := false
	for _, m := range g.Members {
		member = member || m == req.Provider
	}
	if !member {
		httpapi.Error(w, http.StatusBadRequest, "not_a_member", req.Provider+" is not in the "+g.Label+" group", nil)
		return
	}
	ctx := r.Context()
	if err := h.upsertSetting(ctx, defaultsType, defaultsName, g.ID, req.Provider); err != nil {
		h.logger.Error("set provider default", zap.Error(err))
		httpapi.Error(w, http.StatusInternalServerError, "save_failed", "could not save the default", nil)
		return
	}
	if g.Type != "" {
		// The legacy per-type preference (providers.Manager.getPreferred) reads the same choice.
		if err := h.upsertSetting(ctx, g.Type, req.Provider, "_preferred", req.Provider); err != nil {
			h.logger.Warn("mirror _preferred", zap.Error(err))
		} else {
			_, _ = h.client.ProviderSetting.Delete().Where(providersetting.TenantIDEQ(platformTenantID),
				providersetting.ProviderTypeEQ(g.Type), providersetting.KeyEQ("_preferred"),
				providersetting.ProviderNameNEQ(req.Provider)).Exec(ctx)
		}
	}
	h.logger.Info("provider default set", zap.String("group", g.ID), zap.String("provider", req.Provider), zap.String("by", actorOf(r)))
	httpapi.JSON(w, http.StatusOK, map[string]any{"group": g.ID, "default": req.Provider})
}

func actorOf(r *http.Request) string {
	if c, ok := authclient.ClaimsFromContext(r.Context()); ok && c != nil {
		if c.Email != "" {
			return c.Email
		}
		return c.Subject
	}
	return "system"
}

// upsertSetting writes one plain (not secret) platform setting row.
func (h *PlatformProviderHandler) upsertSetting(ctx context.Context, ptype, pname, key, value string) error {
	n, err := h.client.ProviderSetting.Update().
		Where(providersetting.TenantIDEQ(platformTenantID), providersetting.ProviderTypeEQ(ptype),
			providersetting.ProviderNameEQ(pname), providersetting.KeyEQ(key), providersetting.EnvironmentEQ("production")).
		SetValue(value).SetIsActive(true).Save(ctx)
	if err != nil || n > 0 {
		return err
	}
	return h.client.ProviderSetting.Create().
		SetTenantID(platformTenantID).SetProviderType(ptype).SetProviderName(pname).SetEnvironment("production").
		SetKey(key).SetValue(value).SetIsPlatform(true).SetIsPlatformManaged(true).
		SetIsActive(true).SetStatus("active").Exec(ctx)
}
