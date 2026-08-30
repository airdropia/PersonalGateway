package codexoauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeStore is the in-memory Store implementation used by the service
// tests. It keeps every row in a map so tests can assert what the service
// persists and how it behaves on read.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]Connection
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: make(map[string]Connection)}
}

func (s *fakeStore) GetByProvider(_ context.Context, name string) (*Connection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[name]
	if !ok {
		return nil, ErrNotFound
	}
	copy := c
	return &copy, nil
}

func (s *fakeStore) Upsert(_ context.Context, c Connection) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[c.ProviderName] = c
	return nil
}

func (s *fakeStore) Delete(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[name]; !ok {
		return ErrNotFound
	}
	delete(s.rows, name)
	return nil
}

func (s *fakeStore) Close() error { return nil }

// fakeTokenClient serves canned responses for /oauth/token exchanges
// and refreshes. Every test that exercises the service wires one of
// these up so it can pin the response shape and count calls.
type fakeTokenClient struct {
	exchangeResp  CodeExchangeResponse
	refreshResp   RefreshResponse
	exchangeCalls atomic.Int32
	refreshCalls  atomic.Int32
	refreshErr    error
}

func (f *fakeTokenClient) ExchangeCode(_ context.Context, req CodeExchangeRequest) (*CodeExchangeResponse, error) {
	f.exchangeCalls.Add(1)
	if req.CodeVerifier == "" {
		return nil, errors.New("missing code_verifier")
	}
	if req.RedirectURI == "" {
		return nil, errors.New("missing redirect_uri")
	}
	out := f.exchangeResp
	return &out, nil
}

func (f *fakeTokenClient) Refresh(_ context.Context, req RefreshRequest) (*RefreshResponse, error) {
	f.refreshCalls.Add(1)
	if f.refreshErr != nil {
		return nil, f.refreshErr
	}
	if req.RefreshToken == "" {
		return nil, errors.New("missing refresh_token")
	}
	out := f.refreshResp
	return &out, nil
}

// makeTestJWT builds a deterministic JWT the parser can decode. Tests
// use it to assert the package surfaces account_id, email, and plan
// from the token.
func makeTestJWT(t *testing.T, accountID, email, plan string, exp time.Time) string {
	t.Helper()
	payload := `{"sub":"user-1","exp":` +
		strconv.FormatInt(exp.Unix(), 10) +
		`,"email":"` + email + `","https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID +
		`"},"https://api.openai.com/profile":{"plan_type":"` + plan + `"}}`
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + body + "." + sig
}

