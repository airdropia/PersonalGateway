package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/codeximport"
	"github.com/enterpilot/gomodel/internal/providers"
)

// fakeCodexCredsService is the test stub for ProviderCredentialsAdmin. It
// captures every Upsert call so tests can assert which credential the
// Codex import endpoint hands off to the live service.
type fakeCodexCredsService struct {
	mu          sync.Mutex
	upsertCalls []providers.ManagedProviderCredential
	upsertErr   error
	managed     map[string]bool
	registered  []string
}

func newFakeCodexCredsService() *fakeCodexCredsService {
	return &fakeCodexCredsService{
		managed:    map[string]bool{},
		registered: []string{"chatgpt", "openai"},
	}
}

func (s *fakeCodexCredsService) List(_ context.Context) ([]providers.ManagedProviderCredential, error) {
	return nil, nil
}
func (s *fakeCodexCredsService) Get(_ context.Context, _ string) (*providers.ManagedProviderCredential, error) {
	return nil, errors.New("not implemented in stub")
}
func (s *fakeCodexCredsService) Upsert(_ context.Context, c providers.ManagedProviderCredential) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upsertErr != nil {
		return s.upsertErr
	}
	s.upsertCalls = append(s.upsertCalls, c)
	return nil
}
func (s *fakeCodexCredsService) Delete(_ context.Context, _ string) error { return nil }
func (s *fakeCodexCredsService) IsManaged(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.managed[name]
}
func (s *fakeCodexCredsService) RegisteredTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.registered...)
}
func (s *fakeCodexCredsService) CredentialSchemas() []providers.CredentialSchema {
	return nil
}

// makeTestToken builds a synthetic JWT that the codeximport package will
// accept. Tests must not rely on any external token format.
func makeTestToken(t *testing.T, accountID, email, plan string, exp time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"sub":   "user-1",
		"exp":   exp.Unix(),
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
		},
		"https://api.openai.com/profile": map[string]any{
			"plan_type": plan,
		},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + body + "." + sig
}

