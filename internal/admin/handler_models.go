package admin

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/virtualmodels"
)

type modelAccessResponse struct {
	Selector         string                      `json:"selector"`
	DefaultEnabled   bool                        `json:"default_enabled"`
	EffectiveEnabled bool                        `json:"effective_enabled"`
	UserPaths        []string                    `json:"user_paths,omitempty"`
	Override         *virtualmodels.VirtualModel `json:"override,omitempty"`
}

type modelInventoryResponse struct {
	providers.ModelWithProvider
	Access modelAccessResponse `json:"access"`
}

// ListModels handles GET /admin/models.
// Supports optional category and include_hidden query parameters.
//
// @Summary      List all registered models with provider info and access state
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Param        category        query     string  false  "Filter by model category"
// @Param        include_hidden  query     bool    false  "Include hidden models"
// @Success      200  {array} modelInventoryResponse
// @Failure      400  {object} core.GatewayError
// @Failure      401  {object} core.GatewayError
// @Router       /admin/models [get]
func (h *Handler) ListModels(c *echo.Context) error {
	if h.registry == nil {
		return c.JSON(http.StatusOK, []modelInventoryResponse{})
	}

	cat := core.ModelCategory(strings.TrimSpace(c.QueryParam("category")))
	if cat != "" && cat != core.CategoryAll && !isValidCategory(cat) {
		return handleError(c, core.NewInvalidRequestError("invalid category: "+string(cat), nil))
	}

	includeHidden := false
	if raw := strings.TrimSpace(c.QueryParam("include_hidden")); raw != "" {
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return handleError(c, core.NewInvalidRequestError("invalid include_hidden value, expected true or false", err))
		}
		includeHidden = parsed
	}

	var models []providers.ModelWithProvider
	if cat != "" && cat != core.CategoryAll {
		models = h.registry.ListModelsWithProviderByCategory(cat)
	} else {
		models = h.registry.ListModelsWithProvider()
	}
	if models == nil {
		models = []providers.ModelWithProvider{}
	}

	access := h.modelAccessResolver()
	response := make([]modelInventoryResponse, 0, len(models))
	for _, model := range models {
		selector := strings.TrimSpace(model.Selector)
		if selector == "" {
			selector = strings.TrimSpace(model.ProviderName) + "/" + strings.TrimSpace(model.Model.ID)
		}
		if !includeHidden && h.modelPreferences != nil && h.modelPreferences.IsHidden(selector) {
			continue
		}
		response = append(response, modelInventoryResponse{
			ModelWithProvider: model,
			Access: access(core.ModelSelector{
				Provider: strings.TrimSpace(model.ProviderName),
				Model:    strings.TrimSpace(model.Model.ID),
			}),
		})
	}
	return c.JSON(http.StatusOK, response)
}

// modelAccessResolver returns a function that produces the access view for a
// given selector. When model overrides are configured the resolver consults
// the service for effective state; otherwise every model is reported as
// default-on.
func (h *Handler) modelAccessResolver() func(core.ModelSelector) modelAccessResponse {
	if h.virtualModels == nil {
		return func(selector core.ModelSelector) modelAccessResponse {
			return modelAccessResponse{
				Selector:         selector.QualifiedModel(),
				DefaultEnabled:   true,
				EffectiveEnabled: true,
			}
		}
	}
	return func(selector core.ModelSelector) modelAccessResponse {
		effective := h.virtualModels.EffectiveState(selector)
		access := modelAccessResponse{
			Selector:         effective.Selector,
			DefaultEnabled:   effective.DefaultEnabled,
			EffectiveEnabled: effective.Enabled,
			UserPaths:        append([]string(nil), effective.UserPaths...),
		}
		if override, ok := h.virtualModels.Get(selector.QualifiedModel()); ok && override != nil && !override.IsRedirect() {
			overrideCopy := *override
			access.Override = &overrideCopy
		}
		return access
	}
}

func isValidCategory(cat core.ModelCategory) bool {
	return slices.Contains(core.AllCategories(), cat)
}

// ListCategories handles GET /admin/models/categories.
//
// @Summary      List model categories with counts
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Success      200  {array} providers.CategoryCount
// @Failure      401  {object} core.GatewayError
// @Router       /admin/models/categories [get]
func (h *Handler) ListCategories(c *echo.Context) error {
	if h.registry == nil {
		return c.JSON(http.StatusOK, []providers.CategoryCount{})
	}
	return c.JSON(http.StatusOK, h.registry.GetCategoryCounts())
}

// DashboardConfig handles GET /admin/runtime/config.
//
// @Summary      Get admin runtime configuration
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object} DashboardConfigResponse
// @Failure      401  {object} core.GatewayError
// @Router       /admin/runtime/config [get]
func (h *Handler) DashboardConfig(c *echo.Context) error {
	return c.JSON(http.StatusOK, cloneDashboardRuntimeConfig(h.runtimeConfig))
}