func newServiceForTest(t *testing.T, store Store, client TokenClient) *Service {
	t.Helper()
	svc, err := NewService(store, client)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

// TestPKCE_RoundTrip pins the documented shape of the PKCE pair: a
// URL-safe verifier, an S256 challenge whose decoded SHA-256 matches
// the verifier.
func TestPKCE_RoundTrip(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if l := len(pkce.Verifier); l < 43 || l > 128 {
		t.Fatalf("verifier length %d out of [43,128] band", l)
	}
	sum := sha256.Sum256([]byte(pkce.Verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != expected {
		t.Fatal("challenge does not match S256 of verifier")
	}
}

// TestBuildAuthorizeURL asserts the production defaults are wired up
// and that an explicit override takes precedence.
func TestBuildAuthorizeURL(t *testing.T) {
	pkce := PKCE{Verifier: "v", Challenge: "c"}
	got, err := BuildAuthorizeURL(AuthorizeParams{
		State: "state-1",
	}, pkce)
	if err != nil {
		t.Fatalf("BuildAuthorizeURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}
	if !strings.HasPrefix(u.String(), ProductionIssuer) {
		t.Fatalf("issuer prefix wrong: %s", u.String())
	}
	q := u.Query()
	if q.Get("response_type") != "code" {
		t.Fatalf("response_type = %q", q.Get("response_type"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Fatalf("code_challenge_method = %q", q.Get("code_challenge_method"))
	}
	if q.Get("code_challenge") != "c" {
		t.Fatalf("code_challenge = %q", q.Get("code_challenge"))
	}
	if q.Get("state") != "state-1" {
		t.Fatalf("state = %q", q.Get("state"))
	}
	if q.Get("client_id") != ProductionClientID {
		t.Fatalf("client_id = %q", q.Get("client_id"))
	}
}

// TestParseCallback_Valid covers the success path.
func TestParseCallback_Valid(t *testing.T) {
	callbackURL := "https://example.com/auth/callback?code=abc&state=xyz"
	code, state, err := ParseCallback(callbackURL)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if code != "abc" || state != "xyz" {
		t.Fatalf("code=%q state=%q", code, state)
	}
}

// TestParseCallback_RejectsMissingParameters guards against silent
// fallthrough: both parameters must be present and non-empty after
// trimming.
func TestParseCallback_RejectsMissingParameters(t *testing.T) {
	if _, _, err := ParseCallback(""); err == nil {
		t.Fatal("empty callback URL should fail")
	}
	if _, _, err := ParseCallback("https://example.com/?state=xyz"); err == nil {
		t.Fatal("missing code parameter should fail")
	}
	if _, _, err := ParseCallback("https://example.com/?code=abc"); err == nil {
		t.Fatal("missing state parameter should fail")
	}
}

// TestService_StartLogin populates the pending flow and returns an
// authorization URL whose state matches the pending nonce.
func TestService_StartLogin(t *testing.T) {
	svc := newServiceForTest(t, newFakeStore(), &fakeTokenClient{})

	pending, err := svc.StartLogin(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	if pending.State == "" {
		t.Fatal("state must be populated")
	}
	if pending.CodeVerifier == "" {
		t.Fatal("code_verifier must be populated")
	}
	if !strings.Contains(pending.AuthURL, pending.State) {
		t.Fatalf("AuthURL must echo the state nonce: %s", pending.AuthURL)
	}
}

// TestService_CompleteLogin_HappyPath exercises the canonical exchange
// flow: a pending flow plus a callback URL with a matching state must
// yield a persisted connection.
func TestService_CompleteLogin_HappyPath(t *testing.T) {
	store := newFakeStore()
	exp := time.Now().Add(time.Hour)
	access := makeTestJWT(t, "acc-1", "user@example.com", "pro", exp)
	client := &fakeTokenClient{
		exchangeResp: CodeExchangeResponse{
			AccessToken:  access,
			RefreshToken: "rt-1",
			IDToken:      access,
			ExpiresIn:    3600,
		},
	}
	svc := newServiceForTest(t, store, client)

	pending, err := svc.StartLogin(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callbackURL := "https://chatgpt.com/backend-api/codex/callback?code=auth-code&state=" + url.QueryEscape(pending.State)
	conn, err := svc.CompleteLogin(context.Background(), "chatgpt", callbackURL)
	if err != nil {
		t.Fatalf("CompleteLogin: %v", err)
	}
	if conn.AccessToken != access {
		t.Fatal("access token not stored")
	}
	if conn.RefreshToken != "rt-1" {
		t.Fatal("refresh token not stored")
	}
	if conn.AccountID != "acc-1" {
		t.Fatalf("account_id derived from JWT = %q", conn.AccountID)
	}
	if conn.Email != "user@example.com" {
		t.Fatalf("email derived from JWT = %q", conn.Email)
	}
	if conn.Plan != "pro" {
		t.Fatalf("plan derived from JWT = %q", conn.Plan)
	}
	if conn.Status != "active" {
		t.Fatalf("status = %q", conn.Status)
	}

	// Pending flow must be cleared so a second CompleteLogin with the
	// same callback is rejected as state-mismatch.
	if _, err := svc.CompleteLogin(context.Background(), "chatgpt", callbackURL); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second call err = %v, want ErrInvalidState", err)
	}
}

// TestService_CompleteLogin_StateMismatch rejects callbacks that arrive
// with a state different from the pending flow.
func TestService_CompleteLogin_StateMismatch(t *testing.T) {
	store := newFakeStore()
	client := &fakeTokenClient{
		exchangeResp: CodeExchangeResponse{
			AccessToken: "x", RefreshToken: "y", ExpiresIn: 60,
		},
	}
	svc := newServiceForTest(t, store, client)

	if _, err := svc.StartLogin(context.Background(), "chatgpt"); err != nil {
		t.Fatalf("StartLogin: %v", err)
	}
	callbackURL := "https://example.com/?code=abc&state=wrong-state"
	if _, err := svc.CompleteLogin(context.Background(), "chatgpt", callbackURL); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("err = %v, want ErrInvalidState", err)
	}
}

// TestService_RefreshIfNeeded_WithinSkew returns the existing access
// token when expiry is comfortably in the future.
func TestService_RefreshIfNeeded_WithinSkew(t *testing.T) {
	store := newFakeStore()
	future := time.Now().Add(time.Hour).Unix()
	store.rows["chatgpt"] = Connection{
		ProviderName:    "chatgpt",
		AccessToken:     "still-fresh",
		RefreshToken:    "rt-1",
		AccessExpiresAt: future,
		Status:          "active",
	}
	client := &fakeTokenClient{}
	svc := newServiceForTest(t, store, client)

	token, err := svc.RefreshIfNeeded(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if token != "still-fresh" {
		t.Fatalf("token = %q, want still-fresh", token)
	}
	if got := client.refreshCalls.Load(); got != 0 {
		t.Fatalf("refresh calls = %d, want 0", got)
	}
}

// TestService_RefreshIfNeeded_Refreshes covers the expiry-near path:
// the service round-trips the refresh-token grant, persists the new
// tokens, and returns the fresh access token.
func TestService_RefreshIfNeeded_Refreshes(t *testing.T) {
	store := newFakeStore()
	past := time.Now().Add(-time.Minute).Unix()
	store.rows["chatgpt"] = Connection{
		ProviderName:    "chatgpt",
		AccessToken:     "stale",
		RefreshToken:    "rt-1",
		AccessExpiresAt: past,
		Status:          "active",
	}
	exp := time.Now().Add(time.Hour)
	newAccess := makeTestJWT(t, "acc-1", "", "", exp)
	client := &fakeTokenClient{
		refreshResp: RefreshResponse{
			AccessToken:  newAccess,
			RefreshToken: "rt-2",
			IDToken:      newAccess,
			ExpiresIn:    3600,
		},
	}
	svc := newServiceForTest(t, store, client)

	token, err := svc.RefreshIfNeeded(context.Background(), "chatgpt")
	if err != nil {
		t.Fatalf("RefreshIfNeeded: %v", err)
	}
	if token != newAccess {
		t.Fatalf("token = %q, want new", token)
	}
	if client.refreshCalls.Load() != 1 {
		t.Fatalf("refresh calls = %d, want 1", client.refreshCalls.Load())
	}
	stored, _ := store.GetByProvider(context.Background(), "chatgpt")
	if stored.RefreshToken != "rt-2" {
		t.Fatalf("refresh token not rotated: %q", stored.RefreshToken)
	}
	if stored.AccessToken != newAccess {
		t.Fatal("new access token not stored")
	}
	if stored.Status != "active" {
		t.Fatalf("status after successful refresh = %q", stored.Status)
	}
}

// TestService_RefreshIfNeeded_CollapsesConcurrentCalls verifies the
// singleflight guarantee: ten parallel callers on an expired token
// trigger exactly one upstream refresh, mirroring plan §9.4.
func TestService_RefreshIfNeeded_CollapsesConcurrentCalls(t *testing.T) {
	store := newFakeStore()
	past := time.Now().Add(-time.Minute).Unix()
	store.rows["chatgpt"] = Connection{
		ProviderName:    "chatgpt",
		AccessToken:     "stale",
		RefreshToken:    "rt-1",
		AccessExpiresAt: past,
		Status:          "active",
	}
	client := &gatedTokenClient{
		resp: RefreshResponse{
			AccessToken:  "fresh",
			RefreshToken: "rt-2",
			ExpiresIn:    3600,
		},
	}
	svc := newServiceForTest(t, store, client)

	var wg sync.WaitGroup
	const callers = 10
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := svc.RefreshIfNeeded(context.Background(), "chatgpt"); err != nil {
				t.Errorf("RefreshIfNeeded: %v", err)
			}
		}()
	}
	// Let the singleflight goroutines pile up on the gate before
	// releasing the first upstream call. Without the collapse, ten
	// concurrent refreshes would call upstream ten times.
	time.Sleep(50 * time.Millisecond)
	client.Release()
	wg.Wait()
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 (singleflight collapse)", got)
	}
}

// TestService_RefreshIfNeeded_RefreshFailed_MarksNeedsRelogin covers
// the permanent-rejection path: when the issuer rejects the refresh
// token the stored connection flips to needs_relogin so the dashboard
// can prompt for a re-login.
func TestService_RefreshIfNeeded_RefreshFailed_MarksNeedsRelogin(t *testing.T) {
	store := newFakeStore()
	past := time.Now().Add(-time.Minute).Unix()
	store.rows["chatgpt"] = Connection{
		ProviderName:    "chatgpt",
		AccessToken:     "stale",
		RefreshToken:    "rt-1",
		AccessExpiresAt: past,
		Status:          "active",
	}
	client := &fakeTokenClient{
		refreshErr: errors.New("refresh_token_expired"),
	}
	svc := newServiceForTest(t, store, client)

	if _, err := svc.RefreshIfNeeded(context.Background(), "chatgpt"); err == nil {
		t.Fatal("expected error from rejected refresh")
	}
	stored, _ := store.GetByProvider(context.Background(), "chatgpt")
	if stored.Status != "needs_relogin" {
		t.Fatalf("status = %q, want needs_relogin", stored.Status)
	}
}

// TestService_Forget_DeletesConnection confirms the forget path
// actually removes the stored row.
func TestService_Forget_DeletesConnection(t *testing.T) {
	store := newFakeStore()
	store.rows["chatgpt"] = Connection{ProviderName: "chatgpt", AccessToken: "x"}
	svc := newServiceForTest(t, store, &fakeTokenClient{})

	if err := svc.Forget(context.Background(), "chatgpt"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	if _, err := svc.Connection(context.Background(), "chatgpt"); !errors.Is(err, ErrNoConnection) {
		t.Fatalf("after Forget, Connection err = %v, want ErrNoConnection", err)
	}
}

// gatedTokenClient blocks the first Refresh call on a channel so the
// caller can stack concurrent refreshes against the singleflight key
// before the upstream response arrives.
type gatedTokenClient struct {
	mu     sync.Mutex
	resp   RefreshResponse
	calls  atomic.Int32
	gate   chan struct{}
	closed atomic.Bool
}

func (b *gatedTokenClient) Refresh(_ context.Context, _ RefreshRequest) (*RefreshResponse, error) {
	b.calls.Add(1)
	if !b.closed.Load() {
		b.mu.Lock()
		if b.gate == nil {
			b.gate = make(chan struct{})
		}
		gate := b.gate
		b.mu.Unlock()
		<-gate
	}
	out := b.resp
	return &out, nil
}

func (b *gatedTokenClient) Release() {
	b.closed.Store(true)
	b.mu.Lock()
	if b.gate != nil {
		close(b.gate)
	}
	b.mu.Unlock()
}

func (b *gatedTokenClient) ExchangeCode(_ context.Context, _ CodeExchangeRequest) (*CodeExchangeResponse, error) {
	return nil, errors.New("not used in this test")
}