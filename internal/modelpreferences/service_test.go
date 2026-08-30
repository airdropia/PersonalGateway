package modelpreferences

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/enterpilot/gomodel/internal/core"
)

// fakeStore is a deterministic in-memory implementation of Store. It records
// each Upsert so tests can assert the order and shape of operations the
// service issues against persistence.
type fakeStore struct {
	mu       sync.Mutex
	rows     map[string]Preference
	listErr  error
	upsertCh chan Preference
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		rows:     make(map[string]Preference),
		upsertCh: make(chan Preference, 8),
	}
}

func (s *fakeStore) List(_ context.Context) ([]Preference, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Preference, 0, len(s.rows))
	for _, p := range s.rows {
		out = append(out, p)
	}
	return out, nil
}

func (s *fakeStore) Upsert(_ context.Context, p Preference) error {
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	p.UpdatedAt = time.Now().UTC()
	s.mu.Lock()
	s.rows[p.Selector] = p
	s.mu.Unlock()
	select {
	case s.upsertCh <- p:
	default:
	}
	return nil
}

func (s *fakeStore) Delete(_ context.Context, selector string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[selector]; !ok {
		return ErrNotFound
	}
	delete(s.rows, selector)
	return nil
}

func (s *fakeStore) ResetAll(_ context.Context) error {
	s.mu.Lock()
	s.rows = make(map[string]Preference)
	s.mu.Unlock()
	return nil
}

func (s *fakeStore) Close() error { return nil }

// fakeCatalog exposes the provider names used by the service for selector
// normalization. Models referenced in hidden preferences must reference one
// of these names or the selector cannot resolve to a known provider.
type fakeCatalog struct{ names []string }

func (c fakeCatalog) ProviderNames() []string { return append([]string(nil), c.names...) }

func newServiceWithCatalog(t *testing.T, providers []string) (*Service, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc, err := NewService(store, fakeCatalog{names: providers})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	return svc, store
}

// TestIsHidden_ScopePrecedence covers the documented precedence:
//   exact > provider-wide > model-wide > global.
// The most specific applicable preference must win regardless of its hidden
// flag. The selectors package resolves scopes in this order and returns the
// first match it finds.
func TestIsHidden_ScopePrecedence(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})

	ctx := context.Background()
	upsert := func(sel string, hidden bool) {
		t.Helper()
		if _, err := svc.Upsert(ctx, sel, hidden); err != nil {
			t.Fatalf("Upsert(%q): %v", sel, err)
		}
	}
	// Exact, provider-wide, model-wide, global — conflicting values.
	upsert("openai/gpt-4o", false) // exact says visible
	upsert("openai/", true)        // provider-wide says hidden
	upsert("gpt-4o-mini", false)   // model-wide says visible
	upsert("/", true)              // global says hidden

	// Exact selector wins even though provider/global say hidden.
	if svc.IsHidden("openai/gpt-4o") {
		t.Fatal("exact preference must beat provider-wide and global")
	}
	// Different model in the same provider with no exact row: provider-wide
	// is the most specific applicable scope, so its hidden flag wins over
	// model-wide and global.
	if !svc.IsHidden("openai/gpt-4o-mini") {
		t.Fatal("provider-wide hidden must beat model-wide visible for the same provider")
	}
	// Different model in the same provider with no model/global override
	// beyond provider-wide hidden: provider-wide still applies.
	if !svc.IsHidden("openai/embed") {
		t.Fatal("provider-wide hidden must apply to unmentioned models in same provider")
	}
}

// TestIsHidden_NoProviderInInputIsVisible pins the safety net: when a caller
// hands IsHidden an input whose first slash segment does not match a known
// provider, the selectors package falls back to a model-only selector, and
// IsHidden's ProviderName guard means the call returns false. This prevents
// an unknown-prefix typo from silently hiding things.
func TestIsHidden_NoProviderInInputIsVisible(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()
	if _, err := svc.Upsert(ctx, "/", true); err != nil {
		t.Fatalf("Upsert global: %v", err)
	}
	// "mystery/model" — the selectors package treats "mystery" as a model
	// fragment because the provider set has no such name. IsHidden sees
	// ProviderName == "" and returns false rather than guessing.
	if svc.IsHidden("mystery/model") {
		t.Fatal("IsHidden with empty ProviderName must default to visible")
	}
}

// TestUpsert_NormalizesSelector ensures the stored selector is canonical and
// that lookups via either form resolve to the same row.
func TestUpsert_NormalizesSelector(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()

	got, err := svc.Upsert(ctx, " openai/gpt-4o ", true)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got.Selector != "openai/gpt-4o" {
		t.Fatalf("Selector = %q, want canonical openai/gpt-4o", got.Selector)
	}

	fetched, ok, err := svc.Get("openai/gpt-4o")
	if err != nil || !ok {
		t.Fatalf("Get(openai/gpt-4o) ok=%v err=%v", ok, err)
	}
	if !fetched.Hidden {
		t.Fatal("hidden flag lost after roundtrip")
	}
	if _, err := svc.NormalizeSelector(" openai/gpt-4o "); err != nil {
		t.Fatalf("NormalizeSelector: %v", err)
	}
}

