package codexoauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// PKCE holds the verifier / challenge pair the OAuth flow uses to bind
// the authorization request to the token exchange.
type PKCE struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE returns a fresh PKCE pair with a 64-byte verifier (43 URL-safe
// base64 chars after padding removal) and an S256 challenge.
func GeneratePKCE() (PKCE, error) {
	var buf [64]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return PKCE{}, fmt.Errorf("read random bytes: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf[:])
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCE{Verifier: verifier, Challenge: challenge}, nil
}

// GenerateState returns a 32-byte URL-safe random nonce the dashboard must
// echo back from the redirect URI to defeat CSRF.
func GenerateState() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("read random bytes: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf[:]), nil
}

// BuildAuthorizeURL composes the upstream authorization URL. The result is
// what the dashboard opens in a new tab; the redirect target is the
// localhost listener the operator pastes back into the dashboard.
func BuildAuthorizeURL(p AuthorizeParams, pkce PKCE) (string, error) {
	if strings.TrimSpace(p.Issuer) == "" {
		p.Issuer = ProductionIssuer
	}
	if strings.TrimSpace(p.ClientID) == "" {
		p.ClientID = ProductionClientID
	}
	if p.ListenPort == 0 {
		p.ListenPort = DefaultListenPort
	}
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", p.ListenPort, DefaultCallbackPath)

	u, err := url.Parse(strings.TrimRight(p.Issuer, "/") + "/authorize")
	if err != nil {
		return "", fmt.Errorf("parse issuer: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email offline_access")
	q.Set("code_challenge", pkce.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("state", p.State)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// ParseCallback extracts the `code` and `state` query parameters from the
// redirect URL the operator pastes into the dashboard. The path is not
// validated because the operator may copy the URL from any client; the
// state must match the in-memory pending flow.
func ParseCallback(rawURL string) (code, state string, err error) {
	if strings.TrimSpace(rawURL) == "" {
		return "", "", errors.New("callback URL is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse callback URL: %w", err)
	}
	q := u.Query()
	code = strings.TrimSpace(q.Get("code"))
	state = strings.TrimSpace(q.Get("state"))
	if code == "" {
		return "", "", errors.New("callback URL is missing the code parameter")
	}
	if state == "" {
		return "", "", errors.New("callback URL is missing the state parameter")
	}
	return code, state, nil
}