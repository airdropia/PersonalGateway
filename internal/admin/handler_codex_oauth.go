package admin

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/codexoauth"
	"github.com/enterpilot/gomodel/internal/core"
)

// codexOAuthStartRequest is the body the dashboard sends to begin a
// login. The default provider name is "chatgpt"; the personal edition
// does not need to override it under normal operation.
type codexOAuthStartRequest struct {
	ProviderName string `json:"provider_name"`
}

// codexOAuthStartResponse carries the auth URL the dashboard opens and
// the state nonce it must echo back. The state nonce is not signed; it
// only needs to match the pending flow in the service.
type codexOAuthStartResponse struct {
	AuthURL      string `json:"auth_url"`
	State        string `json:"state"`
	ProviderName string `json:"provider_name"`
	IssuedAt     int64  `json:"issued_at"`
	ExpiresAt    int64  `json:"expires_at"`
}

// codexOAuthCallbackRequest is the body the dashboard sends after the
// operator copies the redirect URL out of their browser. The dashboard
// never receives the code directly; the operator pastes the whole URL.
type codexOAuthCallbackRequest struct {
	ProviderName string `json:"provider_name"`
	CallbackURL  string `json:"callback_url"`
}

// codexOAuthConnectionResponse is the public projection of a stored
// Codex OAuth connection. Token values are deliberately omitted.
type codexOAuthConnectionResponse struct {
	ProviderName    string `json:"provider_name"`
	AccountID       string `json:"account_id"`
	Email           string `json:"email"`
	Plan            string `json:"plan"`
	Status          string `json:"status"`
	AccessExpiresAt int64  `json:"access_expires_at"`
	LastRefreshAt   int64  `json:"last_refresh_at"`
}

// StartCodexOAuth handles POST /admin/providers/chatgpt/oauth/start.
//
// The endpoint generates a PKCE pair and a state nonce, stores the
// pending flow in the service, and returns the authorization URL the
// dashboard opens in a new tab. The callback listener is intentionally
// absent in Stage 7: the operator pastes the redirect URL back into the
// dashboard, which posts it to CompleteCodexOAuth. This sidesteps the
// browser-opening and port-management complexity on Windows while
// keeping the OAuth flow usable.
func (h *Handler) StartCodexOAuth(c *echo.Context) error {
	if h.codexOAuth == nil {
		return handleError(c, featureUnavailableError("codex oauth feature is unavailable"))
	}
	var req codexOAuthStartRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	providerName := strings.TrimSpace(req.ProviderName)
	if providerName == "" {
		providerName = "chatgpt"
	}

	pending, err := h.codexOAuth.StartLogin(c.Request().Context(), providerName)
	if err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	issuedAt := time.Unix(pending.IssuedAt, 0)
	return c.JSON(http.StatusOK, codexOAuthStartResponse{
		AuthURL:      pending.AuthURL,
		State:        pending.State,
		ProviderName: pending.ProviderName,
		IssuedAt:     pending.IssuedAt,
		ExpiresAt:    issuedAt.Add(10 * time.Minute).Unix(),
	})
}

// CompleteCodexOAuth handles POST /admin/providers/chatgpt/oauth/callback.
//
// The dashboard posts the redirect URL the operator copied from their
// browser. The service validates the state nonce, exchanges the code
// for tokens, and persists the resulting connection. The endpoint
// returns the public projection: account id, email, plan, status, and
// expiry — never the access or refresh tokens.
func (h *Handler) CompleteCodexOAuth(c *echo.Context) error {
	if h.codexOAuth == nil {
		return handleError(c, featureUnavailableError("codex oauth feature is unavailable"))
	}
	var req codexOAuthCallbackRequest
	if err := c.Bind(&req); err != nil {
		return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
	}
	providerName := strings.TrimSpace(req.ProviderName)
	if providerName == "" {
		providerName = "chatgpt"
	}
	conn, err := h.codexOAuth.CompleteLogin(c.Request().Context(), providerName, req.CallbackURL)
	if err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	return c.JSON(http.StatusOK, projectCodexConnection(conn))
}

