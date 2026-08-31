package run

import (
	"github.com/enterpilot/gomodel/config"
	"github.com/enterpilot/gomodel/internal/observability"
	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/providers/anthropic"
	"github.com/enterpilot/gomodel/internal/providers/azure"
	"github.com/enterpilot/gomodel/internal/providers/bedrock"
	"github.com/enterpilot/gomodel/internal/providers/chatgpt"
	"github.com/enterpilot/gomodel/internal/providers/gemini"
	"github.com/enterpilot/gomodel/internal/providers/groq"
	"github.com/enterpilot/gomodel/internal/providers/openai"
	"github.com/enterpilot/gomodel/internal/providers/opencodego"
	"github.com/enterpilot/gomodel/internal/providers/xai"
)

// defaultProviderFactory builds the provider factory with the providers the
// personal edition keeps (plan §8, Stage 5 doc). Stage 9 drops the
// enterprise-only providers from the binary. The unkept providers remain
// in the source tree for upstream sync, but are no longer linked: Go's
// linker garbage-collects the unreferenced package code.
func defaultProviderFactory(cfg *config.Config) *providers.ProviderFactory {
	factory := providers.NewProviderFactory()

	if cfg.Metrics.Enabled {
		factory.SetHooks(observability.NewPrometheusHooks())
	}

	factory.Add(openai.Registration)
	factory.Add(chatgpt.Registration)
	factory.Add(anthropic.Registration)
	factory.Add(gemini.Registration)
	factory.Add(groq.Registration)
	factory.Add(xai.Registration)
	factory.Add(opencodego.Registration)
	factory.Add(azure.Registration)
	factory.Add(bedrock.Registration)

	return factory
}