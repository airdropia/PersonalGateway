package kilo

import "github.com/airdropia/pgw/internal/providers"

var passthroughSemanticEnricher = providers.NewSemanticEnricher("kilo", map[string]providers.PassthroughEndpointSemantics{
	"/chat/completions": {Operation: "kilo.chat_completions", GenAIOperation: "chat", AuditPath: "/v1/chat/completions"},
})
