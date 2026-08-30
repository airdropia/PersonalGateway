// Package codeximport reads a Codex CLI auth.json file and converts it into
// a credential that the GoModel chatgpt provider can serve. The package is
// intentionally read-only with respect to the source file: it parses, it
// does not modify auth.json or the local Codex installation.
//
// The Codex CLI writes its credentials to:
//
//	$CODEX_HOME/auth.json     when CODEX_HOME is set
//	~/.codex/auth.json        otherwise
//
// On Windows that path resolves to %USERPROFILE%\.codex\auth.json. The
// schema is a JSON object whose top-level shape is:
//
//	{
//	  "auth_mode": "chatgpt",
//	  "tokens": {
//	    "access_token":  "...",
//	    "refresh_token": "...",
//	    "id_token":      "...",
//	    "account_id":    "..."
//	  },
//	  "last_refresh": "2026-08-29T18:54:24+00:00"
//	}
//
// Only auth_mode == "chatgpt" is supported here; the API-key path used by
// the platform-OpenAI provider has its own credential surface. The access
// token is enough to authenticate against the Codex backend; refresh and
// id tokens are surfaced through metadata so a future native-OAuth pass can
// pick them up without re-reading the file.
package codeximport

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// AuthFile documents the on-disk shape of the Codex CLI credentials. It is
// exposed only so the package tests can build deterministic fixtures; the
// import path consumes raw bytes via Parse.
type AuthFile struct {
	AuthMode    string    `json:"auth_mode"`
	Tokens      Tokens    `json:"tokens"`
	LastRefresh time.Time `json:"last_refresh"`
}

// Tokens are the credential fields Codex CLI persists. AccountID is the
// ChatGPT subscription ID the Codex backend expects on every request as
// chatgpt-account-id.
type Tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
}

// Connection is the parsed result of one Codex auth.json file. It carries
// only what the personal gateway needs to register a chatgpt provider:
// the access token, the account ID (derived from the JWT when not present
// in the file), and the metadata an admin UI can show without exposing the
// token values.
type Connection struct {
	AccessToken     string
	RefreshToken    string
	IDToken         string
	AccountID       string
	Email           string
	Plan            string
	LastRefresh     time.Time
	AccessExpiresAt time.Time
	NeedsRelogin    bool
}

// ErrAuthFileNotFound is returned when no Codex auth.json can be located
// in any of the searched directories. Callers should surface this as a
// "run `codex login` first" hint to the operator.
var ErrAuthFileNotFound = errors.New("codex auth.json not found")

// ErrUnsupportedAuthMode is returned when the located auth.json has an
// auth_mode other than "chatgpt" (e.g. "apikey"). The personal gateway
// cannot repurpose API-key mode into a ChatGPT subscription provider.
var ErrUnsupportedAuthMode = errors.New("codex auth.json uses a non-ChatGPT auth_mode")

// ErrMissingAccessToken is returned when the file parses cleanly but does
// not contain a usable access token. The caller should ask the operator
// to run `codex login` again.
var ErrMissingAccessToken = errors.New("codex auth.json is missing an access_token")

// CandidateHome returns the set of Codex home directories that the
// personal gateway will search, in priority order. It honours CODEX_HOME
// first, then the platform-specific default. Windows uses %USERPROFILE%,
// every other platform uses $HOME.
func CandidateHome() []string {
	var homes []string
	if env := strings.TrimSpace(os.Getenv("CODEX_HOME")); env != "" {
		homes = append(homes, env)
	}
	switch runtime.GOOS {
	case "windows":
		if profile := strings.TrimSpace(os.Getenv("USERPROFILE")); profile != "" {
			homes = append(homes, filepath.Join(profile, ".codex"))
		}
	default:
		if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
			homes = append(homes, filepath.Join(home, ".codex"))
		}
	}
	return homes
}

// DiscoverAuthFile searches the candidate homes for an auth.json. Returns
// ErrAuthFileNotFound when none of them contain the file. This is the entry
// point the admin endpoint calls when the operator does not pass an
// explicit codex_home override.
func DiscoverAuthFile() (path string, data []byte, err error) {
	for _, home := range CandidateHome() {
		candidate := filepath.Join(home, "auth.json")
		body, readErr := os.ReadFile(candidate)
		if readErr == nil {
			return candidate, body, nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", nil, fmt.Errorf("read %s: %w", candidate, readErr)
		}
	}
	return "", nil, ErrAuthFileNotFound
}

