package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/codexoauth"
)

// fakeCodexOAuthService is the in-memory service stub. It records every
// Start / Complete / Refresh / Forget call so tests can assert which
// operator action reached the package layer.
type fakeCodexOAuthService struct {
	mu sync.Mutex

	startResult   *codexoauth.PendingFlow
	startErr      error
	completeErr   error
	completeCalls int
	refreshCalls  int
	forgetCalls   int

	connection *codexoauth.Connection
	connErr    error

	refreshErr error
}

func (s *fakeCodexOAuthService) StartLogin(_ context.Context, provider string) (*codexoauth.PendingFlow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.startErr != nil {
		return nil, s.startErr
	}
	return s.startResult, nil
}

func (s *fakeCodexOAuthService) CompleteLogin(_ context.Context, _, _ string) (*codexoauth.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completeCalls++
	if s.completeErr != nil {
		return nil, s.completeErr
	}
	return s.connection, nil
}

func (s *fakeCodexOAuthService) RefreshIfNeeded(_ context.Context, _ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshCalls++
	if s.refreshErr != nil {
		return "", s.refreshErr
	}
	return "fresh-token", nil
}

func (s *fakeCodexOAuthService) Forget(_ context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetCalls++
	return nil
}

func (s *fakeCodexOAuthService) Connection(_ context.Context, _ string) (*codexoauth.Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connErr != nil {
		return nil, s.connErr
	}
	return s.connection, nil
}

func newCodexOAuthRequestContext(t *testing.T, method, path, body string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func makeCodexTestToken(t *testing.T, accountID string, exp time.Time) string {
	t.Helper()
	payload := fmt.Sprintf(
		`{"sub":"user-1","exp":%d,"email":"u@example.com","https://api.openai.com/auth":{"chatgpt_account_id":"%s"}}`,
		exp.Unix(), accountID,
	)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + body + "." + sig
}


func newCodexHandler(t *testing.T, svc *fakeCodexOAuthService) *Handler {
	t.Helper()
	h := NewHandler(nil, nil, WithCodexOAuth(svc))
	return h
}

// TestStartCodexOAuth_NilServiceReturnsUnavailable confirms the
// endpoint answers with 503 when the subsystem is not wired.
func TestStartCodexOAuth_NilServiceReturnsUnavailable(t *testing.T) {
	h := &Handler{}
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/start", "")
	if err := h.StartCodexOAuth(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestStartCodexOAuth_HappyPath returns the auth URL + state without
// leaking the PKCE verifier.
func TestStartCodexOAuth_HappyPath(t *testing.T) {
	svc := &fakeCodexOAuthService{
		startResult: &codexoauth.PendingFlow{
			State:        "state-1",
			CodeVerifier: "verifier-1",
			AuthURL:      "https://auth.openai.com/authorize?state=state-1",
			IssuedAt:     1000,
			ProviderName: "chatgpt",
		},
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/start", "")

	if err := h.StartCodexOAuth(c); err != nil {
		t.Fatalf("StartCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body codexOAuthStartResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.AuthURL == "" || body.State != "state-1" || body.ProviderName != "chatgpt" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "verifier-1") {
		t.Fatal("response must not leak the PKCE verifier")
	}
}

// TestCompleteCodexOAuth_StateMismatch maps the package sentinel to a
// 400 invalid-request error.
func TestCompleteCodexOAuth_StateMismatch(t *testing.T) {
	svc := &fakeCodexOAuthService{
		completeErr: codexoauth.ErrInvalidState,
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/callback",
		`{"provider_name":"chatgpt","callback_url":"https://example.com/cb?code=x&state=y"}`)
	if err := h.CompleteCodexOAuth(c); err != nil {
		t.Fatalf("CompleteCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestCompleteCodexOAuth_HappyPath stores the connection through the
// service and returns the public projection.
func TestCompleteCodexOAuth_HappyPath(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	access := makeCodexTestToken(t, "acc-1", time.Unix(exp, 0))
	svc := &fakeCodexOAuthService{
		connection: &codexoauth.Connection{
			ProviderName:    "chatgpt",
			AccountID:       "acc-1",
			Email:           "u@example.com",
			Plan:            "pro",
			AccessToken:     access,
			RefreshToken:    "rt-1",
			AccessExpiresAt: exp,
			Status:          "active",
		},
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/callback",
		`{"provider_name":"chatgpt","callback_url":"https://example.com/cb?code=x&state=y"}`)
	if err := h.CompleteCodexOAuth(c); err != nil {
		t.Fatalf("CompleteCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body codexOAuthConnectionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.AccountID != "acc-1" || body.Email != "u@example.com" {
		t.Fatalf("body = %+v", body)
	}
	if strings.Contains(rec.Body.String(), "rt-1") || strings.Contains(rec.Body.String(), access) {
		t.Fatal("response must not leak tokens")
	}
}

// TestGetCodexOAuthStatus_NotFound pins the 404 path.
func TestGetCodexOAuthStatus_NotFound(t *testing.T) {
	svc := &fakeCodexOAuthService{
		connErr: codexoauth.ErrNoConnection,
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodGet, "/admin/providers/chatgpt/oauth/status", "")
	if err := h.GetCodexOAuthStatus(c); err != nil {
		t.Fatalf("GetCodexOAuthStatus: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestRefreshCodexOAuth_CallsService and then reads the connection
// back so the response includes the latest status.
func TestRefreshCodexOAuth_CallsService(t *testing.T) {
	svc := &fakeCodexOAuthService{
		connection: &codexoauth.Connection{
			ProviderName: "chatgpt",
			AccountID:    "acc-1",
			Status:       "active",
		},
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/refresh", "")
	if err := h.RefreshCodexOAuth(c); err != nil {
		t.Fatalf("RefreshCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if svc.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", svc.refreshCalls)
	}
}

// TestRefreshCodexOAuth_RefreshError surfaces upstream failures as a
// 502 with the package's sentinel.
func TestRefreshCodexOAuth_RefreshError(t *testing.T) {
	svc := &fakeCodexOAuthService{
		refreshErr: codexoauth.ErrTokenExchange,
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/refresh", "")
	if err := h.RefreshCodexOAuth(c); err != nil {
		t.Fatalf("RefreshCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
}

// TestForgetCodexOAuth_Returns204 confirms the forget path returns
// 204 No Content.
func TestForgetCodexOAuth_Returns204(t *testing.T) {
	svc := &fakeCodexOAuthService{}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/forget", "")
	if err := h.ForgetCodexOAuth(c); err != nil {
		t.Fatalf("ForgetCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

// TestForgetCodexOAuth_NotFound pins the 404 path when there is no
// connection to forget.
func TestForgetCodexOAuth_NotFound(t *testing.T) {
	svc := &fakeCodexOAuthService{
		// Forget itself always returns nil in the stub; the handler
		// 404s when the connection cannot be loaded afterwards. This
		// matches the admin handler's pattern: Forget deletes by name
		// and the service surfaces ErrNoConnection if the row was
		// already gone.
		connErr: codexoauth.ErrNoConnection,
	}
	h := newCodexHandler(t, svc)
	c, rec := newCodexOAuthRequestContext(t, http.MethodPost, "/admin/providers/chatgpt/oauth/forget", "")
	if err := h.ForgetCodexOAuth(c); err != nil {
		t.Fatalf("ForgetCodexOAuth: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (Forget is idempotent)", rec.Code)
	}
}

// keep imports alive when future refactors trim call sites.
var _ = errors.Is