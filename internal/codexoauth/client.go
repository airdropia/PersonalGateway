package codexoauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// HTTPTokenClient is the production TokenClient. It is split into its own
// type so tests can substitute an httptest.Server-backed implementation
// without touching the OAuth service.
type HTTPTokenClient struct {
	Client    *http.Client
	UserAgent string
	Timeout   time.Duration
}

// NewHTTPTokenClient returns a TokenClient that talks to the production
// Codex token endpoint with a sensible default timeout.
func NewHTTPTokenClient(httpClient *http.Client) *HTTPTokenClient {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &HTTPTokenClient{
		Client:    httpClient,
		UserAgent: "personal-gateway/1 (+codex-oauth)",
		Timeout:   30 * time.Second,
	}
}

// ExchangeCode performs the authorization-code-to-token exchange against
// the Codex issuer.
func (c *HTTPTokenClient) ExchangeCode(ctx context.Context, req CodeExchangeRequest) (*CodeExchangeResponse, error) {
	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("client_id", req.ClientID)
	body.Set("code", req.Code)
	body.Set("redirect_uri", req.RedirectURI)
	body.Set("code_verifier", req.CodeVerifier)

	var raw tokenResponse
	if err := c.postForm(ctx, tokenEndpoint(req.Issuer), body, &raw); err != nil {
		return nil, err
	}
	out := &CodeExchangeResponse{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		IDToken:      raw.IDToken,
		ExpiresIn:    raw.ExpiresIn,
		AccountID:    jwtStringClaim(raw.AccessToken, "https://api.openai.com/auth", "chatgpt_account_id"),
		Email:        jwtStringClaim(raw.AccessToken, "email", ""),
		Plan:         planFromToken(raw),
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%w: token endpoint returned no access_token", ErrTokenExchange)
	}
	return out, nil
}

// Refresh performs a refresh-token grant.
func (c *HTTPTokenClient) Refresh(ctx context.Context, req RefreshRequest) (*RefreshResponse, error) {
	body := url.Values{}
	body.Set("grant_type", "refresh_token")
	body.Set("client_id", req.ClientID)
	body.Set("refresh_token", req.RefreshToken)

	var raw tokenResponse
	if err := c.postForm(ctx, tokenEndpoint(req.Issuer), body, &raw); err != nil {
		return nil, err
	}
	out := &RefreshResponse{
		AccessToken:  raw.AccessToken,
		RefreshToken: raw.RefreshToken,
		IDToken:      raw.IDToken,
		ExpiresIn:    raw.ExpiresIn,
		AccountID:    jwtStringClaim(raw.AccessToken, "https://api.openai.com/auth", "chatgpt_account_id"),
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("%w: token endpoint returned no access_token", ErrTokenExchange)
	}
	return out, nil
}

// tokenEndpoint returns the absolute token URL for an issuer. The
// production issuer serves /oauth/token; tests inject an httptest URL
// where the same path applies.
func tokenEndpoint(issuer string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(issuer), "/")
	if trimmed == "" {
		trimmed = ProductionIssuer
	}
	return trimmed + "/oauth/token"
}

// postForm sends a form-encoded POST and decodes the response.
func (c *HTTPTokenClient) postForm(ctx context.Context, url string, form url.Values, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return fmt.Errorf("call token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%w: %s: %s", ErrTokenExchange, resp.Status, strings.TrimSpace(string(body)))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode token response: %w", err)
	}
	return nil
}

// tokenResponse is the JSON shape Codex returns from the token endpoint.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64   `json:"expires_in"`
}

// planFromToken checks the id_token profile first, then the access
// token's chatgpt_plan_type, mirroring the precedence Codex uses for the
// billing dashboard.
func planFromToken(t tokenResponse) string {
	if plan := jwtStringClaim(t.IDToken, "https://api.openai.com/profile", "plan_type"); plan != "" {
		return plan
	}
	return jwtStringClaim(t.AccessToken, "chatgpt_plan_type", "")
}

// jwtStringClaim pulls a string claim from a JWT. It returns "" on any
// parse error or missing claim so callers can fall back cleanly. The
// function lives here instead of in codeximport to avoid an import
// cycle between the two packages: codeximport is the importer, the
// OAuth service is consumed by it indirectly.
func jwtStringClaim(token, namespace, key string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	dec, err := decodeBase64URL(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(dec, &claims); err != nil {
		return ""
	}
	if namespace != "" {
		raw, ok := claims[namespace]
		if !ok {
			return ""
		}
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err != nil {
			return ""
		}
		return stringFromJSON(inner[key])
	}
	return stringFromJSON(claims[key])
}

// stringFromJSON unmarshals a JSON string claim. Empty input returns "".
func stringFromJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return ""
}

// decodeBase64URL accepts both padded and unpadded base64-url encoded
// JWT segments by adding padding before delegating to base64.URLEncoding.
func decodeBase64URL(s string) ([]byte, error) {
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return base64.URLEncoding.DecodeString(s)
}