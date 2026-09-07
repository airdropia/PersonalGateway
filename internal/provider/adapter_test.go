package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airdropia/pgw/internal/core"
	"github.com/airdropia/pgw/internal/providers"
)

// newTestAdapter builds an Adapter whose http.Client targets the given
// test server. It bypasses New so the test can swap the transport.
func newTestAdapter(t *testing.T, baseURL, apiKey string) *Adapter {
	t.Helper()
	cfg := providers.ProviderConfig{
		Name:    "test",
		Type:    Type,
		BaseURL: baseURL,
		APIKey:  apiKey,
	}
	opts := providers.ProviderOptions{}
	a, ok := New(cfg, opts).(*Adapter)
	if !ok {
		t.Fatalf("New did not return *Adapter (got %T)", a)
	}
	return a
}

func TestNew_NormalisesBaseURL(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"trailing slash", "https://example.com/v1/", "https://example.com/v1"},
		{"bare host", "https://example.com", "https://example.com/v1"},
		{"bare host trailing slash", "https://example.com/", "https://example.com/v1"},
		{"explicit v1", "https://example.com/v1", "https://example.com/v1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := providers.ProviderConfig{
				Type:    Type,
				BaseURL: tc.input,
				APIKey:  "test-key",
			}
			a, ok := New(cfg, providers.ProviderOptions{}).(*Adapter)
			if !ok {
				t.Fatalf("New returned %T", a)
			}
			if a.baseURL != tc.expect {
				t.Errorf("baseURL = %q, want %q", a.baseURL, tc.expect)
			}
		})
	}
}

func TestListModels_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q, want /v1/models", r.URL.Path)
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", auth)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "gpt-4o-mini", "object": "model", "owned_by": "openai"},
				{"id": "claude-3-5-sonnet", "object": "model", "owned_by": "anthropic"},
			},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "test-key")
	resp, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if resp.Object != "list" {
		t.Errorf("object = %q, want list", resp.Object)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data len = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].ID != "gpt-4o-mini" {
		t.Errorf("first id = %q, want gpt-4o-mini", resp.Data[0].ID)
	}
	if resp.Data[1].OwnedBy != "anthropic" {
		t.Errorf("second owned_by = %q, want anthropic", resp.Data[1].OwnedBy)
	}
}

func TestListModels_FailsOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "bad-key")
	if _, err := a.ListModels(context.Background()); err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}

func TestChatCompletion_ForwardsBody(t *testing.T) {
	var seenBody []byte
	var seenAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		seenAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Model:   "gpt-4o-mini",
			Choices: []core.Choice{{Message: core.ResponseMessage{Role: "assistant"}, Index: 0, FinishReason: "stop"}},
		})
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "test-key")
	req := &core.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []core.Message{{Role: "user", Content: "hello"}},
	}
	resp, err := a.ChatCompletion(context.Background(), req)
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if resp.ID != "chatcmpl-123" {
		t.Errorf("id = %q, want chatcmpl-123", resp.ID)
	}
	if seenAuth != "Bearer test-key" {
		t.Errorf("auth = %q, want Bearer test-key", seenAuth)
	}
	if !strings.Contains(string(seenBody), `"model":"gpt-4o-mini"`) {
		t.Errorf("body missing model field: %s", string(seenBody))
	}
}

func TestStreamChatCompletion_SetsStreamAndAccepts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Accept") != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", r.Header.Get("Accept"))
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			t.Errorf("body missing stream:true: %s", string(body))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\"}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	a := newTestAdapter(t, srv.URL, "test-key")
	stream, err := a.StreamChatCompletion(context.Background(), &core.ChatRequest{
		Model:    "gpt-4o-mini",
		Messages: []core.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	defer stream.Close()
	body, _ := io.ReadAll(stream)
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Errorf("missing [DONE] marker in stream: %s", string(body))
	}
}

func TestApplyAuth_NilKeyringIsSafe(t *testing.T) {
	a := &Adapter{baseURL: "https://example.com/v1"}
	req, _ := http.NewRequest(http.MethodGet, "https://example.com/v1/models", nil)
	a.applyAuth(req)
	if req.Header.Get("Authorization") != "" {
		t.Errorf("expected no Authorization header, got %q", req.Header.Get("Authorization"))
	}
}