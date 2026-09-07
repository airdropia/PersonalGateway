package openai

import "github.com/airdropia/pgw/internal/providers"

var passthroughSemanticEnricher = providers.NewOpenAICompatibleSemanticEnricher("openai")
