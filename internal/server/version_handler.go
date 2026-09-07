package server

import (
	"net/http"

	"github.com/airdropia/pgw/internal/version"

	"github.com/labstack/echo/v5"
)

// versionStatus is the GET /version response shape the dashboard's
// versionStore parses (web/dashboard/src/lib/stores/version.svelte.js).
//
// The upstream telemetry beacon (visit cookies, install ids, outbound
// manifest requests) was removed with the versioncheck package in
// plan §16 Stage 3. Until Stage 9/10 lands a lightweight
// GitHub-releases check, latest/update_available stay empty; the
// Settings page simply renders no update notice.
type versionStatus struct {
	App             string `json:"app"`
	Version         string `json:"version"`
	Latest          string `json:"latest,omitempty"`
	UpdateAvailable bool   `json:"update_available"`
	CheckedAt       string `json:"checked_at,omitempty"`
	Enabled         bool   `json:"enabled"`
}

// Version handles GET /version. It answers purely from build metadata;
// there is no network access and no per-visit state.
//
// @Summary      Report the running version
// @Tags         health
// @Produce      json
// @Success      200  {object}  versionStatus
// @Router       /version [get]
func (h *Handler) Version(c *echo.Context) error {
	return c.JSON(http.StatusOK, versionStatus{
		App:     version.App,
		Version: version.Version,
		Enabled: false,
	})
}
