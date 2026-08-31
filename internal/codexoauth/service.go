package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"golang.org/x/sync/singleflight"
)

// Service is the personal-edition OAuth state machine. It owns the
// in-memory pending flow (one at a time per process) and exposes a small
// surface the admin handler drives:
//
//	StartLogin -> returns the auth URL and stores the pending flow.
//	CompleteLogin(code, state) -> exchanges the code, persists the
//	                              connection, and clears the pending flow.
//	Connection(provider) -> loads the stored connection.
//	RefreshIfNeeded(provider) -> refreshes when expiry is within the
//	                            configured skew.
//	Forget(provider) -> deletes the stored connection.
//
// The pending flow is intentionally process-local: a restart drops it,
// and the operator restarts the login. The singleflight Group collapses
// concurrent refresh attempts on the same provider into one upstream
// call, satisfying plan §9.4 "Refresh at most once concurrently per
// connection" without serializing unrelated requests.
type Service struct {
	store  Store
	client TokenClient

	// Configuration knobs. Defaults are production constants; tests
	// override them to point at httptest servers.
	Issuer     string
	ClientID   string
	ListenPort int

	// Skew is the buffer subtracted from the access token's expiry when
	// deciding whether to refresh. The Codex tokens default to one hour;
	// a 60-second skew is conservative without being wasteful.
	Skew time.Duration

	mu      sync.Mutex
	pending *PendingFlow

	flight singleflight.Group
}

// NewService wires the OAuth service against the supplied store and
// HTTP client. Production callers pass an *HTTPTokenClient; tests pass a
// httptest-backed TokenClient implementation.
func NewService(store Store, client TokenClient) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if client == nil {
		return nil, fmt.Errorf("token client is required")
	}
	return &Service{
		store:     store,
		client:    client,
		Issuer:    ProductionIssuer,
		ClientID:  ProductionClientID,
		ListenPort: DefaultListenPort,
		Skew:      60 * time.Second,
	}, nil
}

// StartLogin generates a PKCE pair, an OAuth state nonce, and returns the
// authorization URL the dashboard opens. The pending flow is stored
// process-locally so the callback endpoint can validate the state.
func (s *Service) StartLogin(ctx context.Context, providerName string) (*PendingFlow, error) {
	if providerName == "" {
		return nil, fmt.Errorf("provider name is required")
	}
	pkce, err := GeneratePKCE()
	if err != nil {
		return nil, fmt.Errorf("generate PKCE: %w", err)
	}
	state, err := GenerateState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}
	authURL, err := BuildAuthorizeURL(AuthorizeParams{
		Issuer:     s.Issuer,
		ClientID:   s.ClientID,
		ListenPort: s.ListenPort,
		State:      state,
	}, pkce)
	if err != nil {
		return nil, fmt.Errorf("build authorize URL: %w", err)
	}

	s.mu.Lock()
	s.pending = &PendingFlow{
		State:        state,
		CodeVerifier: pkce.Verifier,
		AuthURL:      authURL,
		IssuedAt:     time.Now().Unix(),
		ProviderName: providerName,
	}
	s.mu.Unlock()

	return s.pending, nil
}

// CompleteLogin exchanges the callback's authorization code, persists
// the resulting connection, and clears the pending flow.
func (s *Service) CompleteLogin(ctx context.Context, providerName, callbackURL string) (*Connection, error) {
	code, state, err := ParseCallback(callbackURL)
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()

	if pending == nil {
		return nil, ErrInvalidState
	}
	if pending.State != state || pending.ProviderName != providerName {
		return nil, ErrInvalidState
	}

	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", s.ListenPort, DefaultCallbackPath)
	resp, err := s.client.ExchangeCode(ctx, CodeExchangeRequest{
		Issuer:       s.Issuer,
		ClientID:     s.ClientID,
		Code:         code,
		RedirectURI:  redirectURI,
		CodeVerifier: pending.CodeVerifier,
	})
	if err != nil {
		return nil, err
	}

	accountID := resp.AccountID
	if accountID == "" {
		accountID = jwtStringClaim(resp.AccessToken, "https://api.openai.com/auth", "chatgpt_account_id")
	}
	email := resp.Email
	if email == "" {
		email = jwtStringClaim(resp.AccessToken, "email", "")
	}
	plan := resp.Plan
	if plan == "" {
		plan = planFromTokenClaims(resp.IDToken, resp.AccessToken)
	}
	conn := Connection{
		ProviderName:    providerName,
		AccountID:       accountID,
		Email:           email,
		Plan:            plan,
		AccessToken:     resp.AccessToken,
		RefreshToken:    resp.RefreshToken,
		IDToken:         resp.IDToken,
		AccessExpiresAt: time.Now().Unix() + resp.ExpiresIn,
		LastRefreshAt:   time.Now().Unix(),
		Status:          "active",
	}
	if conn.RefreshToken == "" {
		conn.RefreshToken = resp.RefreshToken
	}
	if err := s.store.Upsert(ctx, conn); err != nil {
		return nil, fmt.Errorf("persist connection: %w", err)
	}
	return &conn, nil
}

