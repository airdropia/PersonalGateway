package admin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/codeximport"
	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/providers"
)

// defaultCodexProviderName is the personal-edition convention: the import
// endpoint always registers under the "chatgpt" provider name. An admin
// who has already configured the same provider under a different name can
// call this endpoint, see the 409 error, and rename before retrying.
const defaultCodexProviderName = "chatgpt"

// importCodexAuthRequest is the optional override body. When empty the
// handler searches CODEX_HOME and the platform default.
type importCodexAuthRequest struct {
	CodexHome string `json:"codex_home"`
}

// importCodexAuthResponse is the success body. It carries the parsed
// metadata that the dashboard renders, never the tokens themselves.
type importCodexAuthResponse struct {
	Provider     string         `json:"provider"`
	AccountID    string         `json:"account_id"`
	Email        string         `json:"email"`
	Plan         string         `json:"plan"`
	NeedsRelogin bool           `json:"needs_relogin"`
	LastRefresh  string         `json:"last_refresh,omitempty"`
	ExpiresAt    string         `json:"access_expires_at,omitempty"`
	Metadata     map[string]any `json:"metadata"`
}

// ImportCodexAuth handles POST /admin/providers/chatgpt/import-codex.
//
// The endpoint reads the local Codex CLI auth.json (CODEX_HOME/auth.json
// or ~/.codex/auth.json, %USERPROFILE%\.codex\auth.json on Windows),
// validates the access token against the documented JWT shape, and
// persists the result through the provider credential service so the
// existing chatgpt provider starts serving requests against the same
// Codex backend the CLI itself uses.
//
// The response deliberately omits the access, refresh, and id tokens: the
// dashboard must never echo credentials, even in logs. The account id,
// email, plan, last-refresh, and access-expiry timestamps are enough for
// the UI to render a "logged in as user@example.com, plan Pro, expires
// in 2h" summary and a "re-login required" banner when the token is
// stale.
func (h *Handler) ImportCodexAuth(c *echo.Context) error {
	if h.providerCredentials == nil {
		return handleError(c, featureUnavailableError("provider credentials feature is unavailable"))
	}

	var req importCodexAuthRequest
	// A missing or empty body is acceptable: the operator is asking the
	// server to discover the file. Only an explicit, well-formed override
	// goes through Bind.
	if c.Request().ContentLength > 0 {
		if err := c.Bind(&req); err != nil {
			return handleError(c, core.NewInvalidRequestError("invalid request body: "+err.Error(), err))
		}
	}

	data, err := readCodexAuthFile(req.CodexHome)
	if err != nil {
		switch {
		case errors.Is(err, codeximport.ErrAuthFileNotFound):
			return handleError(c, core.NewInvalidRequestError(
				"no Codex auth.json found; run `codex login` first or set CODEX_HOME",
				err,
			))
		case errors.Is(err, codeximport.ErrUnsupportedAuthMode):
			return handleError(c, core.NewInvalidRequestError(
				"Codex auth.json is not in chatgpt mode; the personal gateway imports ChatGPT subscriptions only",
				err,
			))
		case errors.Is(err, codeximport.ErrMissingAccessToken):
			return handleError(c, core.NewInvalidRequestError(
				"Codex auth.json has no access_token; run `codex login` again",
				err,
			))
		default:
			return handleError(c, core.NewProviderError("codex_import", http.StatusBadGateway, "codex import failed", err))
		}
	}

	conn, err := codeximport.Parse(data)
	if err != nil {
		return handleError(c, core.NewProviderError("codex_import", http.StatusBadGateway, "codex import failed", err))
	}

	if h.providerCredentials.IsManaged(defaultCodexProviderName) {
		return handleError(c, core.NewInvalidRequestError(
			"provider "+defaultCodexProviderName+" is managed by config/env and is read-only",
			nil,
		).WithParam("name"))
	}

	if !slices.Contains(h.providerCredentials.RegisteredTypes(), "chatgpt") {
		return handleError(c, core.NewInvalidRequestError(
			"chatgpt provider type is not registered; the personal binary was built without it",
			nil,
		))
	}

	cred := providers.ManagedProviderCredential{
		Name:    defaultCodexProviderName,
		Type:    "chatgpt",
		APIKeys: []string{conn.AccessToken},
		Enabled: true,
	}
	h.mutationMu.Lock()
	defer h.mutationMu.Unlock()
	if err := h.providerCredentials.Upsert(c.Request().Context(), cred); err != nil {
		return handleError(c, providerCredentialWriteError(err))
	}

	resp := importCodexAuthResponse{
		Provider:     defaultCodexProviderName,
		AccountID:    conn.AccountID,
		Email:        conn.Email,
		Plan:         conn.Plan,
		NeedsRelogin: conn.NeedsRelogin,
		LastRefresh:  formatTime(conn.LastRefresh),
		ExpiresAt:    formatTime(conn.AccessExpiresAt),
		Metadata:     conn.Metadata(),
	}
	return c.JSON(http.StatusOK, resp)
}

// readCodexAuthFile returns the raw bytes of the Codex auth.json, honouring
// an explicit override from the request body, falling back to the
// platform-discovery path.
func readCodexAuthFile(override string) ([]byte, error) {
	if home := strings.TrimSpace(override); home != "" {
		path, err := safeJoinCodexHome(home)
		if err != nil {
			return nil, err
		}
		return os.ReadFile(path)
	}
	_, data, err := codeximport.DiscoverAuthFile()
	return data, err
}

// safeJoinCodexHome rejects path separators and traversal escapes in the
// caller-supplied CODEX_HOME so the admin endpoint cannot be tricked into
// reading arbitrary files.
func safeJoinCodexHome(home string) (string, error) {
	cleaned := filepath.Clean(home)
	if cleaned == "" || strings.ContainsAny(cleaned, "\x00") {
		return "", core.NewInvalidRequestError("invalid codex_home path", nil).WithParam("codex_home")
	}
	return filepath.Join(cleaned, "auth.json"), nil
}

// formatTime renders a timestamp as RFC3339 or returns the empty string
// for the zero value, keeping optional JSON fields clean.
func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}