// GetCodexOAuthStatus handles GET /admin/providers/chatgpt/oauth/status.
//
// Returns the public projection of the stored connection when one
// exists, or a 404 when the operator has never logged in.
func (h *Handler) GetCodexOAuthStatus(c *echo.Context) error {
	if h.codexOAuth == nil {
		return handleError(c, featureUnavailableError("codex oauth feature is unavailable"))
	}
	providerName := strings.TrimSpace(c.QueryParam("provider_name"))
	if providerName == "" {
		providerName = "chatgpt"
	}
	conn, err := h.codexOAuth.Connection(c.Request().Context(), providerName)
	if err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	return c.JSON(http.StatusOK, projectCodexConnection(conn))
}

// RefreshCodexOAuth handles POST /admin/providers/chatgpt/oauth/refresh.
//
// Manual refresh path: when the dashboard suspects the token has
// expired (e.g. after an upstream 401 surfaced as a chat failure) it
// can call this endpoint to force a refresh round-trip. The response
// shape matches GetCodexOAuthStatus.
func (h *Handler) RefreshCodexOAuth(c *echo.Context) error {
	if h.codexOAuth == nil {
		return handleError(c, featureUnavailableError("codex oauth feature is unavailable"))
	}
	providerName := strings.TrimSpace(c.QueryParam("provider_name"))
	if providerName == "" {
		providerName = "chatgpt"
	}
	if _, err := h.codexOAuth.RefreshIfNeeded(c.Request().Context(), providerName); err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	conn, err := h.codexOAuth.Connection(c.Request().Context(), providerName)
	if err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	return c.JSON(http.StatusOK, projectCodexConnection(conn))
}

// ForgetCodexOAuth handles POST /admin/providers/chatgpt/oauth/forget.
//
// Drops the stored connection and clears any in-flight pending flow.
// The provider remains registered; only the OAuth credentials are gone,
// so the operator can re-run StartCodexOAuth without restarting.
func (h *Handler) ForgetCodexOAuth(c *echo.Context) error {
	if h.codexOAuth == nil {
		return handleError(c, featureUnavailableError("codex oauth feature is unavailable"))
	}
	providerName := strings.TrimSpace(c.QueryParam("provider_name"))
	if providerName == "" {
		providerName = "chatgpt"
	}
	if err := h.codexOAuth.Forget(c.Request().Context(), providerName); err != nil {
		return handleError(c, codexOAuthWriteError(err))
	}
	return c.NoContent(http.StatusNoContent)
}

// projectCodexConnection strips the token values from a stored
// connection before it leaves the gateway. The dashboard sees only the
// metadata it needs to render the connection card.
func projectCodexConnection(c *codexoauth.Connection) codexOAuthConnectionResponse {
	return codexOAuthConnectionResponse{
		ProviderName:    c.ProviderName,
		AccountID:       c.AccountID,
		Email:           c.Email,
		Plan:            c.Plan,
		Status:          c.Status,
		AccessExpiresAt: c.AccessExpiresAt,
		LastRefreshAt:   c.LastRefreshAt,
	}
}

// codexOAuthWriteError converts an OAuth-layer error into an admin
// error response with the right status code.
func codexOAuthWriteError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, codexoauth.ErrInvalidState):
		return core.NewInvalidRequestError("login expired or state mismatch; restart the OAuth flow", err)
	case errors.Is(err, codexoauth.ErrNoConnection):
		return core.NewNotFoundError("codex oauth connection")
	case errors.Is(err, codexoauth.ErrTokenExchange):
		return core.NewProviderError("codex_oauth", http.StatusBadGateway, "codex token endpoint rejected the request", err)
	case errors.Is(err, codexoauth.ErrNotFound):
		return core.NewNotFoundError("codex oauth connection")
	default:
		return core.NewProviderError("codex_oauth", http.StatusBadGateway, "codex oauth error", err)
	}
}