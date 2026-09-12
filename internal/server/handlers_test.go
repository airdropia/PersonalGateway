package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/airdropia/pgw/internal/auditlog"
	"github.com/airdropia/pgw/internal/core"
	"github.com/airdropia/pgw/internal/llmclient"
	"github.com/airdropia/pgw/internal/guardrails"
	"github.com/airdropia/pgw/internal/virtualmodels"
	provideradapter "github.com/airdropia/pgw/internal/providers"
	"github.com/airdropia/pgw/internal/usage"
)

func withRequestSnapshotAndPrompt(req *http.Request, frame *core.RequestSnapshot) *http.Request {
	if req == nil || frame == nil {
		return req
	}
	ctx := core.WithRequestSnapshot(req.Context(), frame)
	if prompt := core.DeriveWhiteBoxPrompt(frame); prompt != nil {
		ctx = core.WithWhiteBoxPrompt(ctx, prompt)
	}
	return req.WithContext(ctx)
}

// redirectVM builds a redirect (alias) virtual model for server tests.
func redirectVM(name, targetModel, targetProvider string, enabled bool) virtualmodels.VirtualModel {
	return virtualmodels.VirtualModel{
		Source:  name,
		Targets: []virtualmodels.Target{{Provider: targetProvider, Model: targetModel}},
		Enabled: enabled,
	}
}

type aliasesTestStore struct {
	rows []virtualmodels.VirtualModel
}

func newAliasesTestStore(rows ...virtualmodels.VirtualModel) *aliasesTestStore {
	return &aliasesTestStore{rows: append([]virtualmodels.VirtualModel(nil), rows...)}
}

func (s *aliasesTestStore) List(_ context.Context) ([]virtualmodels.VirtualModel, error) {
	return append([]virtualmodels.VirtualModel(nil), s.rows...), nil
}

func (s *aliasesTestStore) Get(_ context.Context, source string) (*virtualmodels.VirtualModel, error) {
	for _, vm := range s.rows {
		if vm.Source == source {
			clone := vm
			return &clone, nil
		}
	}
	return nil, virtualmodels.ErrNotFound
}

func (s *aliasesTestStore) Upsert(_ context.Context, vm virtualmodels.VirtualModel) error {
	for i := range s.rows {
		if s.rows[i].Source == vm.Source {
			s.rows[i] = vm
			return nil
		}
	}
	s.rows = append(s.rows, vm)
	return nil
}

func (s *aliasesTestStore) Delete(_ context.Context, source string) error {
	for i := range s.rows {
		if s.rows[i].Source == source {
			s.rows = append(s.rows[:i], s.rows[i+1:]...)
			return nil
		}
	}
	return virtualmodels.ErrNotFound
}

func (s *aliasesTestStore) Close() error {
	return nil
}

type aliasesTestCatalog struct {
	supported     map[string]bool
	providerTypes map[string]string
	models        map[string]core.Model
}

func (c *aliasesTestCatalog) Supports(model string) bool {
	return c.supported[model]
}

func (c *aliasesTestCatalog) ModelAvailable(model string) bool {
	return c.Supports(model)
}

func (c *aliasesTestCatalog) GetProviderType(model string) string {
	return c.providerTypes[model]
}

func (c *aliasesTestCatalog) LookupModel(model string) (*core.Model, bool) {
	entry, ok := c.models[model]
	if !ok {
		return nil, false
	}
	copy := entry
	return &copy, true
}

func (c *aliasesTestCatalog) ProviderNames() []string {
	seen := map[string]struct{}{}
	names := make([]string, 0, len(c.providerTypes))
	for _, providerType := range c.providerTypes {
		if providerType == "" {
			continue
		}
		if _, ok := seen[providerType]; ok {
			continue
		}
		seen[providerType] = struct{}{}
		names = append(names, providerType)
	}
	return names
}

type chunkedReadCloser struct {
	chunks [][]byte
	index  int
}

func (r *chunkedReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}

	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

func (r *chunkedReadCloser) Close() error {
	return nil
}

type flushCountingRecorder struct {
	*httptest.ResponseRecorder
	flushes int
}

func (r *flushCountingRecorder) Flush() {
	r.flushes++
	r.ResponseRecorder.Flush()
}

type delayedChunkReadCloser struct {
	chunks []delayedChunk
	index  int
}

type delayedChunk struct {
	data    []byte
	delay   time.Duration
	started chan<- struct{}
	release <-chan struct{}
}

func (r *delayedChunkReadCloser) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}

	chunk := r.chunks[r.index]
	r.index++
	if chunk.started != nil {
		close(chunk.started)
		r.chunks[r.index-1].started = nil
	}
	if chunk.delay > 0 {
		time.Sleep(chunk.delay)
	}
	if chunk.release != nil {
		<-chunk.release
	}

	return copy(p, chunk.data), nil
}

func (r *delayedChunkReadCloser) Close() error {
	return nil
}

type streamingProviderWithCustomReader struct {
	mockProvider
	reader io.ReadCloser
}

func (p *streamingProviderWithCustomReader) StreamChatCompletion(_ context.Context, _ *core.ChatRequest) (io.ReadCloser, error) {
	if p.err != nil {
		return nil, p.err
	}
	return p.reader, nil
}

type erroringReadCloser struct {
	data []byte
	err  error
	read bool
}

func (r *erroringReadCloser) Read(p []byte) (int, error) {
	if r.read {
		return 0, r.err
	}
	r.read = true
	n := copy(p, r.data)
	if r.err != nil {
		return n, r.err
	}
	return n, io.EOF
}

func (r *erroringReadCloser) Close() error {
	return nil
}

type closeCountingReadCloser struct {
	io.ReadCloser
	closes int
}

func (r *closeCountingReadCloser) Close() error {
	r.closes++
	if r.ReadCloser == nil {
		return nil
	}
	return r.ReadCloser.Close()
}


type capturingAuditLogger struct {
	config  auditlog.Config
	entries []*auditlog.LogEntry
}

func (l *capturingAuditLogger) Write(entry *auditlog.LogEntry) {
	l.entries = append(l.entries, entry)
}

func (l *capturingAuditLogger) Config() auditlog.Config {
	return l.config
}

func (l *capturingAuditLogger) Close() error {
	return nil
}

type erroringWriter struct {
	err error
}

func (w *erroringWriter) Write([]byte) (int, error) {
	return 0, w.err
}

type mockProvider struct {
	err               error
	response          *core.ChatResponse
	responsesResponse *core.ResponsesResponse
	modelsResponse    *core.ModelsResponse
	embeddingResponse *core.EmbeddingResponse
	embeddingErr      error
	streamData        string
	supportedModels   []string
	providerTypes     map[string]string
	providerNames     map[string]string

	passthroughResponse     *core.PassthroughResponse
	passthroughErr          error
	lastPassthroughProvider string
	lastPassthroughReq      *core.PassthroughRequest

	responseGetResponse         *core.ResponsesResponse
	responseInputItemsResponse  *core.ResponseInputItemListResponse
	responseCancelResponse      *core.ResponsesResponse
	responseDeleteResponse      *core.ResponseDeleteResponse
	responseInputTokensResponse *core.ResponseInputTokensResponse
	responseCompactResponse     *core.ResponseCompactResponse
	responseLifecycleErr        error
	responseUtilityErr          error
	responseGetCalls            []responseCall
	responseInputItemsCalls     []responseCall
	responseCancelCalls         []responseCall
	responseDeleteCalls         []responseCall
	capturedResponseUtilityReqs []*core.ResponsesRequest
	capturedResponseUtility     []responseUtilityCall
}

type responseCall struct {
	provider string
	id       string
}

type responseUtilityCall struct {
	provider  string
	operation string
}

type recordingModelAuthorizer struct {
	lastSelector core.ModelSelector
	err          error
	allow        func(core.ModelSelector) bool
}

func (a *recordingModelAuthorizer) ValidateModelAccess(_ context.Context, selector core.ModelSelector) error {
	a.lastSelector = selector
	return a.err
}

func (a *recordingModelAuthorizer) AllowsModel(_ context.Context, selector core.ModelSelector) bool {
	if a.allow != nil {
		return a.allow(selector)
	}
	return true
}

func (a *recordingModelAuthorizer) FilterPublicModels(_ context.Context, models []core.Model) []core.Model {
	return models
}

type staticExposedModelLister struct {
	models []core.Model
}

func (l staticExposedModelLister) ExposedModels() []core.Model {
	return append([]core.Model(nil), l.models...)
}

func readPassthroughRequestBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	if body == nil {
		return ""
	}
	defer func() {
		_ = body.Close()
	}()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("failed to read passthrough request body: %v", err)
	}
	return string(data)
}

func (m *mockProvider) Supports(model string) bool {
	selector, err := core.ParseModelSelector(model, "")
	if err == nil {
		model = selector.Model
	}
	return slices.Contains(m.supportedModels, model)
}

func (m *mockProvider) GetProviderType(model string) string {
	selector, err := core.ParseModelSelector(model, "")
	if err == nil && selector.Provider != "" {
		if m.providerTypes != nil {
			if providerType, ok := m.providerTypes[selector.QualifiedModel()]; ok {
				return providerType
			}
		}
		model = selector.Model
	}

	if m.providerTypes != nil {
		if providerType, ok := m.providerTypes[model]; ok {
			return providerType
		}
		if providerType, ok := inferQualifiedProviderValue(m.providerTypes, model); ok {
			return providerType
		}
	}
	if m.Supports(model) {
		return "mock"
	}
	return ""
}

func (m *mockProvider) GetProviderName(model string) string {
	selector, err := core.ParseModelSelector(model, "")
	if err == nil && selector.Provider != "" {
		if m.providerNames != nil {
			if providerName, ok := m.providerNames[selector.QualifiedModel()]; ok {
				return providerName
			}
		}
		model = selector.Model
	}

	if m.providerNames != nil {
		if providerName, ok := m.providerNames[model]; ok {
			return providerName
		}
		if providerName, ok := inferQualifiedProviderValue(m.providerNames, model); ok {
			return providerName
		}
	}
	return ""
}

