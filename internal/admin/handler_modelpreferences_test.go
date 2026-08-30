package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/enterpilot/gomodel/internal/modelpreferences"
)

// mpFakeStore is the in-memory Store backing modelpreferences.Service during
// the admin handler tests. It is intentionally tiny — it only has to satisfy
// modelpreferences.Store; the contract surface is exercised by the service
// tests in the modelpreferences package itself.
type mpFakeStore struct {
	mu   sync.Mutex
	rows map[string]modelpreferences.Preference
}

func newMPFakeStore() *mpFakeStore {
	return &mpFakeStore{rows: make(map[string]modelpreferences.Preference)}
}

func (s *mpFakeStore) List(_ context.Context) ([]modelpreferences.Preference, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]modelpreferences.Preference, 0, len(s.rows))
	for _, p := range s.rows {
		out = append(out, p)
	}
	return out, nil
}

func (s *mpFakeStore) Upsert(_ context.Context, p modelpreferences.Preference) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.rows[p.Selector] = p
	s.mu.Unlock()
	return nil
}

func (s *mpFakeStore) Delete(_ context.Context, selector string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[selector]; !ok {
		return modelpreferences.ErrNotFound
	}
	delete(s.rows, selector)
	return nil
}

func (s *mpFakeStore) ResetAll(_ context.Context) error {
	s.mu.Lock()
	s.rows = make(map[string]modelpreferences.Preference)
	s.mu.Unlock()
	return nil
}

func (s *mpFakeStore) Close() error { return nil }

// mpFakeCatalog is the minimum surface the service needs from the model
// registry: provider names for selector validation.
type mpFakeCatalog struct{ names []string }

func (c mpFakeCatalog) ProviderNames() []string { return append([]string(nil), c.names...) }

