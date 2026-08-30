// Package codexoauth implements the personal-edition Codex / ChatGPT
// subscription login flow.
//
// The flow is the standard OAuth 2.0 authorization-code variant with PKCE
// the upstream Codex CLI uses, parameterised so issuer, client id, and
// redirect URI can be overridden for tests and private deployments.
//
// Single-account scope (Stage 7 of the personal-edition plan): one
// connection per (provider name, account id) tuple, refresh on demand, and
// no automatic 401 retry on the request path. The latter is a deliberate
// deferral: chatgpt-provider integration with on-request refresh needs an
// llmclient middleware hook the upstream package does not yet expose.
//
// The package is read-only with respect to the operator's local Codex
// installation: it does not touch CODEX_HOME/auth.json or any other file
// under the Codex CLI. The login flow lives entirely inside the personal
// gateway and stores its result in the dedicated SQLite / MongoDB
// connection table the gateway already owns.
package codexoauth

import (
	"context"
	"errors"
)

// IssuerOverride is the only Codex-specific knob exposed for testing. The
// personal binary uses the production issuer at runtime; tests inject a
// httptest server URL instead.
const (
	ProductionIssuer = "https://auth.openai.com"
	ProductionClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// DefaultListenPort is the upstream Codex CLI default. The Stage7 personal
// gateway does not run its own localhost listener (operators paste the
// redirect URL back into the dashboard) but the value is preserved so a
// future callback server can pick it up without a config change.
const DefaultListenPort = 1455

// DefaultCallbackPath is the path the upstream Codex CLI mounts the
// callback handler under.
const DefaultCallbackPath = "/auth/callback"

// AuthorizeParams carries the inputs the operator passes when starting a
// login flow. Empty fields fall back to the production constants so the
// admin endpoint can issue a one-liner call.
type AuthorizeParams struct {
	Issuer     string
	ClientID   string
	ListenPort int
	State      string // optional; "" generates one
}

// PendingFlow tracks an in-progress login: the state nonce the dashboard
// must echo back, the PKCE verifier the callback exchange will present,
// and the listener URL the dashboard opens. The personal gateway stores
// the pending flow in memory only; on restart the operator starts a new
// login.
type PendingFlow struct {
	State          string
	CodeVerifier   string
	AuthURL        string
	IssuedAt       int64 // unix seconds; flows older than ~10 minutes are stale
	ProviderName   string
}

// Connection is one stored Codex subscription. The provider name is the
// gateway-side name the operator configures (default "chatgpt"); the
// account id is the ChatGPT subscription id the JWT carries.
type Connection struct {
	ID              int64
	ProviderName    string
	AccountID       string
	Email           string
	Plan            string
	AccessToken     string
	RefreshToken    string
	IDToken         string
	AccessExpiresAt int64 // unix seconds
	LastRefreshAt   int64 // unix seconds
	CreatedAt       int64
	UpdatedAt       int64
	Status          string // active | needs_relogin | refresh_failed
}

// ErrNoConnection is returned when an operation requires an existing
// connection but the store has none under the requested provider name.
var ErrNoConnection = errors.New("codex oauth: no connection stored for this provider")

// ErrInvalidState is returned when a callback arrives with a state nonce
// that does not match the pending flow. The admin endpoint translates
// this into a 400 so the dashboard can show "login expired, try again".
var ErrInvalidState = errors.New("codex oauth: callback state does not match pending flow")

// ErrTokenExchange is returned when the token endpoint rejects an
// authorization code, refresh, or revocation request. The admin endpoint
// surfaces the wrapped error message so the operator sees the upstream
// reason rather than a generic 502.
var ErrTokenExchange = errors.New("codex oauth: token endpoint rejected the request")

// TokenClient is the narrow HTTP surface the OAuth flow needs from the
// rest of the gateway. Tests substitute an httptest.Server-backed
// implementation; production wires the default net/http client through
// NewTokenClient.
type TokenClient interface {
	ExchangeCode(ctx context.Context, req CodeExchangeRequest) (*CodeExchangeResponse, error)
	Refresh(ctx context.Context, req RefreshRequest) (*RefreshResponse, error)
}

// CodeExchangeRequest carries the parameters of the authorization-code to
// token exchange.
type CodeExchangeRequest struct {
	Issuer       string
	ClientID     string
	Code         string
	RedirectURI  string
	CodeVerifier string
}

// CodeExchangeResponse mirrors the OAuth 2.0 token endpoint body Codex
// returns. ExpiresIn is in seconds; the store converts it to an absolute
// expires-at timestamp.
type CodeExchangeResponse struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int64
	AccountID    string
	Email        string
	Plan         string
}

// RefreshRequest carries the parameters of a refresh-token grant.
type RefreshRequest struct {
	Issuer       string
	ClientID     string
	RefreshToken string
}

// RefreshResponse mirrors Codex's refresh response body.
type RefreshResponse struct {
	AccessToken  string
	RefreshToken string
	IDToken      string
	ExpiresIn    int64
	AccountID    string
}