func (m *mockProvider) GetProviderNameForType(providerType string) string {
	providerType = strings.TrimSpace(providerType)
	if providerType == "" {
		return ""
	}
	if len(m.providerNames) == 0 {
		return ""
	}
	for qualifiedModel, providerName := range m.providerNames {
		if strings.TrimSpace(m.providerTypes[qualifiedModel]) == providerType {
			return providerName
		}
	}
	return ""
}

func (m *mockProvider) GetProviderTypeForName(providerName string) string {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" || len(m.providerNames) == 0 {
		return ""
	}
	for qualifiedModel, candidate := range m.providerNames {
		if strings.TrimSpace(candidate) != providerName {
			continue
		}
		if providerType := strings.TrimSpace(m.providerTypes[qualifiedModel]); providerType != "" {
			return providerType
		}
	}
	return ""
}

func inferQualifiedProviderValue(values map[string]string, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}

	match := ""
	for qualifiedModel, value := range values {
		selector, err := core.ParseModelSelector(qualifiedModel, "")
		if err != nil || selector.Provider == "" || selector.Model != model || strings.TrimSpace(value) == "" {
			continue
		}
		if match != "" && match != value {
			return "", false
		}
		match = value
	}
	if match == "" {
		return "", false
	}
	return match, true
}



func (m *mockProvider) NativeResponseProviderTypes() []string {
	return nil
}

func (m *mockProvider) ChatCompletion(_ context.Context, _ *core.ChatRequest) (*core.ChatResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.response, nil
}

func (m *mockProvider) StreamChatCompletion(_ context.Context, _ *core.ChatRequest) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(strings.NewReader(m.streamData)), nil
}

func (m *mockProvider) ListModels(_ context.Context) (*core.ModelsResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.modelsResponse, nil
}

func (m *mockProvider) Responses(_ context.Context, _ *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.responsesResponse, nil
}

func (m *mockProvider) StreamResponses(_ context.Context, _ *core.ResponsesRequest) (io.ReadCloser, error) {
	if m.err != nil {
		return nil, m.err
	}
	return io.NopCloser(strings.NewReader(m.streamData)), nil
}

func (m *mockProvider) Embeddings(_ context.Context, _ *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	if m.embeddingErr != nil {
		return nil, m.embeddingErr
	}
	if m.err != nil {
		return nil, m.err
	}
	return m.embeddingResponse, nil
}

func (m *mockProvider) Passthrough(_ context.Context, providerType string, req *core.PassthroughRequest) (*core.PassthroughResponse, error) {
	m.lastPassthroughProvider = providerType
	m.lastPassthroughReq = req
	if m.passthroughErr != nil {
		return nil, m.passthroughErr
	}
	return m.passthroughResponse, nil
}

func (m *mockProvider) GetResponse(_ context.Context, providerType, id string, _ core.ResponseRetrieveParams) (*core.ResponsesResponse, error) {
	m.responseGetCalls = append(m.responseGetCalls, responseCall{provider: providerType, id: id})
	if m.responseLifecycleErr != nil {
		return nil, m.responseLifecycleErr
	}
	if m.responseGetResponse != nil {
		return m.responseGetResponse, nil
	}
	return &core.ResponsesResponse{ID: id, Object: "response", Model: "gpt-5-mini", Provider: providerType, Status: "completed"}, nil
}

func (m *mockProvider) ListResponseInputItems(_ context.Context, providerType, id string, _ core.ResponseInputItemsParams) (*core.ResponseInputItemListResponse, error) {
	m.responseInputItemsCalls = append(m.responseInputItemsCalls, responseCall{provider: providerType, id: id})
	if m.responseLifecycleErr != nil {
		return nil, m.responseLifecycleErr
	}
	if m.responseInputItemsResponse != nil {
		return m.responseInputItemsResponse, nil
	}
	return &core.ResponseInputItemListResponse{Object: "list"}, nil
}

func (m *mockProvider) CancelResponse(_ context.Context, providerType, id string) (*core.ResponsesResponse, error) {
	m.responseCancelCalls = append(m.responseCancelCalls, responseCall{provider: providerType, id: id})
	if m.responseLifecycleErr != nil {
		return nil, m.responseLifecycleErr
	}
	if m.responseCancelResponse != nil {
		return m.responseCancelResponse, nil
	}
	return &core.ResponsesResponse{ID: id, Object: "response", Model: "gpt-5-mini", Provider: providerType, Status: "cancelled"}, nil
}

func (m *mockProvider) DeleteResponse(_ context.Context, providerType, id string) (*core.ResponseDeleteResponse, error) {
	m.responseDeleteCalls = append(m.responseDeleteCalls, responseCall{provider: providerType, id: id})
	if m.responseLifecycleErr != nil {
		return nil, m.responseLifecycleErr
	}
	if m.responseDeleteResponse != nil {
		return m.responseDeleteResponse, nil
	}
	return &core.ResponseDeleteResponse{ID: id, Object: "response", Deleted: true}, nil
}

func (m *mockProvider) CountResponseInputTokens(_ context.Context, providerType string, req *core.ResponsesRequest) (*core.ResponseInputTokensResponse, error) {
	m.capturedResponseUtilityReqs = append(m.capturedResponseUtilityReqs, req)
	m.capturedResponseUtility = append(m.capturedResponseUtility, responseUtilityCall{
		provider:  providerType,
		operation: "CountResponseInputTokens",
	})
	if m.responseUtilityErr != nil {
		return nil, m.responseUtilityErr
	}
	if m.responseInputTokensResponse != nil {
		return m.responseInputTokensResponse, nil
	}
	return &core.ResponseInputTokensResponse{Object: "response.input_tokens", InputTokens: 7}, nil
}

func (m *mockProvider) CompactResponse(_ context.Context, providerType string, req *core.ResponsesRequest) (*core.ResponseCompactResponse, error) {
	m.capturedResponseUtilityReqs = append(m.capturedResponseUtilityReqs, req)
	m.capturedResponseUtility = append(m.capturedResponseUtility, responseUtilityCall{
		provider:  providerType,
		operation: "CompactResponse",
	})
	if m.responseUtilityErr != nil {
		return nil, m.responseUtilityErr
	}
	if m.responseCompactResponse != nil {
		return m.responseCompactResponse, nil
	}
	return &core.ResponseCompactResponse{ID: "cmp_1", Object: "response.compaction", Provider: providerType}, nil
}














func TestChatCompletion(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		response: &core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-4o-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					Message:      core.ResponseMessage{Role: "assistant", Content: "Hello!"},
					FinishReason: "stop",
				},
			},
			Usage: core.Usage{
				PromptTokens:     10,
				CompletionTokens: 5,
				TotalTokens:      15,
			},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "chatcmpl-123") {
		t.Errorf("response missing expected ID, got: %s", body)
	}
	if !strings.Contains(body, "Hello!") {
		t.Errorf("response missing expected content, got: %s", body)
	}
}