func newModelPreferencesServiceForTest(t *testing.T, providers []string) *modelpreferences.Service {
	t.Helper()
	svc, err := modelpreferences.NewService(newMPFakeStore(), mpFakeCatalog{names: providers})
	if err != nil {
		t.Fatalf("modelpreferences.NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("svc.Refresh: %v", err)
	}
	return svc
}

// jsonRequest builds a request with a JSON body and the matching Content-Type
// header, following the existing convention used by handler_authkeys_test.go.
func jsonRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func newRequestContext(t *testing.T, method, path, body string) (*echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := jsonRequest(t, method, path, body)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}
// mode when the subsystem is not wired. The admin endpoint must answer with
// a clear service-unavailable rather than crashing.
func TestListModelPreferences_NilServiceReturnsUnavailable(t *testing.T) {
	h := &Handler{}
	c, rec := newRequestContext(t, http.MethodGet, "/admin/model-preferences", "")

	if err := h.ListModelPreferences(c); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestListModelPreferences_EmptyAndPopulated verifies the round-trip shape:
// no rows returns an empty JSON array (not null), inserted rows return in
// canonical selector order with their hidden flag intact.
func TestListModelPreferences_EmptyAndPopulated(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	// Empty
	c, rec := newRequestContext(t, http.MethodGet, "/admin/model-preferences", "")
	if err := h.ListModelPreferences(c); err != nil {
		t.Fatalf("ListModelPreferences: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("empty status = %d, want 200", rec.Code)
	}
	var empty []modelpreferences.Preference
	if err := json.Unmarshal(rec.Body.Bytes(), &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if empty == nil {
		t.Fatal("empty response should be [] not null")
	}

	// Populated
	ctx := context.Background()
	if _, err := svc.Upsert(ctx, "openai/gpt-4o", true); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := svc.Upsert(ctx, "openai/", false); err != nil {
		t.Fatalf("Upsert provider: %v", err)
	}
	c, rec = newRequestContext(t, http.MethodGet, "/admin/model-preferences", "")
	if err := h.ListModelPreferences(c); err != nil {
		t.Fatalf("ListModelPreferences populated: %v", err)
	}
	var rows []modelpreferences.Preference
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal populated: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].Selector != "openai/" || rows[1].Selector != "openai/gpt-4o" {
		t.Fatalf("selector order = %q,%q", rows[0].Selector, rows[1].Selector)
	}
	if !rows[1].Hidden {
		t.Fatal("hidden flag lost in admin response")
	}
}

// TestUpsertModelPreference_InvalidSelectorReturnsBadRequest pins the
// contract that a syntactically invalid selector is a 400 from the API,
// not a 500 from the service.
func TestUpsertModelPreference_InvalidSelectorReturnsBadRequest(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	body := modelPreferenceRequest{Selector: "   ", Hidden: true}
	raw, _ := json.Marshal(body)
	c, rec := newRequestContext(t, http.MethodPut, "/admin/model-preferences", string(raw))
	if err := h.UpsertModelPreference(c); err != nil {
		t.Fatalf("UpsertModelPreference: %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestUpsertModelPreference_RoundTrip ensures a valid PUT persists, returns
// the canonical selector, and shows up on the next GET.
func TestUpsertModelPreference_RoundTrip(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	body := modelPreferenceRequest{Selector: " openai/gpt-4o ", Hidden: true}
	raw, _ := json.Marshal(body)
	c, rec := newRequestContext(t, http.MethodPut, "/admin/model-preferences", string(raw))
	if err := h.UpsertModelPreference(c); err != nil {
		t.Fatalf("UpsertModelPreference: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var returned modelpreferences.Preference
	if err := json.Unmarshal(rec.Body.Bytes(), &returned); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if returned.Selector != "openai/gpt-4o" {
		t.Fatalf("Selector = %q, want canonical", returned.Selector)
	}
	if !returned.Hidden {
		t.Fatal("Hidden flag not echoed")
	}

	// Re-fetch via GET to confirm persistence.
	c, rec = newRequestContext(t, http.MethodGet, "/admin/model-preferences", "")
	if err := h.ListModelPreferences(c); err != nil {
		t.Fatalf("ListModelPreferences: %v", err)
	}
	var rows []modelpreferences.Preference
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(rows) != 1 || rows[0].Selector != "openai/gpt-4o" {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestDeleteModelPreference_NotFoundReturns404 ensures the admin endpoint
// surfaces a typed "not found" rather than a 500 when callers target a
// selector that has no row.
func TestDeleteModelPreference_NotFoundReturns404(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	body := deleteModelPreferenceRequest{Selector: "openai/gpt-4o"}
	raw, _ := json.Marshal(body)
	c, rec := newRequestContext(t, http.MethodDelete, "/admin/model-preferences", string(raw))
	if err := h.DeleteModelPreference(c); err != nil {
		t.Fatalf("DeleteModelPreference: %v", err)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestDeleteModelPreference_RoundTrip confirms a successful DELETE returns
// 204 and the preference disappears from the in-memory snapshot.
func TestDeleteModelPreference_RoundTrip(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	if _, err := svc.Upsert(context.Background(), "openai/gpt-4o", true); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	body := deleteModelPreferenceRequest{Selector: "openai/gpt-4o"}
	raw, _ := json.Marshal(body)
	c, rec := newRequestContext(t, http.MethodDelete, "/admin/model-preferences", string(raw))
	if err := h.DeleteModelPreference(c); err != nil {
		t.Fatalf("DeleteModelPreference: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if svc.Len() != 0 {
		t.Fatalf("service Len = %d, want 0", svc.Len())
	}
}

// TestResetModelPreferences_Returns204 confirms the bulk reset endpoint
// returns 204 and clears the in-memory snapshot.
func TestResetModelPreferences_Returns204(t *testing.T) {
	svc := newModelPreferencesServiceForTest(t, []string{"openai"})
	h := &Handler{modelPreferences: svc}

	for _, sel := range []string{"openai/gpt-4o", "openai/", "gpt-4o-mini", "/"} {
		if _, err := svc.Upsert(context.Background(), sel, true); err != nil {
			t.Fatalf("Upsert(%q): %v", sel, err)
		}
	}
	c, rec := newRequestContext(t, http.MethodPost, "/admin/model-preferences/reset", "")
	if err := h.ResetModelPreferences(c); err != nil {
		t.Fatalf("ResetModelPreferences: %v", err)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	if svc.Len() != 0 {
		t.Fatalf("service Len = %d, want 0", svc.Len())
	}
}

// TestListModelPreferences_NilServiceReturnsUnavailable_AllEndpoints checks
// every new admin endpoint for the same "service unavailable" contract. A
// partial wire-up (Handler constructed but option not applied) must be
// observable from every entry point, not just the GET.
func TestListModelPreferences_NilServiceReturnsUnavailable_AllEndpoints(t *testing.T) {
	h := &Handler{}
	cases := []struct {
		name   string
		method string
		path   string
		body   string
		invoke func(*echo.Context) error
	}{
		{
			name: "List",
			method: http.MethodGet,
			path: "/admin/model-preferences",
			invoke: func(c *echo.Context) error { return h.ListModelPreferences(c) },
		},
		{
			name: "Upsert",
			method: http.MethodPut,
			path: "/admin/model-preferences",
			body: `{"selector":"openai/gpt-4o","hidden":true}`,
			invoke: func(c *echo.Context) error { return h.UpsertModelPreference(c) },
		},
		{
			name: "Delete",
			method: http.MethodDelete,
			path: "/admin/model-preferences",
			body: `{"selector":"openai/gpt-4o"}`,
			invoke: func(c *echo.Context) error { return h.DeleteModelPreference(c) },
		},
		{
			name: "Reset",
			method: http.MethodPost,
			path: "/admin/model-preferences/reset",
			invoke: func(c *echo.Context) error { return h.ResetModelPreferences(c) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := newRequestContext(t, tc.method, tc.path, tc.body)
			if err := tc.invoke(c); err != nil {
				t.Fatalf("invoke: %v", err)
			}
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", rec.Code)
			}
		})
	}
}
