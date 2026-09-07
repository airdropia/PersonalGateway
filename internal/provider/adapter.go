// Package provider implements the single generic OpenAI-compatible adapter
// used by the pgw gateway. Every configured upstream is one instance of
// Adapter, identified by a user-chosen display_name. There are no
// provider-specific packages; all behaviour follows the OpenAI standard.
//
// Plan §6.
package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/airdropia/pgw/internal/core"
	"github.com/airdropia/pgw/internal/providers"
)

// Type is the factory registration name used by run/providers.go to add
// the generic adapter. Plan §6.1: every provider is one instance of one
// type. Admin storage uses this same string as the configured provider
// type for custom providers.
const Type = "openai-compatible"

// defaultBaseURL is the fallback used when the operator does not supply
// a base_url. It is kept local to the constructor (rather than read back
// from the Registration var) to avoid a Go package-initialization cycle
// between the Registration literal and the constructor.
const defaultBaseURL = "https://api.openai.com/v1"

// Registration is the factory hook for the generic adapter. Discovery
// matches the plain OpenAI shape; DefaultBaseURL ends in /v1 so the
// gateway's /v1/models discovery call works without further surgery.
var Registration = providers.Registration{
	Type: Type,
	New:  New,
	Discovery: providers.DiscoveryConfig{
		DefaultBaseURL:  defaultBaseURL,
		RequireBaseURL:  true,
		AllowAPIKeyless: false,
	},
}

// Adapter is the single OpenAI-compatible provider implementation. It
// forwards every request body unchanged to the upstream base_url and
// returns the upstream response body, optionally as an SSE stream. It
// has no provider-specific behaviour beyond authenticating with the
// caller's api key.
type Adapter struct {
	baseURL string
	keys    *providers.Keyring
	client  *http.Client
}

// New constructs an Adapter from a factory-resolved ProviderConfig.
// It does not perform any I/O at construction time; ListModels, Chat
// completion, etc. open HTTP on demand.
func New(cfg providers.ProviderConfig, opts providers.ProviderOptions) core.Provider {
	baseURL := providers.ResolveBaseURL(cfg.BaseURL, defaultBaseURL)
	// Ensure the base URL ends in /v1 so the path-joined endpoints below
	// hit the OpenAI-compatible surface even if the operator gave a bare
	// host. Trailing slashes are stripped first to keep the join clean.
	baseURL = strings.TrimRight(baseURL, "/")
	if !strings.HasSuffix(baseURL, "/v1") {
		baseURL = baseURL + "/v1"
	}
	return &Adapter{
		baseURL: baseURL,
		keys:    opts.Keyring(cfg.APIKey),
		client:  &http.Client{},
	}
}

// listResponse mirrors the OpenAI /v1/models response body.
type listResponse struct {
	Object string `json:"object"`
	Data   []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// ListModels calls GET {base_url}/v1/models with the configured api key.
// It returns only the standard fields; metadata heuristics live in
// metadata.go and are applied at the storage projection layer, not here.
func (a *Adapter) ListModels(ctx context.Context) (*core.ModelsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.baseURL+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("build /v1/models request: %w", err)
	}
	a.applyAuth(req)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call /v1/models: %w", err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream /v1/models returned %d", resp.StatusCode)
	}

	var body listResponse
	if err := decodeJSON(resp.Body, &body); err != nil {
		return nil, fmt.Errorf("decode /v1/models: %w", err)
	}

	out := &core.ModelsResponse{Object: "list"}
	for _, m := range body.Data {
		out.Data = append(out.Data, core.Model{
			ID:      m.ID,
			Object:  nonEmpty(m.Object, "model"),
			Created: m.Created,
			OwnedBy: m.OwnedBy,
		})
	}
	return out, nil
}

// postJSON sends a POST of payload to the given endpoint, authenticates
// it, checks the upstream status, and decodes the response body into
// out. It is the shared spine of every non-streaming OpenAI-compatible
// surface the adapter implements (chat completions, responses,
// embeddings); each public method supplies its own endpoint and decode
// target. Stage 5 removes the responses/embeddings callers, at which
// point this helper shrinks with them.
func (a *Adapter) postJSON(ctx context.Context, endpoint string, payload any, out any) error {
	body, err := encodeJSON(payload)
	if err != nil {
		return fmt.Errorf("encode %s: %w", endpoint, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s: %w", endpoint, err)
	}
	a.applyAuth(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s: %w", endpoint, err)
	}
	defer drainAndClose(resp.Body)

	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream %s returned %d", endpoint, resp.StatusCode)
	}
	if err := decodeJSON(resp.Body, out); err != nil {
		return fmt.Errorf("decode %s: %w", endpoint, err)
	}
	return nil
}

// postStream sends a POST of payload to the given endpoint with
// stream=true and Accept: text/event-stream, and returns the raw SSE
// body. The caller is responsible for closing the body.
func (a *Adapter) postStream(ctx context.Context, endpoint string, payload any) (io.ReadCloser, error) {
	body, err := encodeJSON(payload)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", endpoint, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", endpoint, err)
	}
	a.applyAuth(httpReq)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", endpoint, err)
	}
	if resp.StatusCode >= 400 {
		defer drainAndClose(resp.Body)
		return nil, fmt.Errorf("upstream %s returned %d", endpoint, resp.StatusCode)
	}
	return resp.Body, nil
}

// ChatCompletion forwards the request body unchanged to
// {base_url}/chat/completions and returns the parsed upstream response.
// No provider-specific transformation is applied.
func (a *Adapter) ChatCompletion(ctx context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	var out core.ChatResponse
	if err := a.postJSON(ctx, "chat/completions", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamChatCompletion forwards the request body unchanged and returns
// the raw SSE stream. The caller is responsible for closing the body.
func (a *Adapter) StreamChatCompletion(ctx context.Context, req *core.ChatRequest) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("nil chat request")
	}
	return a.postStream(ctx, "chat/completions", req.WithStreaming())
}

// Responses forwards to {base_url}/responses. Stage 5 removes the
// /v1/responses endpoints, at which point this method is dropped from
// the adapter; until then the gateway can already reach it for callers
// who insist on the Responses shape.
func (a *Adapter) Responses(ctx context.Context, req *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	var out core.ResponsesResponse
	if err := a.postJSON(ctx, "responses", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// StreamResponses forwards to {base_url}/responses with stream=true and
// returns the SSE body. Stage 5 removes the Responses surface.
func (a *Adapter) StreamResponses(ctx context.Context, req *core.ResponsesRequest) (io.ReadCloser, error) {
	if req == nil {
		return nil, errors.New("nil responses request")
	}
	streamReq := *req
	streamReq.Stream = true
	return a.postStream(ctx, "responses", &streamReq)
}

// Embeddings forwards to {base_url}/embeddings. Stage 4 removes the
// /v1/embeddings endpoint, at which point this method is dropped.
func (a *Adapter) Embeddings(ctx context.Context, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	var out core.EmbeddingResponse
	if err := a.postJSON(ctx, "embeddings", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// applyAuth sets the Bearer token on the request. Rotation through the
// Keyring is a Stage 1+ optimization; for now the first key is enough.
func (a *Adapter) applyAuth(req *http.Request) {
	if a.keys == nil || a.keys.Len() == 0 {
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.keys.Primary())
}

// nonEmpty returns s unless it is empty, in which case it returns def.
func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// drainAndClose consumes any remaining bytes in the body and closes it.
// Used in defer to allow connection reuse and to avoid leaking sockets.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, body)
	_ = body.Close()
}
