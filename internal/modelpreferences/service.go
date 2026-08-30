package modelpreferences

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/modelselectors"
)

// Service keeps preferences cached in memory and resolves scoped visibility.
type Service struct {
	store   Store
	catalog modelselectors.Catalog

	mu       sync.RWMutex
	snapshot map[string]Preference
}

// NewService creates a model preference service backed by store.
func NewService(store Store, catalog modelselectors.Catalog) (*Service, error) {
	if store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("catalog is required")
	}
	return &Service{store: store, catalog: catalog, snapshot: make(map[string]Preference)}, nil
}

// Refresh reloads preferences. Unknown historical selectors are retained.
func (s *Service) Refresh(ctx context.Context) error {
	preferences, err := s.store.List(ctx)
	if err != nil {
		return fmt.Errorf("refresh model preferences: %w", err)
	}
	next := make(map[string]Preference, len(preferences))
	for _, preference := range preferences {
		normalized, normalizeErr := modelselectors.NormalizeStored(preference.Selector, "", "")
		if normalizeErr != nil {
			continue
		}
		preference.Selector = normalized.Selector
		next[preference.Selector] = preference
	}
	s.mu.Lock()
	s.snapshot = next
	s.mu.Unlock()
	return nil
}

// NormalizeSelector validates and canonicalizes a user-supplied selector.
func (s *Service) NormalizeSelector(raw string) (string, error) {
	parts, err := modelselectors.NormalizeInput(s.catalog, raw)
	if err != nil {
		return "", err
	}
	return parts.Selector, nil
}

// List returns all preferences in deterministic selector order.
func (s *Service) List() []Preference {
	s.mu.RLock()
	result := make([]Preference, 0, len(s.snapshot))
	for _, preference := range s.snapshot {
		result = append(result, preference)
	}
	s.mu.RUnlock()
	sort.Slice(result, func(i, j int) bool { return result[i].Selector < result[j].Selector })
	return result
}

// Get returns an exact preference after selector normalization.
func (s *Service) Get(raw string) (Preference, bool, error) {
	selector, err := s.NormalizeSelector(raw)
	if err != nil {
		return Preference{}, false, err
	}
	s.mu.RLock()
	preference, ok := s.snapshot[selector]
	s.mu.RUnlock()
	return preference, ok, nil
}

// IsHidden resolves the most-specific hidden preference for a concrete model.
func (s *Service) IsHidden(raw string) bool {
	parts, err := modelselectors.NormalizeInput(s.catalog, raw)
	if err != nil || parts.ProviderName == "" || parts.Model == "" {
		return false
	}
	keys := []string{
		parts.Selector,
		modelselectors.String(parts.ProviderName, ""),
		parts.Model,
		"/",
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range keys {
		if preference, ok := s.snapshot[key]; ok {
			return preference.Hidden
		}
	}
	return false
}

// FilterModels removes hidden models from a projection while preserving order.
func (s *Service) FilterModels(models []core.Model) []core.Model {
	if len(models) == 0 {
		return models
	}
	filtered := make([]core.Model, 0, len(models))
	for _, model := range models {
		if !s.IsHidden(model.ID) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

// Upsert normalizes, persists, refreshes, and returns the resulting preference.
func (s *Service) Upsert(ctx context.Context, raw string, hidden bool) (Preference, error) {
	selector, err := s.NormalizeSelector(raw)
	if err != nil {
		return Preference{}, err
	}
	if err := s.store.Upsert(ctx, Preference{Selector: selector, Hidden: hidden}); err != nil {
		return Preference{}, err
	}
	if err := s.Refresh(ctx); err != nil {
		return Preference{}, err
	}
	preference, ok, err := s.Get(selector)
	if err != nil {
		return Preference{}, err
	}
	if !ok {
		return Preference{}, fmt.Errorf("model preference missing after upsert")
	}
	return preference, nil
}

// Delete forgets a preference; it does not affect provider discovery or routing.
func (s *Service) Delete(ctx context.Context, raw string) error {
	selector, err := s.NormalizeSelector(raw)
	if err != nil {
		return err
	}
	if err := s.store.Delete(ctx, selector); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// ResetAll forgets every local visibility preference.
func (s *Service) ResetAll(ctx context.Context) error {
	if err := s.store.ResetAll(ctx); err != nil {
		return err
	}
	return s.Refresh(ctx)
}

// Len returns the number of stored preferences.
func (s *Service) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.snapshot)
}
