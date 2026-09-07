package run

import (
	"github.com/airdropia/pgw/config"
	"github.com/airdropia/pgw/internal/provider"
	"github.com/airdropia/pgw/internal/providers"
)

// defaultProviderFactory builds the provider factory with the single
// generic OpenAI-compatible adapter (plan §6). There are no
// provider-specific packages; every configured upstream is one instance
// of the same adapter, identified by a user-chosen display_name. The
// vendor packages that upstream GoModel shipped are deleted from the
// link path in Stage 1; the factory can therefore never accidentally
// re-register them.
func defaultProviderFactory(_ *config.Config) *providers.ProviderFactory {
	factory := providers.NewProviderFactory()
	factory.Add(provider.Registration)
	return factory
}
