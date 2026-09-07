package zai

import "github.com/airdropia/pgw/internal/providers"

var passthroughSemanticEnricher = providers.NewOpenAICompatibleSemanticEnricher("zai")
