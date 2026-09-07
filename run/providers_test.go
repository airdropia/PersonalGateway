package run

import (
	"slices"
	"testing"

	"github.com/airdropia/pgw/config"
	"github.com/airdropia/pgw/internal/provider"
	"github.com/airdropia/pgw/internal/providers"
)

// TestDefaultProviderFactoryCredentialForms pins the credential form the
// shipped provider registrations produce — the contract the admin API serves
// and the dashboard renders. The personal edition keeps one provider type,
// the generic OpenAI-compatible adapter (plan §6); the schema therefore
// derives from the plain "API key against one endpoint" shape.
func TestDefaultProviderFactoryCredentialForms(t *testing.T) {
	schemas := map[string]providers.CredentialSchema{}
	for _, schema := range defaultProviderFactory(&config.Config{}).CredentialSchemas() {
		schemas[schema.Type] = schema
	}

	tests := []struct {
		providerType string
		defaultURL   string
		fields       []string // exact, in display order
		required     []string
		absent       []string
	}{
		{
			// The plain shape every API-key provider derives.
			providerType: provider.Type,
			defaultURL:   "https://api.openai.com/v1",
			fields:       []string{"api_keys", "base_url", "session_sticky_keys", "models"},
			required:     []string{"api_keys"},
			absent:       []string{"api_version", "vertex_project"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.providerType, func(t *testing.T) {
			schema, ok := schemas[tt.providerType]
			if !ok {
				t.Fatalf("schema for %q not registered", tt.providerType)
			}
			if schema.DefaultBaseURL != tt.defaultURL {
				t.Errorf("DefaultBaseURL = %q, want %q", schema.DefaultBaseURL, tt.defaultURL)
			}
			got := make([]string, len(schema.Fields))
			for i, f := range schema.Fields {
				got[i] = f.Name
			}
			if !slices.Equal(got, tt.fields) {
				t.Errorf("Fields = %v, want %v", got, tt.fields)
			}
			for _, want := range tt.absent {
				if schema.Accepts(want) {
					t.Errorf("schema should not accept field %q, but does", want)
				}
			}
		})
	}
}

// TestDefaultProviderFactoryRegistersAllProviderTypes is the personal-edition
// counterpart to the upstream test. Stage 9 trims the registered set to
// the single generic adapter; the upstream list is no longer
// authoritative.
func TestDefaultProviderFactoryRegistersAllProviderTypes(t *testing.T) {
	expected := []string{provider.Type}

	factory := defaultProviderFactory(&config.Config{})
	got := factory.RegisteredTypes()
	slices.Sort(got)

	if !slices.Equal(got, expected) {
		t.Errorf("registered types = %v, want %v", got, expected)
	}

	// CredentialSchemas is the source for
	// GET /admin/provider-credentials/types, which drives the dashboard's
	// Add Provider selector. Keep it in exact lockstep with construction.
	dashboardTypes := make([]string, 0, len(expected))
	for _, schema := range factory.CredentialSchemas() {
		dashboardTypes = append(dashboardTypes, schema.Type)
	}
	if !slices.Equal(dashboardTypes, expected) {
		t.Errorf("dashboard provider types = %v, want %v", dashboardTypes, expected)
	}
}

// TestPersonalEdition_DropsKimicode and TestPersonalEdition_DropsHetzner
// are explicit canaries against the upstream registration list: they fail if
// those providers are silently re-introduced through a careless merge. The
// personal edition intentionally drops them, so the canaries assert the
// providers are NOT registered.
func TestPersonalEdition_DropsKimicode(t *testing.T) {
	registered := defaultProviderFactory(&config.Config{}).RegisteredTypes()
	if slices.Contains(registered, "kimicode") {
		t.Fatalf("kimicode must not be registered in the personal edition (plan §8); got %v", registered)
	}
}

func TestPersonalEdition_DropsHetzner(t *testing.T) {
	registered := defaultProviderFactory(&config.Config{}).RegisteredTypes()
	if slices.Contains(registered, "hetzner") {
		t.Fatalf("hetzner must not be registered in the personal edition (plan §8); got %v", registered)
	}
}
