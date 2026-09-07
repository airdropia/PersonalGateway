package provider

import (
	"testing"

	"github.com/airdropia/pgw/internal/core"
)

func TestExtractMetadata_AllKnownFields(t *testing.T) {
	raw := map[string]any{
		"max_context_length": float64(128000),
		"max_tokens":         float64(4096),
		"capabilities": map[string]any{
			"tools":  true,
			"vision": true,
		},
		"pricing": map[string]any{
			"prompt":     float64(0.15),
			"completion": float64(0.60),
		},
	}
	got := ExtractMetadata(raw)
	if got.ContextWindow != 128000 {
		t.Errorf("ContextWindow = %d, want 128000", got.ContextWindow)
	}
	if got.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens = %d, want 4096", got.MaxOutputTokens)
	}
	if !got.SupportsTools {
		t.Error("SupportsTools = false, want true")
	}
	if !got.SupportsVision {
		t.Error("SupportsVision = false, want true")
	}
	if got.InputPricePer1M != 0.15 {
		t.Errorf("InputPricePer1M = %v, want 0.15", got.InputPricePer1M)
	}
	if got.OutputPricePer1M != 0.60 {
		t.Errorf("OutputPricePer1M = %v, want 0.60", got.OutputPricePer1M)
	}
}

func TestExtractMetadata_FallbackKeys(t *testing.T) {
	// OpenRouter / Groq use different field names; verify the table covers them.
	raw := map[string]any{
		"context_window":  float64(32000),
		"max_output_tokens": float64(8192),
		"supports_tools":    true,
		"price_input":       float64(0.10),
		"price_output":      float64(0.30),
	}
	got := ExtractMetadata(raw)
	if got.ContextWindow != 32000 {
		t.Errorf("ContextWindow = %d, want 32000", got.ContextWindow)
	}
	if got.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d, want 8192", got.MaxOutputTokens)
	}
	if !got.SupportsTools {
		t.Error("SupportsTools = false, want true")
	}
	if got.InputPricePer1M != 0.10 {
		t.Errorf("InputPricePer1M = %v, want 0.10", got.InputPricePer1M)
	}
}

func TestExtractMetadata_StringEncodedValues(t *testing.T) {
	raw := map[string]any{
		"max_context_length": "131072",
		"max_output_tokens":  "8192",
		"supports_vision":    "yes",
	}
	got := ExtractMetadata(raw)
	if got.ContextWindow != 131072 {
		t.Errorf("ContextWindow = %d, want 131072", got.ContextWindow)
	}
	if got.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d, want 8192", got.MaxOutputTokens)
	}
	if !got.SupportsVision {
		t.Error("SupportsVision = false, want true")
	}
}

func TestExtractMetadata_EmptyAndMissing(t *testing.T) {
	if got := ExtractMetadata(nil); got != (ExtractedMetadata{}) {
		t.Errorf("nil raw = %+v, want zero", got)
	}
	if got := ExtractMetadata(map[string]any{}); got != (ExtractedMetadata{}) {
		t.Errorf("empty raw = %+v, want zero", got)
	}
	// Unknown field names must yield zero values, not panic.
	raw := map[string]any{
		"foo_bar": float64(99),
	}
	if got := ExtractMetadata(raw); got != (ExtractedMetadata{}) {
		t.Errorf("unknown fields = %+v, want zero", got)
	}
}

func TestApplyToCoreMetadata_OnlySetsKnownValues(t *testing.T) {
	e := ExtractedMetadata{ContextWindow: 64000, SupportsTools: true}
	m := &core.Model{ID: "x"}
	e.ApplyToCoreMetadata(m)
	if m.Metadata == nil {
		t.Fatal("Metadata not populated despite a known value")
	}
	if m.Metadata.ContextWindow == nil || *m.Metadata.ContextWindow != 64000 {
		t.Errorf("ContextWindow = %v, want 64000", m.Metadata.ContextWindow)
	}
	if m.Metadata.Capabilities["tools"] != true {
		t.Errorf("Capabilities[tools] = %v, want true", m.Metadata.Capabilities["tools"])
	}
	if m.Metadata.MaxOutputTokens != nil {
		t.Errorf("MaxOutputTokens set to %v, want nil", m.Metadata.MaxOutputTokens)
	}
}

func TestDig_MissingPathIsSafe(t *testing.T) {
	raw := map[string]any{
		"a": map[string]any{"b": float64(1)},
	}
	if v := dig(raw, "a.x.y"); v != nil {
		t.Errorf("dig missing path = %v, want nil", v)
	}
	if v := dig(raw, "a.b"); v != float64(1) {
		t.Errorf("dig a.b = %v, want 1", v)
	}
}