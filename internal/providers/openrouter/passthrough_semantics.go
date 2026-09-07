package openrouter

import "github.com/airdropia/pgw/internal/providers"

var passthroughSemanticEnricher = providers.NewOpenAICompatibleSemanticEnricher("openrouter")
