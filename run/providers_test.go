package run

import (
	"slices"
	"testing"

	"github.com/enterpilot/gomodel/config"
	"github.com/enterpilot/gomodel/internal/providers"
)

// TestDefaultProviderFactoryCredentialForms pins the credential form the
// shipped provider registrations produce — the contract the admin API serves
// and the dashboard renders. The Stage-9 personal edition keeps only the
// nine providers the plan §8 personal-use profile commits to; the
// enterprise-only providers that still exist in the source tree are no
// longer linked, so they no longer need schema coverage here.
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
		options      map[string][]string
	}{
		{
			// The plain shape every API-key provider derives.
			providerType: "openai",
			defaultURL:   "https://api.openai.com/v1",
			fields:       []string{"api_keys", "base_url", "session_sticky_keys", "models"},
			required:     []string{"api_keys"},
			absent:       []string{"api_version", "vertex_project"},
		},
		{
			// Azure is the one type that takes an API version.
			providerType: "azure",
			fields:       []string{"api_keys", "base_url", "api_version", "session_sticky_keys", "models"},
			required:     []string{"api_keys", "base_url"},
		},
		{
			// Authenticates through the AWS SDK credential chain, never a key.
			providerType: "bedrock",
			fields:       []string{"base_url", "models"},
			absent:       []string{"api_keys"},
		},
		{
			// One adapter, two backends: an AI Studio key, or Google
			providerType: "gemini",
			defaultURL:   "https://generativelanguage.googleapis.com/v1beta/openai",
			fields: []string{
				"api_keys", "backend", "base_url", "api_mode", "auth_type",
				"vertex_project", "vertex_location", "service_account_json",
				"service_account_file", "service_account_json_base64", "gcp_scope", "session_sticky_keys", "models",
			},
			required: nil,
			options: map[string][]string{
				"backend":   {"aistudio", "vertex"},
				"api_mode":  {"native", "openai_compatible"},
				"auth_type": {"api_key", "gcp_adc", "gcp_service_account"},
			},
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
// the nine providers the plan §8 profile commits to; the upstream list
// is no longer authoritative.
func TestDefaultProviderFactoryRegistersAllProviderTypes(t *testing.T) {
	expected := []string{
		"anthropic", "azure", "bedrock", "chatgpt", "gemini", "groq", "openai", "opencode_go", "xai",
	}

	for _, metricsEnabled := range []bool{false, true} {
		cfg := &config.Config{}
		cfg.Metrics.Enabled = metricsEnabled

		factory := defaultProviderFactory(cfg)
		got := factory.RegisteredTypes()
		slices.Sort(got)

		if !slices.Equal(got, expected) {
			t.Errorf("metrics=%v: registered types = %v, want %v", metricsEnabled, got, expected)
		}

		// CredentialSchemas is the source for
		// GET /admin/provider-credentials/types, which drives the dashboard's
		// Add Provider selector. Keep it in exact lockstep with construction.
		dashboardTypes := make([]string, 0, len(expected))
		for _, schema := range factory.CredentialSchemas() {
			dashboardTypes = append(dashboardTypes, schema.Type)
		}
		if !slices.Equal(dashboardTypes, expected) {
			t.Errorf("metrics=%v: dashboard provider types = %v, want %v", metricsEnabled, dashboardTypes, expected)
		}
	}
}

// TestPersonalEdition_DropsKimicode and TestPersonalEdition_DropsHetzner
// are explicit canaries against the upstream registration list: they fail if
// those providers are silently re-introduced through a careless merge. The
// Stage 9 personal edition intentionally drops them, so the canaries
// assert the providers are NOT registered.
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