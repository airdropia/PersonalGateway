package cohere

import "github.com/airdropia/pgw/internal/providers"

var passthroughSemanticEnricher = providers.NewSemanticEnricher("cohere", map[string]providers.PassthroughEndpointSemantics{
	"/v2/chat":  {Operation: "cohere.chat", GenAIOperation: "chat", AuditPath: "/p/cohere/v2/chat"},
	"/v2/embed": {Operation: "cohere.embed", GenAIOperation: "embeddings", AuditPath: "/p/cohere/v2/embed"},
})
