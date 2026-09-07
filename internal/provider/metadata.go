package provider

import (
	"strconv"

	"github.com/airdropia/pgw/internal/core"
)

// ExtractedMetadata is the result of running heuristic extraction over
// a raw upstream model object. Zero values mean "unknown".
type ExtractedMetadata struct {
	ContextWindow    int64
	MaxOutputTokens  int64
	SupportsTools    bool
	SupportsVision   bool
	SupportsStreaming bool
	InputPricePer1M  float64
	OutputPricePer1M float64
}

// ExtractMetadata walks the raw upstream JSON for a single model and
// populates ExtractedMetadata with whichever fields the provider
// happens to publish. The heuristic key candidates are listed in plan
// §2.3; manual overrides from the dashboard win over the values
// returned here. Plan §6.3 leaves heuristic extraction at this layer
// so a future storage projection can apply user overrides consistently.
//
// The function is best-effort: a missing field is a no-op, an
// unexpected type is silently skipped. Errors never propagate because
// the metadata is informational and a bad value must never block a
// model from being tickable.
func ExtractMetadata(raw map[string]any) ExtractedMetadata {
	var out ExtractedMetadata
	if raw == nil {
		return out
	}
	out.ContextWindow = firstInt(raw,
		"max_context_length",
		"context_length",
		"context_window",
		"max_input_tokens",
	)
	out.MaxOutputTokens = firstInt(raw,
		"max_tokens",
		"max_output_tokens",
		"max_completion_tokens",
	)
	out.SupportsTools = firstBool(raw,
		"capabilities.tools",
		"supports.tools",
		"supports_tools",
		"tools",
		"function_calling",
	)
	out.SupportsVision = firstBool(raw,
		"capabilities.vision",
		"supports.vision",
		"supports_vision",
		"vision",
		"multimodal",
	)
	out.SupportsStreaming = firstBool(raw,
		"capabilities.streaming",
		"supports.streaming",
		"supports_streaming",
		"stream",
		"streaming",
	)
	out.InputPricePer1M = firstFloat(raw,
		"pricing.prompt",
		"price_input",
		"input_cost",
		"input_price",
	)
	out.OutputPricePer1M = firstFloat(raw,
		"pricing.completion",
		"price_output",
		"output_cost",
		"output_price",
	)
	return out
}

// firstInt returns the first key whose value parses as a positive int,
// or zero when none of the candidates yield a value.
func firstInt(raw map[string]any, keys ...string) int64 {
	for _, k := range keys {
		v := dig(raw, k)
		if v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n > 0 {
				return int64(n)
			}
		case int:
			if n > 0 {
				return int64(n)
			}
		case int64:
			if n > 0 {
				return n
			}
		case string:
			if n == "" {
				continue
			}
			parsed, err := strconv.ParseInt(n, 10, 64)
			if err == nil && parsed > 0 {
				return parsed
			}
		}
	}
	return 0
}

// firstFloat returns the first key whose value parses as a non-negative
// float, or zero when none of the candidates yield a value.
func firstFloat(raw map[string]any, keys ...string) float64 {
	for _, k := range keys {
		v := dig(raw, k)
		if v == nil {
			continue
		}
		switch n := v.(type) {
		case float64:
			if n >= 0 {
				return n
			}
		case string:
			if n == "" {
				continue
			}
			parsed, err := strconv.ParseFloat(n, 64)
			if err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return 0
}

// firstBool returns the first key whose value is a recognised boolean
// representation. JSON booleans, the strings "true"/"yes"/"1" and the
// number 1 all count as true. Anything else is false.
func firstBool(raw map[string]any, keys ...string) bool {
	for _, k := range keys {
		v := dig(raw, k)
		if v == nil {
			continue
		}
		switch n := v.(type) {
		case bool:
			return n
		case string:
			switch n {
			case "true", "yes", "1":
				return true
			}
		case float64:
			return n != 0
		case int:
			return n != 0
		}
	}
	return false
}

// dig walks a dotted path through nested maps. Returns nil when any
// segment is missing or not a map. This is intentionally simpler than a
// full JSONPath: heuristic extraction only needs one level of nesting.
func dig(raw map[string]any, path string) any {
	parts := splitDots(path)
	var cur any = raw
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur, ok = m[p]
		if !ok {
			return nil
		}
	}
	return cur
}

// splitDots splits a dotted path into its segments. Empty segments
// from leading or trailing dots are dropped so "a..b" becomes
// ["a", "b"].
func splitDots(s string) []string {
	out := []string{}
	last := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			if i > last {
				out = append(out, s[last:i])
			}
			last = i + 1
		}
	}
	if last < len(s) {
		out = append(out, s[last:])
	}
	return out
}

// ApplyToCoreMetadata copies the extracted values into a core.Model
// struct, only writing fields whose heuristic actually fired. This is
// how the Models page learns about context_window, max_output_tokens,
// and the capability flags. Plan §6.3 keeps the storage layer in
// charge of user overrides so this function is purely a default fill.
func (e ExtractedMetadata) ApplyToCoreMetadata(m *core.Model) {
	if m.Metadata == nil && (e.ContextWindow > 0 || e.MaxOutputTokens > 0 || e.SupportsTools || e.SupportsVision) {
		m.Metadata = &core.ModelMetadata{}
	}
	if m.Metadata == nil {
		return
	}
	if e.ContextWindow > 0 {
		cw := int(e.ContextWindow)
		m.Metadata.ContextWindow = &cw
	}
	if e.MaxOutputTokens > 0 {
		mt := int(e.MaxOutputTokens)
		m.Metadata.MaxOutputTokens = &mt
	}
	if e.SupportsTools || e.SupportsVision {
		if m.Metadata.Capabilities == nil {
			m.Metadata.Capabilities = map[string]bool{}
		}
		if e.SupportsTools {
			m.Metadata.Capabilities["tools"] = true
		}
		if e.SupportsVision {
			m.Metadata.Capabilities["vision"] = true
		}
	}
}