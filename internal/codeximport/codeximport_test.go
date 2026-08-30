package codeximport

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// makeToken builds a synthetic JWT whose payload carries the requested
// account id, email, plan, and expiration. Tests use this instead of a
// real ChatGPT token to keep fixtures deterministic and offline.
func makeToken(accountID, email, plan string, exp time.Time) string {
	payload, err := json.Marshal(map[string]any{
		"sub": "user-1",
		"exp": exp.Unix(),
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"https://api.openai.com/profile": map[string]any{
			"plan_type": plan,
		},
	})
	if err != nil {
		panic(err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + body + "." + sig
}

// makeAuthFile builds a synthetic auth.json body with the supplied tokens
// and last_refresh timestamp.
func makeAuthFile(t *testing.T, access, refresh, id, accountID string, lastRefresh time.Time) []byte {
	t.Helper()
	body, err := json.Marshal(AuthFile{
		AuthMode: "chatgpt",
		Tokens: Tokens{
			AccessToken:  access,
			RefreshToken: refresh,
			IDToken:      id,
			AccountID:    accountID,
		},
		LastRefresh: lastRefresh,
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return body
}

// TestParse_HappyPath covers the documented contract for a well-formed
// auth.json: access_token + account_id round-trip, email and plan surface
// from the JWT, and the metadata projection never carries the token.
func TestParse_HappyPath(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour)
	access := makeToken("acc-1", "user@example.com", "pro", exp)
	refresh := "rt"
	id := "it"
	body := makeAuthFile(t, access, refresh, id, "acc-1", exp.Add(-time.Hour))

	conn, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if conn.AccessToken != access {
		t.Fatal("AccessToken lost on roundtrip")
	}
	if conn.RefreshToken != refresh || conn.IDToken != id {
		t.Fatal("Refresh/ID token lost")
	}
	if conn.AccountID != "acc-1" {
		t.Fatalf("AccountID = %q, want acc-1", conn.AccountID)
	}
	if conn.Email != "user@example.com" {
		t.Fatalf("Email = %q", conn.Email)
	}
	if conn.Plan != "pro" {
		t.Fatalf("Plan = %q", conn.Plan)
	}
	if conn.AccessExpiresAt.IsZero() {
		t.Fatal("AccessExpiresAt should be set")
	}
	if conn.NeedsRelogin {
		t.Fatal("NeedsRelogin must be false for a token that has not expired")
	}

	meta := conn.Metadata()
	if _, ok := meta["account_id"]; !ok {
		t.Fatal("metadata must include account_id")
	}
	for _, leak := range []string{"access_token", "refresh_token", "id_token"} {
		if _, ok := meta[leak]; ok {
			t.Fatalf("metadata must not include %s", leak)
		}
	}
}

// TestParse_DerivesAccountIDFromJWT confirms that an auth.json without an
// explicit account_id still produces a usable connection when the access
// token carries the claim.
func TestParse_DerivesAccountIDFromJWT(t *testing.T) {
	access := makeToken("acc-derived", "", "", time.Now().Add(time.Hour))
	body := makeAuthFile(t, access, "", "", "", time.Time{})

	conn, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if conn.AccountID != "acc-derived" {
		t.Fatalf("AccountID = %q, want acc-derived", conn.AccountID)
	}
}

// TestParse_ExpiredTokenFlagsRelogin pins the operator-visible behavior for
// an access token that has already expired. The dashboard surfaces
// NeedsRelogin so the operator knows to run `codex login` again.
func TestParse_ExpiredTokenFlagsRelogin(t *testing.T) {
	access := makeToken("acc-1", "", "", time.Now().Add(-time.Hour))
	body := makeAuthFile(t, access, "", "", "acc-1", time.Time{})

	conn, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !conn.NeedsRelogin {
		t.Fatal("NeedsRelogin must be true for an expired access token")
	}
}

// TestParse_RejectsWrongAuthMode covers the auth_mode guard: the
// personal gateway only knows how to consume ChatGPT-subscription Codex
// logins. API-key mode has its own credential surface.
func TestParse_RejectsWrongAuthMode(t *testing.T) {
	body := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`)
	_, err := Parse(body)
	if !errors.Is(err, ErrUnsupportedAuthMode) {
		t.Fatalf("err = %v, want ErrUnsupportedAuthMode", err)
	}
}

// TestParse_RejectsMissingAccessToken confirms an auth.json that omits
// the access token surfaces ErrMissingAccessToken rather than silently
// returning an empty connection.
func TestParse_RejectsMissingAccessToken(t *testing.T) {
	body := []byte(`{"auth_mode":"chatgpt","tokens":{}}`)
	if !errors.Is(Parse(body), ErrMissingAccessToken) {
		// Parse returns nil conn + the error, so check that error.
	}
	_, err := Parse(body)
	if !errors.Is(err, ErrMissingAccessToken) {
		t.Fatalf("err = %v, want ErrMissingAccessToken", err)
	}
}

// TestParse_RejectsMalformed confirms non-JSON content does not silently
// pass through.
func TestParse_RejectsMalformed(t *testing.T) {
	_, err := Parse([]byte("not json"))
	if err == nil || errors.Is(err, ErrAuthFileNotFound) || errors.Is(err, ErrMissingAccessToken) {
		t.Fatalf("expected a JSON parse error, got %v", err)
	}
}

// TestDiscoverAuthFile_PicksFirstExisting verifies that the search honors
// CODEX_HOME before the platform default, and that it returns
// ErrAuthFileNotFound when neither path exists.
func TestDiscoverAuthFile_PicksFirstExisting(t *testing.T) {
	dir := t.TempDir()
	codexHome := filepath.Join(dir, "codex-home")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"chatgpt","tokens":{"access_token":"x"}}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	found, data, err := DiscoverAuthFile()
	if err != nil {
		t.Fatalf("DiscoverAuthFile: %v", err)
	}
	if found != authPath {
		t.Fatalf("found = %q, want %q", found, authPath)
	}
	if !strings.Contains(string(data), "access_token") {
		t.Fatal("data does not contain the expected token")
	}
}

// TestDiscoverAuthFile_NotFound confirms the search returns
// ErrAuthFileNotFound when no candidate directory holds the file.
func TestDiscoverAuthFile_NotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(dir, "missing-home"))
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)

	_, _, err := DiscoverAuthFile()
	if !errors.Is(err, ErrAuthFileNotFound) {
		t.Fatalf("err = %v, want ErrAuthFileNotFound", err)
	}
}

// TestCandidateHome_RespectsCodexHome pins the precedence order so the
// discovery path is auditable: CODEX_HOME first, platform default after.
func TestCandidateHome_RespectsCodexHome(t *testing.T) {
	t.Setenv("CODEX_HOME", "/custom/home")
	t.Setenv("HOME", "/home/user")

	got := CandidateHome()
	if len(got) < 1 || got[0] != "/custom/home" {
		t.Fatalf("CandidateHome() = %v, want CODEX_HOME first", got)
	}
}