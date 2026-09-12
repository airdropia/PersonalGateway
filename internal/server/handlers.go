// Package server provides HTTP handlers and server setup for the LLM gateway.
package server

import (
	"net/http"
	"strings"
	"sync"

	"github.com/labstack/echo/v5"

	"github.com/airdropia/pgw/internal/anthropicapi"
	"github.com/airdropia/pgw/internal/auditlog"
	"github.com/airdropia/pgw/internal/core"
	"github.com/airdropia/pgw/internal/mcpgateway"
	"github.com/airdropia/pgw/internal/responsecache"
	"github.com/airdropia/pgw/internal/usage"
)

// Handler holds the HTTP handlers
type Handler struct {
	provider                        core.RoutableProvider
	modelResolver                   RequestModelResolver
	modelAuthorizer                 RequestModelAuthorizer
	failoverResolver                RequestFailoverResolver
	workflowPolicyResolver          RequestWorkflowPolicyResolver
	translatedRequestPatcher        TranslatedRequestPatcher
	exposedModelLister              ExposedModelLister
	keepOnlyAliasesAtModelsEndpoint bool
	logger                          auditlog.LoggerInterface
	usageLogger                     usage.LoggerInterface
	budgetChecker                   BudgetChecker
	rateLimiter                     RateLimiter
	usageSummarizer                 UsageSummarizer
	userPathHeaderName              string
	pricingResolver                 usage.PricingResolver
	// storesMu guards translatedSvc wiring.
	storesMu                     sync.RWMutex
	normalizePassthroughV1Prefix bool
	enabledPassthroughProviders  map[string]struct{}
	mcpEnabled                   bool
	mcpGateway                   *mcpgateway.Service
	responseCache                *responsecache.ResponseCacheMiddleware
	guardrailsHash               string
	storageProbe                 ReadinessProbe
	cacheProbe                   ReadinessProbe

	translatedSvc     *translatedInferenceService // snapshot of handler fields at first use; server.New sets cache/hash before traffic
	translatedSvcOnce sync.Once
}

// newHandlerWithAuthorizer creates a new handler with the given routable
// provider (typically the Router) and optional resolvers.
func newHandlerWithAuthorizer(
	provider core.RoutableProvider,
	logger auditlog.LoggerInterface,
	usageLogger usage.LoggerInterface,
	pricingResolver usage.PricingResolver,
	modelResolver RequestModelResolver,
	modelAuthorizer RequestModelAuthorizer,
	workflowPolicyResolver RequestWorkflowPolicyResolver,
	failoverResolver RequestFailoverResolver,
	translatedRequestPatcher TranslatedRequestPatcher,
) *Handler {
	return &Handler{
		provider:                 provider,
		modelResolver:            modelResolver,
		modelAuthorizer:          modelAuthorizer,
		failoverResolver:         failoverResolver,
		workflowPolicyResolver:   workflowPolicyResolver,
		translatedRequestPatcher: translatedRequestPatcher,
		logger:                   logger,
		usageLogger:              usageLogger,
		pricingResolver:          pricingResolver,
		normalizePassthroughV1Prefix:    true,
		enabledPassthroughProviders:     normalizeEnabledPassthroughProviders(defaultEnabledPassthroughProviders),
	}
}

func (h *Handler) translatedInference() *translatedInferenceService {
	h.translatedSvcOnce.Do(func() {
		s := &translatedInferenceService{
			provider:                 h.provider,
			modelResolver:            h.modelResolver,
			modelAuthorizer:          h.modelAuthorizer,
			workflowPolicyResolver:   h.workflowPolicyResolver,
			failoverResolver:         h.failoverResolver,
			translatedRequestPatcher: h.translatedRequestPatcher,
			logger:                   h.logger,
			usageLogger:              h.usageLogger,
			budgetChecker:            h.budgetChecker,
			rateLimiter:              h.rateLimiter,
			pricingResolver:          h.pricingResolver,
			responseCache:            h.responseCache,
			guardrailsHash:           h.guardrailsHash,
		}
		s.initHandlers()
		h.storesMu.Lock()
		h.translatedSvc = s
		h.storesMu.Unlock()
	})
	h.storesMu.RLock()
	defer h.storesMu.RUnlock()
	return h.translatedSvc
}

func (h *Handler) mcp() *mcpService {
	var logBodies bool
	if h.logger != nil {
		logBodies = h.logger.Config().LogBodies
	}
	return &mcpService{
		gateway:       h.mcpGateway,
		budgetChecker: h.budgetChecker,
		rateLimiter:   h.rateLimiter,
		enabled:       h.mcpEnabled && h.mcpGateway != nil,
		logBodies:     logBodies,
	}
}