// Parse converts the raw bytes of an auth.json into a Connection. The
// function is tolerant of unknown fields and the optional last_refresh; a
// successful parse does not by itself mean the access token is still
// valid.
func Parse(data []byte) (*Connection, error) {
	var file AuthFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("codex auth.json is not valid JSON: %w", err)
	}
	if file.AuthMode != "" && file.AuthMode != "chatgpt" {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedAuthMode, file.AuthMode)
	}
	access := strings.TrimSpace(file.Tokens.AccessToken)
	if access == "" {
		return nil, ErrMissingAccessToken
	}

	accountID := strings.TrimSpace(file.Tokens.AccountID)
	if accountID == "" {
		accountID = accountIDFromToken(access)
	}

	expiresAt, email, plan := jwtMetadata(access)

	needsRelogin := false
	if expiresAt.IsZero() {
		// An unreadable JWT is not fatal: the provider will surface a 401
		// on the first failed request, and the dashboard can prompt for a
		// re-login from there.
		needsRelogin = false
	} else if !expiresAt.After(time.Now()) {
		needsRelogin = true
	}

	return &Connection{
		AccessToken:     access,
		RefreshToken:    strings.TrimSpace(file.Tokens.RefreshToken),
		IDToken:         strings.TrimSpace(file.Tokens.IDToken),
		AccountID:       accountID,
		Email:           email,
		Plan:            plan,
		LastRefresh:     file.LastRefresh,
		AccessExpiresAt: expiresAt,
		NeedsRelogin:    needsRelogin,
	}, nil
}

// Metadata returns the JSON-safe projection of a Connection that the admin
// endpoint surfaces to the dashboard. The access, refresh, and id tokens
// are deliberately omitted: the dashboard must never echo them, even in
// logs, because they are equivalent to the operator's subscription.
func (c *Connection) Metadata() map[string]any {
	out := map[string]any{
		"account_id":       c.AccountID,
		"email":            c.Email,
		"plan":             c.Plan,
		"needs_relogin":    c.NeedsRelogin,
		"has_refresh_token": c.RefreshToken != "",
		"has_id_token":     c.IDToken != "",
	}
	if !c.LastRefresh.IsZero() {
		out["last_refresh"] = c.LastRefresh.UTC().Format(time.RFC3339)
	}
	if !c.AccessExpiresAt.IsZero() {
		out["access_expires_at"] = c.AccessExpiresAt.UTC().Format(time.RFC3339)
	}
	return out
}

// accountIDFromToken mirrors the same JWT-claim parsing the chatgpt
// provider uses at request time, so a connection imported without an
// explicit account_id in the file behaves identically to one whose
// account_id was hand-written.
func accountIDFromToken(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	var auth struct {
		AccountID string `json:"chatgpt_account_id"`
	}
	if err := json.Unmarshal(claims["https://api.openai.com/auth"], &auth); err != nil {
		return ""
	}
	return auth.AccountID
}

// jwtMetadata inspects a ChatGPT access token's payload for the
// expiration, email, and plan fields Codex embeds. Missing fields are
// returned as zero values; this function does not surface the token
// contents to the caller.
func jwtMetadata(token string) (expiresAt time.Time, email string, plan string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, "", ""
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, "", ""
	}
	if raw, ok := claims["exp"]; ok {
		var epoch int64
		if err := json.Unmarshal(raw, &epoch); err == nil && epoch > 0 {
			expiresAt = time.Unix(epoch, 0).UTC()
		}
	}
	if raw, ok := claims["email"]; ok {
		_ = json.Unmarshal(raw, &email)
	}
	if raw, ok := claims["https://api.openai.com/profile"]; ok {
		var profile struct {
			PlanType string `json:"plan_type"`
		}
		if err := json.Unmarshal(raw, &profile); err == nil {
			plan = profile.PlanType
		}
	}
	if plan == "" {
		if raw, ok := claims["chatgpt_plan_type"]; ok {
			_ = json.Unmarshal(raw, &plan)
		}
	}
	return expiresAt, email, plan
}