// Connection returns the stored connection for a provider name, or
// ErrNotFound when none exists.
func (s *Service) Connection(ctx context.Context, providerName string) (*Connection, error) {
	conn, err := s.store.GetByProvider(ctx, providerName)
	if errors.Is(err, ErrNotFound) {
		return nil, ErrNoConnection
	}
	return conn, err
}

// Forget deletes the stored connection.
func (s *Service) Forget(ctx context.Context, providerName string) error {
	err := s.store.Delete(ctx, providerName)
	if errors.Is(err, ErrNotFound) {
		return ErrNoConnection
	}
	return err
}

// RefreshIfNeeded returns the current access token when it has not yet
// expired; otherwise it refreshes via the token endpoint under a
// singleflight guard. The returned token is the fresh access token the
// caller should use immediately. The stored connection is updated in
// place and the new expiry is reflected.
//
// Plan §9.4 calls for "Refresh shortly before expiry" and "Refresh at
// most once concurrently per connection". The singleflight.Group
// guarantees the latter; the Skew field on the service guarantees the
// former. Concurrent callers share the result of one upstream call.
func (s *Service) RefreshIfNeeded(ctx context.Context, providerName string) (string, error) {
	conn, err := s.store.GetByProvider(ctx, providerName)
	if errors.Is(err, ErrNotFound) {
		return "", ErrNoConnection
	}
	if err != nil {
		return "", err
	}
	if !needsRefresh(conn.AccessExpiresAt, s.Skew) {
		return conn.AccessToken, nil
	}

	key := "codex-oauth-refresh:" + providerName
	v, err, _ := s.flight.Do(key, func() (any, error) {
		// Re-read inside the singleflight so a second caller after the
		// first one wins does not trigger an unnecessary refresh.
		latest, lookupErr := s.store.GetByProvider(ctx, providerName)
		if lookupErr != nil {
			return "", lookupErr
		}
		if !needsRefresh(latest.AccessExpiresAt, s.Skew) {
			return latest.AccessToken, nil
		}
		return s.doRefresh(ctx, latest)
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// doRefresh performs the upstream refresh-token grant and writes the
// new connection back to the store. On permanent failure (refresh token
// rejected by the issuer) the stored connection is marked
// needs_relogin so the dashboard can prompt the operator to start over.
func (s *Service) doRefresh(ctx context.Context, conn *Connection) (string, error) {
	resp, err := s.client.Refresh(ctx, RefreshRequest{
		Issuer:       s.Issuer,
		ClientID:     s.ClientID,
		RefreshToken: conn.RefreshToken,
	})
	if err != nil {
		// Permanent rejection: rotate the refresh token cannot proceed.
		// Mark the stored row so the dashboard flags it.
		conn.Status = "needs_relogin"
		if writeErr := s.store.Upsert(ctx, *conn); writeErr != nil {
			return "", fmt.Errorf("refresh failed and status update also failed: %w (status update: %v)", err, writeErr)
		}
		return "", fmt.Errorf("refresh rejected by token endpoint: %w", err)
	}
	conn.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		conn.RefreshToken = resp.RefreshToken
	}
	conn.IDToken = resp.IDToken
	if resp.AccountID != "" {
		conn.AccountID = resp.AccountID
	}
	conn.AccessExpiresAt = time.Now().Unix() + resp.ExpiresIn
	conn.LastRefreshAt = time.Now().Unix()
	conn.Status = "active"
	if err := s.store.Upsert(ctx, *conn); err != nil {
		return "", fmt.Errorf("persist refreshed connection: %w", err)
	}
	return conn.AccessToken, nil
}

// needsRefresh reports whether the access token will expire within skew.
func needsRefresh(expiresAtUnix int64, skew time.Duration) bool {
	if expiresAtUnix == 0 {
		return true
	}
	now := time.Now().Unix()
	return expiresAtUnix-now <= int64(skew.Seconds())
}

// planFromTokenClaims checks the id_token profile first, then the access
// token's chatgpt_plan_type, mirroring the precedence Codex uses for the
// billing dashboard.
func planFromTokenClaims(idToken, accessToken string) string {
	if plan := jwtStringClaim(idToken, "https://api.openai.com/profile", "plan_type"); plan != "" {
		return plan
	}
	return jwtStringClaim(accessToken, "chatgpt_plan_type", "")
}

// jwtStringClaim pulls a string claim from a JWT. Returns "" on any
// parse error or missing claim. The Service uses the same parser the
// token arrived via the production HTTP path or a test fake.
// Helpers (jwtStringClaim, stringFromJSON, decodeBase64URL) live in
// client.go since they are package-private and the HTTP client needs
// them too. The Service reuses them via the same package.