func (h *Handler) passthrough() *passthroughService {
	return &passthroughService{
		provider:                     h.provider,
		modelAuthorizer:              h.modelAuthorizer,
		logger:                       h.logger,
		usageLogger:                  h.usageLogger,
		budgetChecker:                h.budgetChecker,
		rateLimiter:                  h.rateLimiter,
		pricingResolver:              h.pricingResolver,
		normalizePassthroughV1Prefix: h.normalizePassthroughV1Prefix,
		enabledPassthroughProviders:  h.enabledPassthroughProviders,
	}
}

// ProviderPassthrough handles opaque provider-native requests under /p/{provider}/{endpoint}.
//
// OpenAI and Anthropic are the first-class providers in this ADR-0002 slice. Other
// providers are intentionally deferred until they fit the same low-friction opaque path.
//
// @Summary      Provider passthrough
// @Description  Runtime-configurable passthrough endpoint under /p/{provider}/{endpoint}; enabled by default via server.enable_passthrough_routes. The endpoint path is opaque and may proxy JSON, binary, or SSE responses with upstream status codes preserved. For multi-segment provider endpoints, clients that rely on OpenAPI-generated path handling should URL-encode embedded slashes in the endpoint parameter. A leading v1/ segment is normalized away by default so /p/{provider}/v1/... and /p/{provider}/... map to the same upstream path relative to the provider base URL.
// @Tags         passthrough
// @Accept       json
// @Accept       mpfd
// @Produce      json
// @Produce      application/octet-stream
// @Produce      text/event-stream
// @Security     BearerAuth
// @Param        provider  path      string  true  "Provider type"
// @Param        endpoint  path      string  true  "Provider-native endpoint path relative to the provider base URL. URL-encode embedded / characters when using generated clients."
// @Success      200       {file}    file    "Opaque upstream response body"
// @Success      201       {file}    file    "Opaque upstream response body"
// @Success      202       {file}    file    "Opaque upstream response body"
// @Success      204       {string}  string  "No Content passthrough response"
// @Failure      400       {object}  core.OpenAIErrorEnvelope
// @Failure      401       {object}  core.OpenAIErrorEnvelope
// @Failure      502       {object}  core.OpenAIErrorEnvelope
// @Router       /p/{provider}/{endpoint} [get]
// @Router       /p/{provider}/{endpoint} [post]
// @Router       /p/{provider}/{endpoint} [put]
// @Router       /p/{provider}/{endpoint} [patch]
// @Router       /p/{provider}/{endpoint} [delete]
// @Router       /p/{provider}/{endpoint} [head]
// @Router       /p/{provider}/{endpoint} [options]
func (h *Handler) ProviderPassthrough(c *echo.Context) error {
	return h.passthrough().ProviderPassthrough(c)
}

// MCP handles the aggregated MCP endpoint at /mcp.
//
// @Summary      MCP gateway (aggregated)
// @Description  Streamable-HTTP MCP endpoint aggregating every configured upstream MCP server visible to the caller. Tools and prompts are namespaced as {slug}_{name}. POST carries JSON-RPC messages, GET opens the server-notification SSE stream, DELETE ends the session. The X-MCP-Servers request header optionally narrows the visible servers to a comma-separated subset of server slugs.
// @Tags         mcp
// @Accept       json
// @Produce      json
// @Produce      text/event-stream
// @Security     BearerAuth
// @Success      200  {object}  map[string]interface{}  "JSON-RPC response or SSE stream"
// @Failure      401  {object}  core.OpenAIErrorEnvelope
// @Failure      429  {object}  core.OpenAIErrorEnvelope
// @Failure      501  {object}  core.OpenAIErrorEnvelope
// @Router       /mcp [post]
// @Router       /mcp [get]
// @Router       /mcp [delete]
func (h *Handler) MCP(c *echo.Context) error {
	return h.mcp().handle(c, "")
}

// MCPServer handles the per-server MCP endpoints at /mcp/{server}.
//
// @Summary      MCP gateway (single server)
// @Description  Streamable-HTTP MCP endpoint exposing one configured upstream MCP server with original (un-prefixed) tool names.
// @Tags         mcp
// @Accept       json
// @Produce      json
// @Produce      text/event-stream
// @Security     BearerAuth
// @Param        server  path      string  true  "Configured MCP server slug"
// @Success      200     {object}  map[string]interface{}  "JSON-RPC response or SSE stream"
// @Failure      401     {object}  core.OpenAIErrorEnvelope
// @Failure      404     {object}  core.OpenAIErrorEnvelope
// @Failure      429     {object}  core.OpenAIErrorEnvelope
// @Failure      501     {object}  core.OpenAIErrorEnvelope
// @Router       /mcp/{server} [post]
// @Router       /mcp/{server} [get]
// @Router       /mcp/{server} [delete]
func (h *Handler) MCPServer(c *echo.Context) error {
	return h.mcp().handle(c, c.Param("server"))
}

