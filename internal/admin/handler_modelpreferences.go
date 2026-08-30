package admin

import (
	"errors"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/modelpreferences"
)

type modelPreferenceRequest struct {
	Selector string `json:"selector"`
	Hidden   bool   `json:"hidden"`
}

type deleteModelPreferenceRequest struct {
	Selector string `json:"selector"`
}

// ListModelPreferences handles GET /admin/model-preferences.
//
// @Summary      List model visibility preferences
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Success      200  {array} modelpreferences.Preference
// @Failure      401  {object} core.GatewayError
// @Failure      503  {object} core.GatewayError
// @Router       /admin/model-preferences [get]
func (h *Handler) ListModelPreferences(c *echo.Context) error {
	if h.modelPreferences == nil {
		return handleError(c, featureUnavailableError("model preferences feature is unavailable"))
	}
	preferences := h.modelPreferences.List()
	if preferences == nil {
		preferences = []modelpreferences.Preference{}
	}
	return c.JSON(http.StatusOK, preferences)
}

// UpsertModelPreference handles PUT /admin/model-preferences.
//
// @Summary      Create or update one model visibility preference
// @Tags         admin
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        preference  body      modelPreferenceRequest  true  "Model selector and hidden state"
// @Success      200         {object}  modelpreferences.Preference
// @Failure      400         {object}  core.GatewayError
// @Failure      401         {object}  core.GatewayError
// @Failure      502         {object}  core.GatewayError
// @Failure      503         {object}  core.GatewayError
// @Router       /admin/model-preferences [put]
func (h *Handler) UpsertModelPreference(c *echo.Context) error {
	if h.modelPreferences == nil {
		return handleError(c, featureUnavailableError("model preferences feature is unavailable"))
	}
	var req modelPreferenceRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	preference, err := h.modelPreferences.Upsert(c.Request().Context(), req.Selector, req.Hidden)
	if err != nil {
		return handleError(c, modelPreferenceWriteError(err))
	}
	return c.JSON(http.StatusOK, preference)
}

// DeleteModelPreference handles DELETE /admin/model-preferences.
//
// @Summary      Forget one model visibility preference
// @Tags         admin
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        request  body      deleteModelPreferenceRequest  true  "Model selector to forget"
// @Success      204       "No Content"
// @Failure      400       {object}  core.GatewayError
// @Failure      401       {object}  core.GatewayError
// @Failure      404       {object}  core.GatewayError
// @Failure      503       {object}  core.GatewayError
// @Router       /admin/model-preferences [delete]
func (h *Handler) DeleteModelPreference(c *echo.Context) error {
	if h.modelPreferences == nil {
		return handleError(c, featureUnavailableError("model preferences feature is unavailable"))
	}
	var req deleteModelPreferenceRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	if err := h.modelPreferences.Delete(c.Request().Context(), req.Selector); err != nil {
		if errors.Is(err, modelpreferences.ErrNotFound) {
			return handleError(c, core.NewNotFoundError("model preference not found"))
		}
		return handleError(c, modelPreferenceWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

// ResetModelPreferences handles POST /admin/model-preferences/reset.
//
// @Summary      Reset all model visibility preferences
// @Tags         admin
// @Produce      json
// @Security     BearerAuth
// @Success      204       "No Content"
// @Failure      401       {object}  core.GatewayError
// @Failure      503       {object}  core.GatewayError
// @Router       /admin/model-preferences/reset [post]
func (h *Handler) ResetModelPreferences(c *echo.Context) error {
	if h.modelPreferences == nil {
		return handleError(c, featureUnavailableError("model preferences feature is unavailable"))
	}
	if err := h.modelPreferences.ResetAll(c.Request().Context()); err != nil {
		return handleError(c, modelPreferenceWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

func modelPreferenceWriteError(err error) error {
	if modelpreferences.IsValidationError(err) {
		return core.NewInvalidRequestError(err.Error(), err)
	}
	return core.NewProviderError("model_preferences", http.StatusBadGateway, "model preference storage failed", err)
}