func TestChatCompletion_BindsMultimodalContent(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-4o-mini"},
		response: &core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-4o-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					Message:      core.ResponseMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":[{"type":"text","text":"Describe this image"},{"type":"image_url","image_url":{"url":"https://example.com/image.png","detail":"high"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if provider.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}

	parts, ok := core.NormalizeContentParts(provider.capturedChatReq.Messages[0].Content)
	if !ok {
		t.Fatalf("captured content type = %T, want structured content", provider.capturedChatReq.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("len(parts) = %d, want 2", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "Describe this image" {
		t.Fatalf("unexpected first part: %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "https://example.com/image.png" {
		t.Fatalf("unexpected second part: %+v", parts[1])
	}
}

func TestChatCompletion_PreservesUnknownTopLevelFields(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-5-mini"},
		response: &core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-5-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					Message:      core.ResponseMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{
		"model":"gpt-5-mini",
		"messages":[{"role":"user","content":"return json"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"math_response",
				"schema":{"type":"object","properties":{"answer":{"type":"string"}}}
			}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.ChatCompletion(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if provider.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if provider.capturedChatReq.ExtraFields.Lookup("response_format") == nil {
		t.Fatal("response_format missing from ExtraFields")
	}

	body, err := json.Marshal(provider.capturedChatReq)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !bytes.Contains(body, []byte(`"response_format"`)) {
		t.Fatalf("marshaled request missing response_format: %s", string(body))
	}
}

func TestChatCompletion_PreservesUnknownNestedFields(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-5-mini"},
		response: &core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-5-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					Message:      core.ResponseMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{
		"model":"gpt-5-mini",
		"messages":[
			{
				"role":"user",
				"name":"alice",
				"content":[{"type":"text","text":"hello","cache_control":{"type":"ephemeral"}}]
			}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.ChatCompletion(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if provider.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if provider.capturedChatReq.Messages[0].ExtraFields.Lookup("name") == nil {
		t.Fatal("message.name missing from ExtraFields")
	}

	body, err := json.Marshal(provider.capturedChatReq)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	messages := decoded["messages"].([]any)
	firstMsg := messages[0].(map[string]any)
	if firstMsg["name"] != "alice" {
		t.Fatalf("messages[0].name = %#v, want alice", firstMsg["name"])
	}
	content := firstMsg["content"].([]any)
	firstPart := content[0].(map[string]any)
	if _, ok := firstPart["cache_control"].(map[string]any); !ok {
		t.Fatalf("messages[0].content[0].cache_control = %#v, want object", firstPart["cache_control"])
	}
}

func TestChatCompletion_UsesIngressFrameForDecoding(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-5-mini"},
		response: &core.ChatResponse{
			ID:      "chatcmpl-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "gpt-5-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					Message:      core.ResponseMessage{Role: "assistant", Content: "ok"},
					FinishReason: "stop",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "req-ingress-1")
	req.Body = &explodingReadCloser{}

	frame := core.NewRequestSnapshot(
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		nil,
		nil,
		"application/json",
		[]byte(`{
			"model":"gpt-5-mini",
			"messages":[{"role":"user","content":"return json"}],
			"response_format":{"type":"json_schema"}
		}`),
		false,
		"req-ingress-1",
		nil,
	)
	req = withRequestSnapshotAndPrompt(req, frame)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if provider.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if provider.capturedChatReq.ExtraFields.Lookup("response_format") == nil {
		t.Fatal("response_format missing from ExtraFields")
	}

	env := core.GetWhiteBoxPrompt(c.Request().Context())
	if env == nil || env.CachedChatRequest() == nil {
		t.Fatalf("expected semantic envelope to cache ChatRequest, got %+v", env)
	}
	if env.CachedChatRequest() != provider.capturedChatReq {
		t.Fatal("cached ChatRequest does not match provider request")
	}
}

func TestChatCompletion_NormalizesSemanticSelectorHints(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-5-mini"},
		response: &core.ChatResponse{
			ID:     "chatcmpl_123",
			Object: "chat.completion",
			Model:  "gpt-5-mini",
			Choices: []core.Choice{
				{
					Index:        0,
					FinishReason: "stop",
					Message: core.ResponseMessage{
						Role:    "assistant",
						Content: "ok",
					},
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &explodingReadCloser{}

	frame := core.NewRequestSnapshot(
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		nil,
		nil,
		"application/json",
		[]byte(`{
			"model":"openai/gpt-5-mini",
			"messages":[{"role":"user","content":"return json"}]
		}`),
		false,
		"",
		nil,
	)
	req = withRequestSnapshotAndPrompt(req, frame)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if provider.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if provider.capturedChatReq.Model != "gpt-5-mini" {
		t.Fatalf("captured model = %q, want gpt-5-mini", provider.capturedChatReq.Model)
	}
	if provider.capturedChatReq.Provider != "openai" {
		t.Fatalf("captured provider = %q, want openai", provider.capturedChatReq.Provider)
	}

	env := core.GetWhiteBoxPrompt(c.Request().Context())
	if env == nil || env.CachedChatRequest() == nil {
		t.Fatalf("expected semantic envelope to cache ChatRequest, got %+v", env)
	}
	if env.RouteHints.Model != "gpt-5-mini" {
		t.Fatalf("RouteHints.Model = %q, want gpt-5-mini", env.RouteHints.Model)
	}
	if env.RouteHints.Provider != "openai" {
		t.Fatalf("RouteHints.Provider = %q, want openai", env.RouteHints.Provider)
	}
}

func TestChatCompletion_UsesExplicitAliasResolverWithoutProviderDecorator(t *testing.T) {
	catalog := aliasesTestCatalog{
		supported: map[string]bool{
			"anthropic/claude-opus-4-6": true,
			"openai/gpt-5-nano":         true,
		},
		providerTypes: map[string]string{
			"anthropic/claude-opus-4-6": "anthropic",
			"openai/gpt-5-nano":         "openai",
		},
		models: map[string]core.Model{
			"anthropic/claude-opus-4-6": {ID: "claude-opus-4-6", Object: "model"},
			"openai/gpt-5-nano":         {ID: "gpt-5-nano", Object: "model"},
		},
	}

	service, err := virtualmodels.NewService(newAliasesTestStore(
		redirectVM("anthropic/claude-opus-4-6", "gpt-5-nano", "openai", true),
	), &catalog, true)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}

	inner := &capturingProvider{
		supportedModels: []string{"gpt-5-nano"},
		providerTypes: map[string]string{
			"openai/gpt-5-nano": "openai",
		},
		response: &core.ChatResponse{
			ID:       "chatcmpl_alias_resolver_123",
			Object:   "chat.completion",
			Model:    "gpt-5-nano",
			Provider: "openai",
			Choices: []core.Choice{
				{
					Index:        0,
					FinishReason: "stop",
					Message: core.ResponseMessage{
						Role:    "assistant",
						Content: "ok",
					},
				},
			},
		},
	}

	e := echo.New()
	handler := newHandler(inner, nil, nil, nil, service, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &explodingReadCloser{}

	frame := core.NewRequestSnapshot(
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		nil,
		nil,
		"application/json",
		[]byte(`{
			"model":"anthropic/claude-opus-4-6",
			"messages":[{"role":"user","content":"return json"}]
		}`),
		false,
		"",
		nil,
	)
	req = withRequestSnapshotAndPrompt(req, frame)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if inner.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if inner.capturedChatReq.Model != "gpt-5-nano" {
		t.Fatalf("captured model = %q, want gpt-5-nano", inner.capturedChatReq.Model)
	}
	if inner.capturedChatReq.Provider != "openai" {
		t.Fatalf("captured provider = %q, want openai", inner.capturedChatReq.Provider)
	}

	workflow := core.GetWorkflow(c.Request().Context())
	if workflow == nil || workflow.Resolution == nil {
		t.Fatal("expected workflow resolution in context")
	}
	if !workflow.Resolution.AliasApplied {
		t.Fatal("expected alias resolution to be marked as applied")
	}
	if workflow.ResolvedQualifiedModel() != "openai/gpt-5-nano" {
		t.Fatalf("workflow resolved model = %q, want openai/gpt-5-nano", workflow.ResolvedQualifiedModel())
	}
}

func TestChatCompletion_UsesExplicitTranslatedRequestPatcher(t *testing.T) {
	pipeline := guardrails.NewPipeline()
	systemPrompt, err := guardrails.NewSystemPromptGuardrail("test", guardrails.SystemPromptInject, "guardrail system")
	if err != nil {
		t.Fatalf("NewSystemPromptGuardrail() error = %v", err)
	}
	pipeline.Add(systemPrompt, 0)

	inner := &capturingProvider{
		supportedModels: []string{"gpt-5-nano"},
		providerTypes: map[string]string{
			"gpt-5-nano": "mock",
		},
		response: &core.ChatResponse{
			ID:       "chatcmpl_guardrail_123",
			Object:   "chat.completion",
			Model:    "gpt-5-nano",
			Provider: "mock",
			Choices: []core.Choice{
				{
					Index:        0,
					FinishReason: "stop",
					Message: core.ResponseMessage{
						Role:    "assistant",
						Content: "ok",
					},
				},
			},
		},
	}

	patcher := guardrails.NewWorkflowRequestPatcher(staticPipelineResolver{pipeline: pipeline})

	e := echo.New()
	handler := newHandler(inner, nil, nil, nil, nil, nil, nil, patcher)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Content-Type", "application/json")
	req.Body = &explodingReadCloser{}

	frame := core.NewRequestSnapshot(
		http.MethodPost,
		"/v1/chat/completions",
		nil,
		nil,
		nil,
		"application/json",
		[]byte(`{
			"model":"gpt-5-nano",
			"messages":[{"role":"user","content":"return json"}]
		}`),
		false,
		"",
		nil,
	)
	req = withRequestSnapshotAndPrompt(req, frame)

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	if inner.capturedChatReq == nil {
		t.Fatal("expected chat request to be captured")
	}
	if len(inner.capturedChatReq.Messages) != 2 {
		t.Fatalf("captured messages = %d, want 2", len(inner.capturedChatReq.Messages))
	}
	if inner.capturedChatReq.Messages[0].Role != "system" || inner.capturedChatReq.Messages[0].Content != "guardrail system" {
		t.Fatalf("first message = %+v, want injected guardrail system prompt", inner.capturedChatReq.Messages[0])
	}
	if inner.capturedChatReq.Messages[1].Role != "user" {
		t.Fatalf("second message role = %q, want user", inner.capturedChatReq.Messages[1].Role)
	}
}





func TestChatCompletionStreaming(t *testing.T) {
	streamData := `data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-123","object":"chat.completion.chunk","created":1234567890,"model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"!"},"finish_reason":null}]}

data: [DONE]

`
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		streamData:      streamData,
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "stream": true, "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	contentType := rec.Header().Get("Content-Type")
	if contentType != "text/event-stream" {
		t.Errorf("expected Content-Type text/event-stream, got %s", contentType)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "data:") {
		t.Errorf("response should contain SSE data, got: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Errorf("response should contain [DONE], got: %s", body)
	}
}

func TestChatCompletionStreaming_FastPathUsesPassthroughForOpenAICompatibleProviders(t *testing.T) {
	streamData := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	reqBody := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		providerTypes: map[string]string{
			"gpt-4o-mini": "openai",
		},
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(streamData)),
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	if got := rec.Body.String(); got != streamData {
		t.Fatalf("stream body = %q, want %q", got, streamData)
	}
	if mock.lastPassthroughProvider != "openai" {
		t.Fatalf("lastPassthroughProvider = %q, want openai", mock.lastPassthroughProvider)
	}
	if mock.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil, want passthrough request")
	}
	if !mock.lastPassthroughReq.Stream {
		t.Fatal("passthrough request lost explicit stream intent")
	}
	if body := readPassthroughRequestBody(t, mock.lastPassthroughReq.Body); body != reqBody {
		t.Fatalf("passthrough body = %q, want %q", body, reqBody)
	}
}

func TestChatCompletionStreaming_FastPathUsageCarriesResolvedProviderName(t *testing.T) {
	streamData := "data: {\"id\":\"chatcmpl-123\",\"model\":\"gpt-4o-mini\",\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n"
	usageLog := &collectingUsageLogger{
		config: usage.Config{Enabled: true},
	}
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		providerTypes: map[string]string{
			"gpt-4o-mini": "openai",
		},
		providerNames: map[string]string{
			"gpt-4o-mini": "openai_test",
		},
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(streamData)),
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, usageLog, nil)

	reqBody := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if len(usageLog.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
	}
	if got := usageLog.entries[0].ProviderName; got != "openai_test" {
		t.Fatalf("ProviderName = %q, want openai_test", got)
	}
	if mock.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil, want passthrough request")
	}
	checks := []struct {
		name string
		got  any
		want any
	}{
		{name: "Operation", got: mock.lastPassthroughReq.Operation, want: llmclient.OperationChat},
		{name: "Model", got: mock.lastPassthroughReq.Model, want: "gpt-4o-mini"},
		{name: "Stream", got: mock.lastPassthroughReq.Stream, want: true},
		{name: "ProviderName", got: mock.lastPassthroughReq.ProviderName, want: "openai_test"},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if check.got != check.want {
				t.Errorf("passthrough %s = %v, want %v", check.name, check.got, check.want)
			}
		})
	}
}

func TestChatCompletionStreaming_FastPathSkipsQualifiedModelRewrite(t *testing.T) {
	streamData := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	provider := &capturingProvider{
		supportedModels: []string{"gpt-4o-mini"},
		providerTypes: map[string]string{
			"gpt-4o-mini": "openai",
		},
		streamData: streamData,
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader("data: should-not-be-used\n\n")),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{"model":"openai/gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if provider.lastPassthroughReq != nil {
		t.Fatal("lastPassthroughReq != nil, want rewritten request to use StreamChatCompletion path")
	}
	if provider.capturedChatReq == nil {
		t.Fatal("capturedChatReq = nil, want StreamChatCompletion request")
	}
	if provider.capturedChatReq.Model != "gpt-4o-mini" {
		t.Fatalf("captured model = %q, want gpt-4o-mini", provider.capturedChatReq.Model)
	}
	if provider.capturedChatReq.Provider != "openai" {
		t.Fatalf("captured provider = %q, want openai", provider.capturedChatReq.Provider)
	}
	if got := rec.Body.String(); got != streamData {
		t.Fatalf("stream body = %q, want %q", got, streamData)
	}
}

func TestChatCompletionStreaming_FastPathSkipsProviderFieldRewrite(t *testing.T) {
	streamData := "data: {\"id\":\"chatcmpl-123\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: [DONE]\n\n"
	provider := &capturingProvider{
		supportedModels: []string{"gpt-4o-mini"},
		providerTypes: map[string]string{
			"openai/gpt-4o-mini": "openai",
		},
		streamData: streamData,
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader("data: should-not-be-used\n\n")),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{"model":"gpt-4o-mini","provider":"openai","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if provider.lastPassthroughReq != nil {
		t.Fatal("lastPassthroughReq != nil, want provider field rewrite to use StreamChatCompletion path")
	}
	if provider.capturedChatReq == nil {
		t.Fatal("capturedChatReq = nil, want StreamChatCompletion request")
	}
	if provider.capturedChatReq.Provider != "openai" {
		t.Fatalf("captured provider = %q, want openai", provider.capturedChatReq.Provider)
	}
	if got := rec.Body.String(); got != streamData {
		t.Fatalf("stream body = %q, want %q", got, streamData)
	}
}

func TestHandleStreamingResponse_FlushesEachChunk(t *testing.T) {
	e := echo.New()
	handler := NewHandler(&mockProvider{}, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	c := e.NewContext(req, rec)

	stream := &chunkedReadCloser{
		chunks: [][]byte{
			[]byte("data: {\"id\":\"1\"}\n\n"),
			[]byte("data: {\"id\":\"2\"}\n\n"),
			[]byte("data: [DONE]\n\n"),
		},
	}

	err := handler.translatedInference().handleStreamingResponse(c, nil, "gpt-4o-mini", "openai", "primary-openai", func() (io.ReadCloser, error) {
		return stream, nil
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	if rec.flushes != 4 {
		t.Fatalf("expected 4 flushes (headers + 3 chunks), got %d", rec.flushes)
	}

	if got := rec.Body.String(); got != "data: {\"id\":\"1\"}\n\ndata: {\"id\":\"2\"}\n\ndata: [DONE]\n\n" {
		t.Fatalf("unexpected body %q", got)
	}
}

func TestFlushStream_ReturnsReadError(t *testing.T) {
	expectedErr := errors.New("stream read failed")
	stream := &erroringReadCloser{
		data: []byte("data: {\"id\":\"1\"}\n\n"),
		err:  expectedErr,
	}

	err := flushStream(io.Discard, stream)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected read error %v, got %v", expectedErr, err)
	}
}

func TestFlushStream_ReturnsWriteError(t *testing.T) {
	expectedErr := errors.New("client write failed")
	stream := io.NopCloser(strings.NewReader("data: {\"id\":\"1\"}\n\n"))

	err := flushStream(&erroringWriter{err: expectedErr}, stream)
	if !errors.Is(err, expectedErr) {
		t.Fatalf("expected write error %v, got %v", expectedErr, err)
	}
}

func TestRequestIDFromContextOrHeader(t *testing.T) {
	t.Run("prefers context request id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-Request-ID", "header-id")
		req = req.WithContext(core.WithRequestID(req.Context(), "context-id"))

		if got := requestIDFromContextOrHeader(req); got != "context-id" {
			t.Fatalf("requestIDFromContextOrHeader() = %q, want context-id", got)
		}
	})

	t.Run("falls back to header request id", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		req.Header.Set("X-Request-ID", "  header-id  ")

		if got := requestIDFromContextOrHeader(req); got != "header-id" {
			t.Fatalf("requestIDFromContextOrHeader() = %q, want header-id", got)
		}
	})

	t.Run("nil request returns empty", func(t *testing.T) {
		if got := requestIDFromContextOrHeader(nil); got != "" {
			t.Fatalf("requestIDFromContextOrHeader(nil) = %q, want empty", got)
		}
	})
}

func TestHandleStreamingResponse_RecordsStreamingError(t *testing.T) {
	expectedErr := errors.New("upstream stream failed")
	logger := &capturingAuditLogger{
		config: auditlog.Config{Enabled: true},
	}

	e := echo.New()
	handler := NewHandler(&mockProvider{}, logger, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("X-Request-ID", "req-stream-1")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.Set(string(auditlog.LogEntryKey), &auditlog.LogEntry{
		ID:        "entry-1",
		Timestamp: time.Now(),
		RequestID: "req-stream-1",
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Data:      &auditlog.LogData{},
	})

	err := handler.translatedInference().handleStreamingResponse(c, nil, "gpt-4o-mini", "openai", "primary-openai", func() (io.ReadCloser, error) {
		return &erroringReadCloser{
			data: []byte("data: {\"id\":\"1\"}\n\n"),
			err:  expectedErr,
		}, nil
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if len(logger.entries) != 1 {
		t.Fatalf("expected 1 audit log entry, got %d", len(logger.entries))
	}

	entry := logger.entries[0]
	if entry.ErrorType != "stream_error" {
		t.Fatalf("expected stream_error, got %q", entry.ErrorType)
	}
	if entry.Data == nil || entry.Data.ErrorMessage != expectedErr.Error() {
		t.Fatalf("expected error message %q, got %+v", expectedErr.Error(), entry.Data)
	}
}

func TestHandleStreamingResponse_ClientDisconnectBeforeUpstream(t *testing.T) {
	e := echo.New()
	handler := NewHandler(&mockProvider{}, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // simulate client gone before streamFn returns
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	entry := &auditlog.LogEntry{
		ID:        "entry-cancel",
		Timestamp: time.Now(),
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Data:      &auditlog.LogData{},
	}
	c.Set(string(auditlog.LogEntryKey), entry)

	err := handler.translatedInference().handleStreamingResponse(c, nil, "gpt-4o-mini", "openai", "primary-openai", func() (io.ReadCloser, error) {
		return nil, context.Canceled
	})
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if !entry.Stream {
		t.Fatalf("expected entry.Stream=true, got false")
	}
	if entry.ErrorType != "client_disconnected" {
		t.Fatalf("expected error_type client_disconnected, got %q", entry.ErrorType)
	}
}

// At pre-flush dispatch time the only socket in play is the upstream
// provider connection, so EPIPE / ECONNRESET on the error from streamFn
// belong to the provider and must surface as upstream failures rather than
// be swallowed as client disconnects.
func TestHandleStreamingResponse_UpstreamResetIsNotClassifiedAsClientDisconnect(t *testing.T) {
	logger := &capturingAuditLogger{
		config: auditlog.Config{Enabled: true},
	}

	e := echo.New()
	handler := NewHandler(&mockProvider{}, logger, nil, nil)

	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "bare syscall.ECONNRESET", err: syscall.ECONNRESET},
		{name: "wrapped syscall.EPIPE", err: fmt.Errorf("dial upstream: %w", syscall.EPIPE)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			rec := httptest.NewRecorder()
			c := e.NewContext(req, rec)
			entry := &auditlog.LogEntry{
				ID:        "entry-upstream-reset",
				Timestamp: time.Now(),
				Method:    http.MethodPost,
				Path:      "/v1/chat/completions",
				Data:      &auditlog.LogData{},
			}
			c.Set(string(auditlog.LogEntryKey), entry)

			err := handler.translatedInference().handleStreamingResponse(c, nil, "gpt-4o-mini", "openai", "primary-openai", func() (io.ReadCloser, error) {
				return nil, tt.err
			})

			// handleStreamingResponse always swallows the error by writing a
			// JSON response via handleError; the gateway response must be the
			// upstream failure, not an empty 200.
			if err != nil {
				t.Fatalf("handler returned error: %v", err)
			}
			if rec.Code == http.StatusOK {
				t.Fatalf("upstream reset surfaced as 200 OK; want non-2xx, got body=%q", rec.Body.String())
			}
			if entry.ErrorType == "client_disconnected" {
				t.Fatalf("upstream reset misclassified as client_disconnected (err=%v)", tt.err)
			}
			if !entry.Stream {
				t.Fatalf("expected entry.Stream=true regardless of classification, got false")
			}
		})
	}
}

func TestRecordStreamingError_ClassifiesClientDisconnect(t *testing.T) {
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name     string
		ctx      context.Context
		err      error
		wantType string
	}{
		{
			name:     "explicit context.Canceled",
			ctx:      context.Background(),
			err:      context.Canceled,
			wantType: "client_disconnected",
		},
		{
			name:     "wrapped context.Canceled",
			ctx:      context.Background(),
			err:      fmt.Errorf("upstream send failed: %w", context.Canceled),
			wantType: "client_disconnected",
		},
		{
			name:     "syscall.EPIPE",
			ctx:      context.Background(),
			err:      syscall.EPIPE,
			wantType: "client_disconnected",
		},
		{
			name:     "wrapped syscall.EPIPE",
			ctx:      context.Background(),
			err:      fmt.Errorf("write to client: %w", syscall.EPIPE),
			wantType: "client_disconnected",
		},
		{
			name:     "syscall.ECONNRESET",
			ctx:      context.Background(),
			err:      syscall.ECONNRESET,
			wantType: "client_disconnected",
		},
		{
			name:     "canceled ctx racing real upstream error stays stream_error",
			ctx:      canceledCtx,
			err:      errors.New("upstream malformed"),
			wantType: "stream_error",
		},
		{
			name:     "clean ctx and generic error",
			ctx:      context.Background(),
			err:      errors.New("upstream malformed"),
			wantType: "stream_error",
		},
		{
			// Exercises the err==nil branch of isClientDisconnect and the
			// matching nil-guard fallback in recordStreamingError. Must not
			// panic and must record the context error as the message.
			name:     "canceled ctx with nil err",
			ctx:      canceledCtx,
			err:      nil,
			wantType: "client_disconnected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &auditlog.LogEntry{Data: &auditlog.LogData{}}
			recordStreamingError(entry, "gpt-4o-mini", "openai", "/v1/chat/completions", "req-"+tt.name, tt.ctx, tt.err)
			if entry.ErrorType != tt.wantType {
				t.Fatalf("error_type = %q, want %q", entry.ErrorType, tt.wantType)
			}

			wantMessage := ""
			switch {
			case tt.err != nil:
				wantMessage = tt.err.Error()
			case tt.ctx != nil && tt.ctx.Err() != nil:
				wantMessage = tt.ctx.Err().Error()
			}
			if entry.Data.ErrorMessage != wantMessage {
				t.Fatalf("error_message = %q, want %q", entry.Data.ErrorMessage, wantMessage)
			}
		})
	}
}

func TestChatCompletionStreaming_FlushesBeforeNextChunkArrives(t *testing.T) {
	secondChunkStarted := make(chan struct{})
	releaseSecondChunk := make(chan struct{})

	provider := &streamingProviderWithCustomReader{
		supportedModels: []string{"gpt-4o-mini"},
		reader: &delayedChunkReadCloser{
			chunks: []delayedChunk{
				{data: []byte("data: {\"id\":\"1\"}\n\n")},
				{
					data:    []byte("data: [DONE]\n\n"),
					started: secondChunkStarted,
					release: releaseSecondChunk,
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/v1/chat/completions", handler.ChatCompletion)

	srv := httptest.NewServer(e)
	defer srv.Close()

	reqBody := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	readResult := make(chan struct {
		n   int
		err error
		buf []byte
	}, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := resp.Body.Read(buf)
		readResult <- struct {
			n   int
			err error
			buf []byte
		}{n: n, err: err, buf: buf}
	}()

	select {
	case <-secondChunkStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for server to start reading the delayed second chunk")
	}

	var result struct {
		n   int
		err error
		buf []byte
	}
	select {
	case result = <-readResult:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first chunk to reach the client before releasing the second chunk")
	}

	close(releaseSecondChunk)

	if result.err != nil {
		t.Fatalf("read first chunk: %v", result.err)
	}

	firstChunk := string(result.buf[:result.n])
	if !strings.Contains(firstChunk, `"id":"1"`) {
		t.Fatalf("expected first streamed chunk before delayed tail, got %q", firstChunk)
	}
}

func TestHealth(t *testing.T) {
	e := echo.New()
	handler := NewHandler(&mockProvider{}, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.Health(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("expected ok status in body")
	}
}

func TestListModels(t *testing.T) {
	mock := &mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{
					ID:      "gpt-4o-mini",
					Object:  "model",
					Created: 1721172741,
					OwnedBy: "system",
				},
				{
					ID:      "gpt-4-turbo",
					Object:  "model",
					Created: 1712361441,
					OwnedBy: "system",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ListModels(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, `"object":"list"`) {
		t.Errorf("response missing object field, got: %s", body)
	}
	if !strings.Contains(body, "gpt-4o-mini") {
		t.Errorf("response missing gpt-4o-mini model, got: %s", body)
	}
	if !strings.Contains(body, "gpt-4-turbo") {
		t.Errorf("response missing gpt-4-turbo model, got: %s", body)
	}
}

func TestListModels_AnthropicDialect(t *testing.T) {
	mock := &mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{
					ID:       "gpt-4o-mini",
					Object:   "model",
					Created:  1721172741,
					OwnedBy:  "system",
					Metadata: &core.ModelMetadata{DisplayName: "GPT-4o mini"},
				},
				{
					ID:      "gpt-4-turbo",
					Object:  "model",
					Created: 1712361441,
					OwnedBy: "system",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	// The anthropic-version header marks an Anthropic SDK client; the shared
	// models route renders the Anthropic list shape for it.
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("anthropic-version", "2023-06-01")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.ListModels(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	var body struct {
		Data []struct {
			Type        string `json:"type"`
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
		HasMore bool    `json:"has_more"`
		FirstID *string `json:"first_id"`
		LastID  *string `json:"last_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(body.Data))
	}
	first := body.Data[0]
	if first.Type != "model" || first.ID != "gpt-4o-mini" || first.DisplayName != "GPT-4o mini" {
		t.Errorf("first model = %+v", first)
	}
	if first.CreatedAt != "2024-07-16T23:32:21Z" {
		t.Errorf("created_at = %q, want RFC3339", first.CreatedAt)
	}
	// Models without metadata fall back to the ID as display name.
	if body.Data[1].DisplayName != "gpt-4-turbo" {
		t.Errorf("fallback display_name = %q", body.Data[1].DisplayName)
	}
	if body.HasMore {
		t.Error("has_more should be false (single page)")
	}
	if body.FirstID == nil || *body.FirstID != "gpt-4o-mini" || body.LastID == nil || *body.LastID != "gpt-4-turbo" {
		t.Errorf("first_id/last_id = %v/%v", body.FirstID, body.LastID)
	}
}

func TestListModels_MergesExposedModelsWithoutAliasProviderDecorator(t *testing.T) {
	catalog := &aliasesTestCatalog{
		supported: map[string]bool{
			"openai/gpt-4o": true,
		},
		providerTypes: map[string]string{
			"openai/gpt-4o": "openai",
		},
		models: map[string]core.Model{
			"openai/gpt-4o": {ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
		},
	}
	service, err := virtualmodels.NewService(newAliasesTestStore(
		redirectVM("smart", "gpt-4o", "openai", true),
	), catalog, true)
	require.NoError(t, err)
	require.NoError(t, service.Refresh(context.Background()))

	mock := &mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{
					ID:      "gpt-4o",
					Object:  "model",
					Created: 1721172741,
					OwnedBy: "openai",
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)
	handler.exposedModelLister = service

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = handler.ListModels(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	require.Contains(t, body, `"id":"gpt-4o"`)
	require.Contains(t, body, `"id":"smart"`)
}

func TestListModels_KeepOnlyAliasesOmitsProviderModels(t *testing.T) {
	catalog := &aliasesTestCatalog{
		supported: map[string]bool{
			"openai/gpt-4o": true,
		},
		providerTypes: map[string]string{
			"openai/gpt-4o": "openai",
		},
		models: map[string]core.Model{
			"openai/gpt-4o": {ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
		},
	}
	service, err := virtualmodels.NewService(newAliasesTestStore(
		redirectVM("smart", "gpt-4o", "openai", true),
	), catalog, true)
	require.NoError(t, err)
	require.NoError(t, service.Refresh(context.Background()))

	mock := &mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)
	handler.exposedModelLister = service
	handler.keepOnlyAliasesAtModelsEndpoint = true

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err = handler.ListModels(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	var resp core.ModelsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Data, 1)
	require.Equal(t, "smart", resp.Data[0].ID)
}

func TestListModels_FiltersExposedModelsWhenAuthorizerIsPresent(t *testing.T) {
	mock := &mockProvider{
		modelsResponse: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
			},
		},
	}
	authorizer := &recordingModelAuthorizer{
		allow: func(selector core.ModelSelector) bool {
			return selector.QualifiedModel() != "openai/gpt-5"
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)
	handler.modelAuthorizer = authorizer
	handler.exposedModelLister = staticExposedModelLister{
		models: []core.Model{
			{ID: "openai/gpt-5", Object: "model", OwnedBy: "openai"},
			{ID: "openai/gpt-4o-mini", Object: "model", OwnedBy: "openai"},
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ListModels(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	require.Contains(t, body, `"id":"gpt-4o"`)
	require.Contains(t, body, `"id":"openai/gpt-4o-mini"`)
	require.NotContains(t, body, `"id":"openai/gpt-5"`)
}

func TestListModelsError(t *testing.T) {
	mock := &mockProvider{
		err: io.EOF, // Simulate an error
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ListModels(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("expected status 500, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "error") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

// Tests for typed error handling

func TestHandleError_ProviderError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewProviderError("openai", http.StatusBadGateway, "upstream error", nil),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "provider_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "upstream error") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

func TestHandleError_RateLimitError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewRateLimitError("openai", "rate limit exceeded"),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "rate_limit_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "rate limit exceeded") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

func TestHandleError_InvalidRequestError(t *testing.T) {
	param := "model"
	code := "model_not_found"
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewInvalidRequestError("invalid parameters", nil).WithParam(param).WithCode(code),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error body = %#v, want object", body["error"])
	}

	if errorBody["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", errorBody["type"])
	}
	if errorBody["message"] != "invalid parameters" {
		t.Errorf("error.message = %v, want invalid parameters", errorBody["message"])
	}
	if errorBody["param"] != param {
		t.Errorf("error.param = %v, want %v", errorBody["param"], param)
	}
	if errorBody["code"] != code {
		t.Errorf("error.code = %v, want %v", errorBody["code"], code)
	}
}

func TestHandleError_AuthenticationError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewAuthenticationError("openai", "invalid API key"),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "authentication_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "invalid API key") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

func TestHandleError_NotFoundError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewNotFoundError("model not found"),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status 404, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "not_found_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "model not found") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

func TestHandleError_StreamingError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             core.NewRateLimitError("openai", "rate limit exceeded during streaming"),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "stream": true, "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("expected status 429, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "rate_limit_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
}

func TestHandleError_UnexpectedErrorUsesOpenAISchema(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
		err:             errors.New("boom"),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "gpt-4o-mini", "messages": [{"role": "user", "content": "Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected status 500, got %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode error response: %v", err)
	}

	errorBody, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error body = %#v, want object", body["error"])
	}

	if errorBody["type"] != "provider_error" {
		t.Errorf("error.type = %v, want provider_error", errorBody["type"])
	}
	if errorBody["message"] != "an unexpected error occurred" {
		t.Errorf("error.message = %v, want unexpected error message", errorBody["message"])
	}
	if value, ok := errorBody["param"]; !ok || value != nil {
		t.Errorf("error.param = %v, want nil", value)
	}
	if value, ok := errorBody["code"]; !ok || value != nil {
		t.Errorf("error.code = %v, want nil", value)
	}
}

func TestChatCompletion_InvalidJSON(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"gpt-4o-mini"},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{invalid json}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "invalid_request_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "invalid request body") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

func TestChatCompletion_InvalidContentType(t *testing.T) {
	provider := &capturingProvider{
		supportedModels: []string{"gpt-4o-mini"},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	reqBody := `{"model":"gpt-4o-mini","messages":[{"role":"user","content":123}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d (%s)", rec.Code, rec.Body.String())
	}
	if provider.capturedChatReq != nil {
		t.Fatal("provider should not have been called for invalid content")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "invalid request body") {
		t.Fatalf("response should contain invalid request message, got: %s", body)
	}
	if !strings.Contains(body, "string or array of content parts") {
		t.Fatalf("response should mention supported content types, got: %s", body)
	}
}

func TestEmbeddings(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"text-embedding-3-small"},
		embeddingResponse: &core.EmbeddingResponse{
			Object: "list",
			Data: []core.EmbeddingData{
				{Object: "embedding", Embedding: json.RawMessage(`[0.1,0.2,0.3]`), Index: 0},
			},
			Model:    "text-embedding-3-small",
			Provider: "openai",
			Usage:    core.EmbeddingUsage{PromptTokens: 5, TotalTokens: 5},
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "text-embedding-3-small", "input": "hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.Embeddings(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "text-embedding-3-small") {
		t.Errorf("response missing model, got: %s", body)
	}
	if !strings.Contains(body, "embedding") {
		t.Errorf("response missing embedding data, got: %s", body)
	}
}

func TestEmbeddings_InvalidJSON(t *testing.T) {
	mock := &mockProvider{supportedModels: []string{"text-embedding-3-small"}}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{bad json}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.Embeddings(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}
}

func TestEmbeddings_ProviderReturnsError(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"text-embedding-3-small"},
		embeddingErr:    core.NewInvalidRequestError("embeddings not supported by this provider", nil),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	reqBody := `{"model": "text-embedding-3-small", "input": "hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.Embeddings(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "embeddings not supported") {
		t.Errorf("expected error message about embeddings, got: %s", body)
	}
}

func TestEmbeddings_WithUsageTracking(t *testing.T) {
	mock := &mockProvider{
		supportedModels: []string{"text-embedding-3-small"},
		embeddingResponse: &core.EmbeddingResponse{
			Object: "list",
			Data: []core.EmbeddingData{
				{Object: "embedding", Embedding: json.RawMessage(`[0.1,0.2,0.3]`), Index: 0},
			},
			Model: "provider-canonical-embedding",
			Usage: core.EmbeddingUsage{PromptTokens: 10, TotalTokens: 10},
		},
	}

	var capturedEntry *usage.UsageEntry
	usageLog := &capturingUsageLogger{
		config:   usage.Config{Enabled: true},
		captured: &capturedEntry,
	}

	inputPrice := 0.02
	resolver := &mockPricingResolver{
		pricing: &core.ModelPricing{
			Currency:     "USD",
			InputPerMtok: &inputPrice,
		},
	}

	e := echo.New()
	handler := NewHandler(mock, nil, usageLog, resolver)

	reqBody := `{"model": "text-embedding-3-small", "input": "hello world"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "test-req-embed-usage")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.Embeddings(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	if capturedEntry == nil {
		t.Fatal("expected usage entry to be captured, got nil")
	}
	if capturedEntry.InputTokens != 10 {
		t.Errorf("InputTokens = %d, want 10", capturedEntry.InputTokens)
	}
	if capturedEntry.RequestID != "test-req-embed-usage" {
		t.Errorf("RequestID = %q, want %q", capturedEntry.RequestID, "test-req-embed-usage")
	}
	if resolver.model != "text-embedding-3-small" {
		t.Errorf("pricing resolver model = %q, want requested model", resolver.model)
	}
	if capturedEntry.InputCost == nil || *capturedEntry.InputCost == 0 {
		t.Error("expected non-zero InputCost from pricing resolver")
	}
}

func TestListModels_TypedError(t *testing.T) {
	mock := &mockProvider{
		err: core.NewProviderError("openai", http.StatusBadGateway, "failed to list models", nil),
	}

	e := echo.New()
	handler := NewHandler(mock, nil, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ListModels(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected status 502, got %d", rec.Code)
	}

	body := rec.Body.String()
	if !strings.Contains(body, "provider_error") {
		t.Errorf("response should contain error type, got: %s", body)
	}
	if !strings.Contains(body, "failed to list models") {
		t.Errorf("response should contain error message, got: %s", body)
	}
}

















func TestStreamingChatCompletion_InjectsStreamOptions(t *testing.T) {
	streamData := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	provider := &capturingProvider{
		supportedModels: []string{"gpt-4o-mini"},
		providerTypes: map[string]string{
			"gpt-4o-mini": "openai",
		},
		streamData: streamData,
	}

	usageLog := &mockUsageLogger{
		config: usage.Config{
			Enabled:                   true,
			EnforceReturningUsageData: true,
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, usageLog, nil)

	// Streaming ChatCompletion request SHOULD have StreamOptions injected
	reqBody := `{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	err := handler.ChatCompletion(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	if provider.lastPassthroughReq != nil {
		t.Fatal("lastPassthroughReq != nil, want usage-enforced streaming to stay on translated stream path")
	}

	if provider.capturedChatReq.StreamOptions == nil {
		t.Fatal("ChatCompletion streaming should have StreamOptions injected")
	}

	if !provider.capturedChatReq.StreamOptions.IncludeUsage {
		t.Error("ChatCompletion streaming should have IncludeUsage=true")
	}
}



func TestProviderPassthrough_OpenAI(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusAccepted,
			Headers: map[string][]string{
				"Content-Type":   {"application/json"},
				"X-Upstream":     {"openai"},
				"Set-Cookie":     {"session=secret"},
				"Connection":     {"X-Upstream-Hop, Keep-Alive"},
				"X-Upstream-Hop": {"secret"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/responses?api-version=2026-03-10", strings.NewReader(`{"foo":"bar"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer user-secret")
	req.Header.Set("Cookie", "session=user-secret")
	req.Header.Set("Forwarded", "for=10.0.0.1")
	req.Header.Set("OpenAI-Beta", "responses=v1")
	req.Header.Set("X-Forwarded-For", "10.0.0.1")
	req.Header.Set("Connection", "X-Debug, keep-alive")
	req.Header.Set("X-Debug", "secret")
	req.Header.Set("X-Request-ID", "req_123")
	req.Header.Set(core.UserPathHeader, "/team/a/user")

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusAccepted)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Fatalf("body = %q", got)
	}
	if got := rec.Header().Get("X-Upstream"); got != "openai" {
		t.Fatalf("X-Upstream = %q, want openai", got)
	}
	if got := rec.Header().Get("Set-Cookie"); got != "" {
		t.Fatalf("Set-Cookie should not be forwarded, got %q", got)
	}
	if got := rec.Header().Get("X-Upstream-Hop"); got != "" {
		t.Fatalf("hop-by-hop header should not be forwarded, got %q", got)
	}
	if provider.lastPassthroughProvider != "openai" {
		t.Fatalf("providerType = %q, want openai", provider.lastPassthroughProvider)
	}
	if provider.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil")
	}
	if got := provider.lastPassthroughReq.Endpoint; got != "responses?api-version=2026-03-10" {
		t.Fatalf("endpoint = %q", got)
	}
	if got := readPassthroughRequestBody(t, provider.lastPassthroughReq.Body); got != `{"foo":"bar"}` {
		t.Fatalf("body = %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("Authorization"); got != "" {
		t.Fatalf("authorization header should not be forwarded, got %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("Cookie"); got != "" {
		t.Fatalf("cookie header should not be forwarded, got %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("Forwarded"); got != "" {
		t.Fatalf("forwarded header should not be forwarded, got %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("X-Forwarded-For"); got != "" {
		t.Fatalf("x-forwarded-for header should not be forwarded, got %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("X-Debug"); got != "" {
		t.Fatalf("connection-nominated header should not be forwarded, got %q", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("OpenAI-Beta"); got != "responses=v1" {
		t.Fatalf("OpenAI-Beta = %q, want responses=v1", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get("X-Request-ID"); got != "req_123" {
		t.Fatalf("X-Request-ID = %q, want req_123", got)
	}
	if got := provider.lastPassthroughReq.Headers.Get(core.UserPathHeader); got != "" {
		t.Fatalf("%s should not be forwarded, got %q", core.UserPathHeader, got)
	}
}

func TestProviderPassthrough_PrefersContextRequestID(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/responses", strings.NewReader(`{}`))
	req = req.WithContext(core.WithRequestID(req.Context(), "ctx_req_123"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "header_req_456")

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if provider.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil")
	}
	if got := provider.lastPassthroughReq.Headers.Get("X-Request-ID"); got != "ctx_req_123" {
		t.Fatalf("X-Request-ID = %q, want ctx_req_123", got)
	}
}

func TestProviderPassthrough_NormalizesErrorResponse(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusNotFound,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
				"X-Upstream":   {"openai"},
			},
			Body: io.NopCloser(strings.NewReader(`{"error":{"message":"upstream missing","type":"invalid_request_error"}}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/responses", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusNotFound)
	}
	if got := rec.Header().Get("X-Upstream"); got != "" {
		t.Fatalf("X-Upstream should not be forwarded on normalized errors, got %q", got)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"message":"upstream missing"`) || !strings.Contains(body, `"error"`) {
		t.Fatalf("unexpected error body: %s", body)
	}
}

func TestProviderPassthrough_LLMDDroppedReasonOnNormalizedError(t *testing.T) {
	for _, path := range []string{
		"/p/llmd/tokenize",
		"/p/llmd/v1/chat/completions",
	} {
		t.Run(path, func(t *testing.T) {
			provider := &mockProvider{
				passthroughResponse: &core.PassthroughResponse{
					StatusCode: http.StatusTooManyRequests,
					Headers: http.Header{
						"Content-Type":          {"application/json"},
						llmdDroppedReasonHeader: {"rejected-saturated"},
						"X-Upstream":            {"must-not-leak"},
						"Set-Cookie":            {"session=secret"},
					},
					Body: io.NopCloser(strings.NewReader(`{"error":{"message":"request dropped","type":"rate_limit_error"}}`)),
				},
			}

			e := echo.New()
			handler := NewHandler(provider, nil, nil, nil)
			e.POST("/p/:provider/*", handler.ProviderPassthrough)

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
			}
			if got := rec.Header().Get(llmdDroppedReasonHeader); got != "rejected-saturated" {
				t.Fatalf("%s = %q, want rejected-saturated", llmdDroppedReasonHeader, got)
			}
			if got := rec.Header().Get("X-Upstream"); got != "" {
				t.Fatalf("X-Upstream should not be forwarded, got %q", got)
			}
			if got := rec.Header().Get("Set-Cookie"); got != "" {
				t.Fatalf("Set-Cookie should not be forwarded, got %q", got)
			}
			if body := rec.Body.String(); !strings.Contains(body, `"message":"request dropped"`) {
				t.Fatalf("unexpected error body: %s", body)
			}
		})
	}
}

func TestProviderPassthrough_OpenAIV1AliasNormalizesByDefault(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil")
	}
	if got := provider.lastPassthroughReq.Endpoint; got != "chat/completions" {
		t.Fatalf("endpoint = %q, want chat/completions", got)
	}
}

func TestProviderPassthrough_AnthropicV1AliasNormalizesByDefault(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/anthropic/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil")
	}
	if got := provider.lastPassthroughReq.Endpoint; got != "messages" {
		t.Fatalf("endpoint = %q, want messages", got)
	}
}

func TestProviderPassthrough_UsesPassthroughModelForAuditEntry(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
		providerTypes: map[string]string{"openai_test/gpt-5-mini": "openai"},
		providerNames: map[string]string{"openai_test/gpt-5-mini": "openai_test"},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)

	req := httptest.NewRequest(http.MethodPost, "/p/openai_test/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(core.WithWorkflow(req.Context(), &core.Workflow{
		Mode:         core.ExecutionModePassthrough,
		ProviderType: "openai",
		Passthrough: &core.PassthroughRouteInfo{
			Provider:           "openai_test",
			RawEndpoint:        "chat/completions",
			NormalizedEndpoint: "chat/completions",
			Model:              "gpt-5-mini",
			AuditPath:          "/v1/chat/completions",
		},
	}))

	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	entry := &auditlog.LogEntry{}
	c.Set(string(auditlog.LogEntryKey), entry)

	if err := handler.ProviderPassthrough(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if entry.RequestedModel != "gpt-5-mini" {
		t.Fatalf("audit entry requested model = %q, want gpt-5-mini", entry.RequestedModel)
	}
	if entry.Provider != "openai" {
		t.Fatalf("audit entry provider = %q, want openai", entry.Provider)
	}
	if entry.ProviderName != "openai_test" {
		t.Fatalf("audit entry provider name = %q, want openai_test", entry.ProviderName)
	}
}

func TestProviderPassthrough_UsesConfiguredProviderNameForAccessValidation(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
		providerTypes: map[string]string{
			"openai_test/gpt-5-mini": "openai",
		},
		providerNames: map[string]string{
			"openai_test/gpt-5-mini": "openai_test",
		},
	}
	authorizer := &recordingModelAuthorizer{}

	e := echo.New()
	handler := newHandlerWithAuthorizer(provider, nil, nil, nil, nil, authorizer, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/p/openai_test/chat/completions", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(core.WithWorkflow(req.Context(), &core.Workflow{
		Mode:         core.ExecutionModePassthrough,
		ProviderType: "openai",
		Passthrough: &core.PassthroughRouteInfo{
			Provider:           "openai_test",
			RawEndpoint:        "chat/completions",
			NormalizedEndpoint: "chat/completions",
			Model:              "gpt-5-mini",
			AuditPath:          "/p/openai_test/chat/completions",
		},
	}))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.ProviderPassthrough(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.lastPassthroughProvider != "openai" {
		t.Fatalf("providerType = %q, want openai", provider.lastPassthroughProvider)
	}
	if authorizer.lastSelector.Provider != "openai_test" || authorizer.lastSelector.Model != "gpt-5-mini" {
		t.Fatalf("validated selector = %#v, want openai_test/gpt-5-mini", authorizer.lastSelector)
	}
}

func TestProviderPassthrough_FallsBackFromProviderTypeToCanonicalProviderNameForAccessValidation(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
		providerTypes: map[string]string{
			"openai_test/gpt-5-mini": "openai",
		},
		providerNames: map[string]string{
			"openai_test/gpt-5-mini": "openai_test",
		},
	}
	authorizer := &recordingModelAuthorizer{}

	e := echo.New()
	handler := newHandlerWithAuthorizer(provider, nil, nil, nil, nil, authorizer, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/p/openai/chat/completions", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(core.WithWorkflow(req.Context(), &core.Workflow{
		Mode:         core.ExecutionModePassthrough,
		ProviderType: "openai",
		Passthrough: &core.PassthroughRouteInfo{
			Provider:           "openai",
			RawEndpoint:        "chat/completions",
			NormalizedEndpoint: "chat/completions",
			Model:              "gpt-5-mini",
			AuditPath:          "/p/openai/chat/completions",
		},
	}))
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)

	if err := handler.ProviderPassthrough(c); err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.lastPassthroughProvider != "openai" {
		t.Fatalf("providerType = %q, want openai", provider.lastPassthroughProvider)
	}
	if authorizer.lastSelector.Provider != "openai_test" || authorizer.lastSelector.Model != "gpt-5-mini" {
		t.Fatalf("validated selector = %#v, want openai_test/gpt-5-mini", authorizer.lastSelector)
	}
}

func TestProviderPassthrough_V1AliasDisabledReturnsBadRequest(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"application/json"},
			},
			Body: io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	handler.normalizePassthroughV1Prefix = false
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "v1 alias is disabled") {
		t.Fatalf("body = %q, want v1 alias error", rec.Body.String())
	}
	if provider.lastPassthroughReq != nil {
		t.Fatalf("provider should not have been called, got endpoint %q", provider.lastPassthroughReq.Endpoint)
	}
}

func TestProviderPassthrough_AnthropicStream(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: &chunkedReadCloser{
				chunks: [][]byte{
					[]byte("event: message_start\n"),
					[]byte("data: {\"type\":\"message_start\"}\n\n"),
				},
			},
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/anthropic/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content-type = %q", got)
	}
	if rec.flushes == 0 {
		t.Fatal("expected streaming response to flush")
	}
	if got := rec.Body.String(); !strings.Contains(got, "message_start") {
		t.Fatalf("unexpected stream body: %q", got)
	}
}

func TestProviderPassthrough_StreamWithoutObserversClosesUpstreamBodyOnce(t *testing.T) {
	body := &closeCountingReadCloser{
		ReadCloser: &chunkedReadCloser{
			chunks: [][]byte{
				[]byte("event: message_start\n"),
				[]byte("data: {\"type\":\"message_start\"}\n\n"),
			},
		},
	}
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: body,
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/anthropic/messages", strings.NewReader(`{"model":"claude-sonnet-4-5"}`))
	req.Header.Set("Content-Type", "application/json")

	rec := &flushCountingRecorder{ResponseRecorder: httptest.NewRecorder()}
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body.closes != 1 {
		t.Fatalf("Close calls = %d, want 1", body.closes)
	}
}

func TestProviderPassthrough_OpenAIStreamWritesUsageEntry(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"resp-123\",\"model\":\"gpt-5-mini\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
		providerTypes: map[string]string{"openai_test/gpt-5-mini": "openai"},
		providerNames: map[string]string{"openai_test/gpt-5-mini": "openai_test"},
	}
	usageLog := &collectingUsageLogger{
		config: usage.Config{Enabled: true},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, usageLog, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai_test/responses", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "req-pass-stream-usage")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(usageLog.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
	}

	entry := usageLog.entries[0]
	if entry.Provider != "openai" {
		t.Fatalf("Provider = %q, want openai", entry.Provider)
	}
	if entry.ProviderName != "openai_test" {
		t.Fatalf("ProviderName = %q, want openai_test", entry.ProviderName)
	}
	if entry.Endpoint != "/p/openai_test/responses" {
		t.Fatalf("Endpoint = %q, want /p/openai_test/responses", entry.Endpoint)
	}
	if entry.Model != "gpt-5-mini" {
		t.Fatalf("Model = %q, want gpt-5-mini", entry.Model)
	}
	if entry.TotalTokens != 10 {
		t.Fatalf("TotalTokens = %d, want 10", entry.TotalTokens)
	}
	if entry.RequestID != "req-pass-stream-usage" {
		t.Fatalf("RequestID = %q, want req-pass-stream-usage", entry.RequestID)
	}
}

func TestProviderPassthrough_OpenAIStreamUsageKeepsClientVisibleRoute(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers: map[string][]string{
				"Content-Type": {"text/event-stream"},
			},
			Body: io.NopCloser(strings.NewReader(
				"data: {\"id\":\"resp-123\",\"model\":\"gpt-5-mini\",\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}\n\n" +
					"data: [DONE]\n\n",
			)),
		},
	}
	usageLog := &collectingUsageLogger{
		config: usage.Config{Enabled: true},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, usageLog, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/openai/v1/responses", strings.NewReader(`{"model":"gpt-5-mini"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "req-pass-stream-visible-path")
	req = req.WithContext(core.WithWorkflow(req.Context(), &core.Workflow{
		Mode:         core.ExecutionModePassthrough,
		ProviderType: "openai",
		Passthrough: &core.PassthroughRouteInfo{
			Provider:           "openai",
			RawEndpoint:        "v1/responses",
			NormalizedEndpoint: "responses",
			AuditPath:          "/v1/responses",
			Model:              "gpt-5-mini",
		},
	}))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(usageLog.entries) != 1 {
		t.Fatalf("usage entries = %d, want 1", len(usageLog.entries))
	}
	if got := usageLog.entries[0].Endpoint; got != "/p/openai/v1/responses" {
		t.Fatalf("Endpoint = %q, want /p/openai/v1/responses", got)
	}
}

func TestPassthroughStreamAuditPath_NormalizesKnownEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		requestPath string
		provider    string
		endpoint    string
		want        string
	}{
		{
			name:        "openai responses",
			requestPath: "/p/openai/responses",
			provider:    "openai",
			endpoint:    "responses?trace=1",
			want:        "/v1/responses",
		},
		{
			name:        "anthropic messages",
			requestPath: "/p/anthropic/messages",
			provider:    "anthropic",
			endpoint:    "messages",
			want:        "/v1/messages",
		},
		{
			name:        "unknown endpoint falls back",
			requestPath: "/p/openai/unknown",
			provider:    "openai",
			endpoint:    "unknown",
			want:        "/p/openai/unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := passthroughStreamAuditPath(tt.requestPath, tt.provider, tt.endpoint); got != tt.want {
				t.Fatalf("passthroughStreamAuditPath(%q, %q, %q) = %q, want %q", tt.requestPath, tt.provider, tt.endpoint, got, tt.want)
			}
		})
	}
}

func TestProviderPassthrough_RejectsUnsupportedProvider(t *testing.T) {
	provider := &mockProvider{}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/groq/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `provider passthrough for \"groq\" is not enabled`) {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "anthropic, deepseek, hetzner, kilo, llamacpp, llmd, openai, openrouter, sglang, vllm, zai") {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}

func TestProviderPassthrough_ChutesRequiresExplicitOptIn(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	blockedReq := httptest.NewRequest(http.MethodPost, "/p/chutes/provider-native/admin/keys", strings.NewReader(`{}`))
	blockedRec := httptest.NewRecorder()
	e.ServeHTTP(blockedRec, blockedReq)

	if blockedRec.Code != http.StatusBadRequest {
		t.Fatalf("default status = %d, want 400: %s", blockedRec.Code, blockedRec.Body.String())
	}
	if provider.lastPassthroughReq != nil {
		t.Fatal("default Chutes passthrough reached provider, want rejection before forwarding")
	}

	handler.setEnabledPassthroughProviders([]string{"chutes"})
	req := httptest.NewRequest(http.MethodPost, "/p/chutes/chat/completions", strings.NewReader(`{"model":"Qwen/Qwen3-32B-TEE"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("opt-in status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if provider.lastPassthroughProvider != "chutes" {
		t.Fatalf("providerType = %q, want chutes", provider.lastPassthroughProvider)
	}
	if provider.lastPassthroughReq == nil || provider.lastPassthroughReq.Endpoint != "chat/completions" {
		t.Fatalf("passthrough request = %+v, want chat/completions endpoint", provider.lastPassthroughReq)
	}
}

func TestProviderPassthrough_UsesConfiguredSupportedProviders(t *testing.T) {
	provider := &mockProvider{
		passthroughResponse: &core.PassthroughResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		},
	}

	e := echo.New()
	handler := NewHandler(provider, nil, nil, nil)
	handler.setEnabledPassthroughProviders([]string{"groq"})
	e.POST("/p/:provider/*", handler.ProviderPassthrough)

	req := httptest.NewRequest(http.MethodPost, "/p/groq/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if provider.lastPassthroughProvider != "groq" {
		t.Fatalf("providerType = %q, want groq", provider.lastPassthroughProvider)
	}
	if provider.lastPassthroughReq == nil {
		t.Fatal("lastPassthroughReq = nil")
	}
	if got := provider.lastPassthroughReq.Endpoint; got != "chat/completions" {
		t.Fatalf("endpoint = %q, want chat/completions", got)
	}
	if got := readPassthroughRequestBody(t, provider.lastPassthroughReq.Body); got != `{}` {
		t.Fatalf("body = %q, want {}", got)
	}
	if got := rec.Body.String(); !strings.Contains(got, `"ok":true`) {
		t.Fatalf("unexpected error body: %s", rec.Body.String())
	}
}


// staticPipelineResolver returns a fixed guardrails pipeline regardless of
// context, letting tests drive the production WorkflowRequestPatcher /
// WorkflowBatchPreparer with an explicit pipeline.
type staticPipelineResolver struct{ pipeline *guardrails.Pipeline }

func (s staticPipelineResolver) PipelineForContext(context.Context) *guardrails.Pipeline {
	return s.pipeline
}
type mockUsageLogger struct {
	config usage.Config
}

func (m *mockUsageLogger) Write(_ *usage.UsageEntry) {}
func (m *mockUsageLogger) Config() usage.Config      { return m.config }
func (m *mockUsageLogger) Close() error              { return nil }

type capturingUsageLogger struct {
	config   usage.Config
	captured **usage.UsageEntry
}

func (c *capturingUsageLogger) Write(entry *usage.UsageEntry) { *c.captured = entry }
func (c *capturingUsageLogger) Config() usage.Config          { return c.config }
func (c *capturingUsageLogger) Close() error                  { return nil }

type collectingUsageLogger struct {
	config  usage.Config
	entries []*usage.UsageEntry
}

func (c *collectingUsageLogger) Write(entry *usage.UsageEntry) {
	if entry == nil {
		return
	}
	c.entries = append(c.entries, entry)
}

func (c *collectingUsageLogger) Config() usage.Config { return c.config }
func (c *collectingUsageLogger) Close() error         { return nil }

type mockPricingResolver struct {
	pricing  *core.ModelPricing
	model    string
	provider string
}

func (m *mockPricingResolver) ResolvePricing(model, provider string) *core.ModelPricing {
	m.model = model
	m.provider = provider
	return m.pricing
}

// capturingProvider is a mockProvider that captures the request passed to StreamResponses/StreamChatCompletion.
type capturingProvider struct {
	mockProvider
	capturedChatCtx      context.Context
	capturedChatReq      *core.ChatRequest
	capturedResponsesReq *core.ResponsesRequest
	capturedEmbeddingReq *core.EmbeddingRequest
}

func (c *capturingProvider) ChatCompletion(ctx context.Context, req *core.ChatRequest) (*core.ChatResponse, error) {
	c.capturedChatCtx = ctx
	c.capturedChatReq = req
	if c.err != nil {
		return nil, c.err
	}
	return c.response, nil
}

func (c *capturingProvider) StreamChatCompletion(ctx context.Context, req *core.ChatRequest) (io.ReadCloser, error) {
	c.capturedChatCtx = ctx
	c.capturedChatReq = req
	return io.NopCloser(strings.NewReader(c.streamData)), nil
}

func (c *capturingProvider) Responses(_ context.Context, req *core.ResponsesRequest) (*core.ResponsesResponse, error) {
	c.capturedResponsesReq = req
	if c.err != nil {
		return nil, c.err
	}
	return c.responsesResponse, nil
}

func (c *capturingProvider) StreamResponses(_ context.Context, req *core.ResponsesRequest) (io.ReadCloser, error) {
	c.capturedResponsesReq = req
	return io.NopCloser(strings.NewReader(c.streamData)), nil
}



func (c *capturingProvider) Embeddings(_ context.Context, req *core.EmbeddingRequest) (*core.EmbeddingResponse, error) {
	c.capturedEmbeddingReq = req
	if c.embeddingErr != nil {
		return nil, c.embeddingErr
	}
	if c.err != nil {
		return nil, c.err
	}
	return c.embeddingResponse, nil
}