// ChatCompletion handles POST /v1/chat/completions
//
// @Summary      Create a chat completion
// @Tags         chat
// @Accept       json
// @Produce      json
// @Produce      text/event-stream
// @Security     BearerAuth
// @Param        request  body      core.ChatRequest  true  "Chat completion request"
// @Success      200      {object}  core.ChatResponse  "JSON response or SSE stream when stream=true"
// @Failure      400      {object}  core.OpenAIErrorEnvelope
// @Failure      401      {object}  core.OpenAIErrorEnvelope
// @Failure      429      {object}  core.OpenAIErrorEnvelope
// @Failure      502      {object}  core.OpenAIErrorEnvelope
// @Router       /v1/chat/completions [post]
func (h *Handler) ChatCompletion(c *echo.Context) error {
	return h.translatedInference().ChatCompletion(c)
}

// Health handles GET /health
//
// @Summary      Health check
// @Tags         system
// @Produce      json
// @Success      200  {object}  map[string]string
// @Router       /health [get]
func (h *Handler) Health(c *echo.Context) error {
	return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
}

// ListModels handles GET /v1/models
//
// @Summary      List available models
// @Tags         models
// @Produce      json
// @Security     BearerAuth
// @Success      200  {object}  core.ModelsResponse
// @Failure      401  {object}  core.OpenAIErrorEnvelope
// @Failure      502  {object}  core.OpenAIErrorEnvelope
// @Router       /v1/models [get]
func (h *Handler) ListModels(c *echo.Context) error {
	// Create context with request ID for provider
	requestID := c.Request().Header.Get(core.RequestIDHeader)
	ctx := core.WithRequestID(c.Request().Context(), requestID)

	resp, err := h.provider.ListModels(ctx)
	if err != nil {
		return handleError(c, err)
	}
	if h.keepOnlyAliasesAtModelsEndpoint {
		object := "list"
		if resp != nil && resp.Object != "" {
			object = resp.Object
		}
		resp = &core.ModelsResponse{Object: object, Data: []core.Model{}}
	}
	if h.modelAuthorizer != nil && resp != nil {
		resp = &core.ModelsResponse{
			Object: resp.Object,
			Data:   h.modelAuthorizer.FilterPublicModels(c.Request().Context(), resp.Data),
		}
	}
	if h.exposedModelLister != nil {
		ctx := c.Request().Context()
		// The target-access filter is only available when an authorizer is set.
		var allow func(core.ModelSelector) bool
		if h.modelAuthorizer != nil {
			allow = func(selector core.ModelSelector) bool {
				return h.modelAuthorizer.AllowsModel(ctx, selector)
			}
		}
		// User-path scoping of redirects is a property of the redirect itself, not
		// of the authorizer, so it must apply even when no authorizer is configured
		// (allow is nil there) — otherwise scoped redirect IDs leak to callers
		// outside their user_paths.
		if scoped, ok := h.exposedModelLister.(UserPathExposedModelLister); ok {
			resp = mergeExposedModelsResponse(resp, scoped.ExposedModelsForUserPath(core.UserPathFromContext(ctx), allow))
		} else if filtered, ok := h.exposedModelLister.(FilteredExposedModelLister); ok && allow != nil {
			resp = mergeExposedModelsResponse(resp, filtered.ExposedModelsFiltered(allow))
		} else {
			exposed := h.exposedModelLister.ExposedModels()
			if allow != nil {
				filtered := make([]core.Model, 0, len(exposed))
				for _, model := range exposed {
					selector, err := core.ParseModelSelector(model.ID, "")
					if err != nil || !allow(selector) {
						continue
					}
					filtered = append(filtered, model)
				}
				exposed = filtered
			}
			resp = mergeExposedModelsResponse(resp, exposed)
		}
	}

	// The models route is shared by both wire dialects. Anthropic SDK clients
	// are identified by the anthropic-version header they always send; render
	// the Anthropic list shape for them, the OpenAI shape for everyone else.
	if c.Request().Header.Get("anthropic-version") != "" {
		var models []core.Model
		if resp != nil {
			models = resp.Data
		}
		return c.JSON(http.StatusOK, anthropicapi.FromModels(models))
	}

	return c.JSON(http.StatusOK, resp)
}

func (h *Handler) Embeddings(c *echo.Context) error {
	return h.translatedInference().Embeddings(c)