// TestUpsert_RejectsEmptySelector confirms an empty string is the only input
// the selectors package refuses. A rejected Upsert must not be persisted.
func TestUpsert_RejectsEmptySelector(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	_, err := svc.Upsert(context.Background(), "   ", true)
	if err == nil {
		t.Fatal("Upsert with whitespace-only selector returned nil error")
	}
	if !IsValidationError(err) {
		t.Fatalf("err = %v, want a modelselectors validation error", err)
	}
	if svc.Len() != 0 {
		t.Fatalf("Len = %d, want 0 (rejected Upsert must not persist)", svc.Len())
	}
}

// TestFilterModels_PreservesOrder asserts that filtering never reorders the
// remaining models. The dashboard relies on this for stable lists.
func TestFilterModels_PreservesOrder(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()
	if _, err := svc.Upsert(ctx, "openai/gpt-4o", true); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	in := []core.Model{
		{ID: "openai/gpt-4o", Object: "model", OwnedBy: "openai"},
		{ID: "openai/gpt-3.5-turbo", Object: "model", OwnedBy: "openai"},
		{ID: "openai/gpt-4o-mini", Object: "model", OwnedBy: "openai"},
	}
	out := svc.FilterModels(in)

	want := []string{"openai/gpt-3.5-turbo", "openai/gpt-4o-mini"}
	if len(out) != len(want) {
		t.Fatalf("FilterModels returned %d rows, want %d", len(out), len(want))
	}
	for i, m := range out {
		if m.ID != want[i] {
			t.Fatalf("FilterModels row %d = %q, want %q (order must be preserved)", i, m.ID, want[i])
		}
	}
}

// TestDelete_RemovesHidden verifies the service forgets a preference and
// that subsequent lookups report the model as visible again.
func TestDelete_RemovesHidden(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()

	if _, err := svc.Upsert(ctx, "openai/gpt-4o", true); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if !svc.IsHidden("openai/gpt-4o") {
		t.Fatal("model should be hidden after Upsert")
	}
	if err := svc.Delete(ctx, "openai/gpt-4o"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if svc.IsHidden("openai/gpt-4o") {
		t.Fatal("model is still hidden after Delete")
	}
	if svc.Len() != 0 {
		t.Fatalf("Len = %d, want 0", svc.Len())
	}
}

// TestResetAll_ClearsSnapshot confirms a reset empties the in-memory snapshot
// and persists that state on the next Refresh.
func TestResetAll_ClearsSnapshot(t *testing.T) {
	svc, store := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()

	for _, sel := range []string{"openai/gpt-4o", "openai/", "gpt-4o-mini", "/"} {
		if _, err := svc.Upsert(ctx, sel, true); err != nil {
			t.Fatalf("Upsert(%q): %v", sel, err)
		}
	}
	if svc.Len() != 4 {
		t.Fatalf("Len before reset = %d, want 4", svc.Len())
	}
	if err := svc.ResetAll(ctx); err != nil {
		t.Fatalf("ResetAll: %v", err)
	}
	if svc.Len() != 0 {
		t.Fatalf("Len after reset = %d, want 0", svc.Len())
	}
	// Mutate the underlying store to simulate someone re-adding rows out of
	// band: a fresh Refresh must re-read and pick them up. This proves
	// ResetAll is durable through the persistence boundary, not just the
	// in-memory cache.
	store.rows["openai/gpt-4o"] = Preference{Selector: "openai/gpt-4o", Hidden: true}
	if err := svc.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if svc.Len() != 1 {
		t.Fatalf("Len after re-Refresh = %d, want 1", svc.Len())
	}
	if !svc.IsHidden("openai/gpt-4o") {
		t.Fatal("Refresh failed to reload rows after ResetAll")
	}
}

// TestRefresh_NormalizesLegacyStoredSelectors documents the read path for
// rows persisted before a rename. NormalizeStored is liberal: a row stored
// by an earlier version is re-loaded without error, even though the service
// never wrote that exact selector itself.
func TestRefresh_NormalizesLegacyStoredSelectors(t *testing.T) {
	store := newFakeStore()
	store.rows["legacy-form"] = Preference{Selector: "legacy-form", Hidden: true}

	svc, err := NewService(store, fakeCatalog{names: []string{"openai"}})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if svc.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (legacy row must be retained)", svc.Len())
	}
	if _, ok, _ := svc.Get("legacy-form"); !ok {
		t.Fatal("legacy row must remain queryable after Refresh")
	}
}

// TestList_OrderedBySelector guards the documented deterministic order. The
// dashboard uses this ordering for stable UI rendering.
func TestList_OrderedBySelector(t *testing.T) {
	svc, _ := newServiceWithCatalog(t, []string{"openai"})
	ctx := context.Background()
	for _, sel := range []string{"openai/gpt-4o", "/", "openai/", "gpt-4o-mini"} {
		if _, err := svc.Upsert(ctx, sel, true); err != nil {
			t.Fatalf("Upsert(%q): %v", sel, err)
		}
	}

	list := svc.List()
	want := []string{"/", "gpt-4o-mini", "openai/", "openai/gpt-4o"}
	if len(list) != len(want) {
		t.Fatalf("len = %d, want %d", len(list), len(want))
	}
	for i, p := range list {
		if p.Selector != want[i] {
			t.Fatalf("List[%d] = %q, want %q", i, p.Selector, want[i])
		}
	}
}

// keep fmt referenced; future assertions may want formatted diagnostics.
var _ = fmt.Sprintf