// writeCodexAuthFile lays down a synthetic auth.json and returns its path
// so the test can set CODEX_HOME against it.
func writeCodexAuthFile(t *testing.T, body []byte) string {
	t.Helper()
	home := t.TempDir()
	path := filepath.Join(home, "auth.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	t.Setenv("CODEX_HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	return path
}

func newCodexRequestContext(t *testing.T, method, path, body string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

// TestImportCodexAuth_NilServiceReturnsUnavailable confirms the endpoint
// answers with 503 when the subsystem is not wired, mirroring the other
// admin features.
func TestImportCodexAuth_NilServiceReturnsUnavailable(t *testing.T) {
	h := &Handler{}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")

	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestImportCodexAuth_HappyPath is the main contract test: a synthetic
// auth.json is read, the endpoint returns 200, the metadata is in the
// body, no token values leak, and the credential service receives an
// upsert call with the access token in api_keys.
func TestImportCodexAuth_HappyPath(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour)
	access := makeTestToken(t, "acc-1", "user@example.com", "pro", exp)
	authBody, err := json.Marshal(codeximport.AuthFile{
		AuthMode: "chatgpt",
		Tokens: codeximport.Tokens{
			AccessToken: access,
			AccountID:   "acc-1",
		},
		LastRefresh: exp.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	writeCodexAuthFile(t, authBody)

	svc := newFakeCodexCredsService()
	h := &Handler{providerCredentials: svc}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")

	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}

	var body importCodexAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Provider != "chatgpt" {
		t.Fatalf("Provider = %q, want chatgpt", body.Provider)
	}
	if body.AccountID != "acc-1" {
		t.Fatalf("AccountID = %q", body.AccountID)
	}
	if body.Email != "user@example.com" {
		t.Fatalf("Email = %q", body.Email)
	}
	if body.Plan != "pro" {
		t.Fatalf("Plan = %q", body.Plan)
	}
	if body.NeedsRelogin {
		t.Fatal("NeedsRelogin should be false for a future expiry")
	}
	if body.ExpiresAt == "" {
		t.Fatal("ExpiresAt should be populated for a valid token")
	}
	// Token values must never appear in the response body.
	if strings.Contains(rec.Body.String(), access) {
		t.Fatal("response body must not contain the access token")
	}

	if got := len(svc.upsertCalls); got != 1 {
		t.Fatalf("upsertCalls = %d, want 1", got)
	}
	cred := svc.upsertCalls[0]
	if cred.Name != "chatgpt" || cred.Type != "chatgpt" {
		t.Fatalf("upsert credential = %+v", cred)
	}
	if len(cred.APIKeys) != 1 || cred.APIKeys[0] != access {
		t.Fatalf("api_keys mismatch: %+v", cred.APIKeys)
	}
	if !cred.Enabled {
		t.Fatal("upsert credential should be enabled")
	}
}

// TestImportCodexAuth_ExpiredTokenFlagsRelogin confirms a stale token
// surfaces NeedsRelogin in the response.
func TestImportCodexAuth_ExpiredTokenFlagsRelogin(t *testing.T) {
	exp := time.Now().Add(-time.Hour)
	access := makeTestToken(t, "acc-1", "", "", exp)
	authBody, _ := json.Marshal(codeximport.AuthFile{
		AuthMode: "chatgpt",
		Tokens:   codeximport.Tokens{AccessToken: access, AccountID: "acc-1"},
	})
	writeCodexAuthFile(t, authBody)

	h := &Handler{providerCredentials: newFakeCodexCredsService()}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")
	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	var body importCodexAuthResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.NeedsRelogin {
		t.Fatal("NeedsRelogin must be true for an expired token")
	}
}

// TestImportCodexAuth_NoFileReturnsBadRequest pins the operator-visible
// guidance: "run `codex login` first".
func TestImportCodexAuth_NoFileReturnsBadRequest(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	h := &Handler{providerCredentials: newFakeCodexCredsService()}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")
	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "codex login") {
		t.Fatalf("body must mention codex login, got %s", rec.Body.String())
	}
}

// TestImportCodexAuth_UnsupportedAuthModeReturnsBadRequest confirms the
// auth_mode guard rejects API-key mode.
func TestImportCodexAuth_UnsupportedAuthModeReturnsBadRequest(t *testing.T) {
	body := []byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`)
	writeCodexAuthFile(t, body)

	h := &Handler{providerCredentials: newFakeCodexCredsService()}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")
	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestImportCodexAuth_ManagedNameReturnsBadRequest verifies the read-only
// guard for credentials owned by config/env.
func TestImportCodexAuth_ManagedNameReturnsBadRequest(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	access := makeTestToken(t, "acc-1", "", "", exp)
	authBody, _ := json.Marshal(codeximport.AuthFile{
		AuthMode: "chatgpt",
		Tokens:   codeximport.Tokens{AccessToken: access, AccountID: "acc-1"},
	})
	writeCodexAuthFile(t, authBody)

	svc := newFakeCodexCredsService()
	svc.managed["chatgpt"] = true
	h := &Handler{providerCredentials: svc}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")
	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "read-only") {
		t.Fatalf("body must mention read-only, got %s", rec.Body.String())
	}
}

// TestImportCodexAuth_TypeNotRegistered verifies the chatgpt provider type
// is reachable in the running binary. A Stage-9 build that drops it
// would surface here.
func TestImportCodexAuth_TypeNotRegistered(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	access := makeTestToken(t, "acc-1", "", "", exp)
	authBody, _ := json.Marshal(codeximport.AuthFile{
		AuthMode: "chatgpt",
		Tokens:   codeximport.Tokens{AccessToken: access, AccountID: "acc-1"},
	})
	writeCodexAuthFile(t, authBody)

	svc := newFakeCodexCredsService()
	svc.registered = []string{"openai"} // chatgpt omitted
	h := &Handler{providerCredentials: svc}
	c, rec := newCodexRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/import-codex", "")
	if err := h.ImportCodexAuth(c); err != nil {
		t.Fatalf("ImportCodexAuth: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}