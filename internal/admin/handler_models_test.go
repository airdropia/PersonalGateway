package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/enterpilot/gomodel/internal/core"
	"github.com/enterpilot/gomodel/internal/modelpreferences"
	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/virtualmodels"
)

func newVMModelRegistry(t *testing.T) *providers.ModelRegistry {
	t.Helper()
	registry := providers.NewModelRegistry()
	mock := &handlerMockProvider{
		models: &core.ModelsResponse{
			Object: "list",
			Data: []core.Model{
				{ID: "gpt-4o", Object: "model", OwnedBy: "openai"},
			},
		},
	}
	registry.RegisterProviderWithNameAndType(mock, "openai", "openai")
	if err := registry.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	return registry
}

func newVMServiceForRegistry(t *testing.T, registry *providers.ModelRegistry, defaultEnabled bool, items ...virtualmodels.VirtualModel) *virtualmodels.Service {
	t.Helper()
	store := newVMTestStore(items...)
	service, err := virtualmodels.NewService(store, registry, defaultEnabled)
	if err != nil {
		t.Fatalf("NewService() error = %v", err)
	}
	if err := service.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	return service
}

func TestListModels_IncludesModelAccessState(t *testing.T) {
	registry := newVMModelRegistry(t)
	service := newVMServiceForRegistry(t, registry, false, virtualmodels.VirtualModel{
		Source:    "openai/gpt-4o",
		UserPaths: []string{"/team/alpha"},
		Enabled:   true,
	})

	h := NewHandler(nil, registry, WithVirtualModels(service))
	c, rec := newHandlerContext("/admin/models")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len(body) = %d, want 1", len(body))
	}

	row := body[0]
	if row.Access.Selector != "openai/gpt-4o" {
		t.Fatalf("row.Access.Selector = %q, want openai/gpt-4o", row.Access.Selector)
	}
	if row.Access.DefaultEnabled {
		t.Fatal("row.Access.DefaultEnabled = true, want false")
	}
	if !row.Access.EffectiveEnabled {
		t.Fatal("row.Access.EffectiveEnabled = false, want true")
	}
	if len(row.Access.UserPaths) != 1 || row.Access.UserPaths[0] != "/team/alpha" {
		t.Fatalf("row.Access.UserPaths = %#v, want [/team/alpha]", row.Access.UserPaths)
	}
	if row.Access.Override == nil || row.Access.Override.Source != "openai/gpt-4o" {
		t.Fatalf("row.Access.Override = %#v, want exact override", row.Access.Override)
	}
}

func TestListModels_DisabledPolicyTurnsModelOff(t *testing.T) {
	registry := newVMModelRegistry(t)
	service := newVMServiceForRegistry(t, registry, true, virtualmodels.VirtualModel{
		Source:  "openai/gpt-4o",
		Enabled: false,
	})

	h := NewHandler(nil, registry, WithVirtualModels(service))
	c, rec := newHandlerContext("/admin/models")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}

	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len(body) = %d, want 1", len(body))
	}
	row := body[0]
	if !row.Access.DefaultEnabled {
		t.Fatal("row.Access.DefaultEnabled = false, want true")
	}
	if row.Access.EffectiveEnabled {
		t.Fatal("row.Access.EffectiveEnabled = true, want false (disabled policy)")
	}
}

func TestListModels_AppliesProviderWideOverrideToConcreteModels(t *testing.T) {
	registry := newVMModelRegistry(t)
	service := newVMServiceForRegistry(t, registry, true, virtualmodels.VirtualModel{
		Source:    "openai/",
		UserPaths: []string{"/team/provider"},
		Enabled:   true,
	})

	h := NewHandler(nil, registry, WithVirtualModels(service))
	c, rec := newHandlerContext("/admin/models")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}

	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len(body) = %d, want 1", len(body))
	}
	row := body[0]
	if row.Access.Selector != "openai/gpt-4o" {
		t.Fatalf("row.Access.Selector = %q, want openai/gpt-4o", row.Access.Selector)
	}
	if len(row.Access.UserPaths) != 1 || row.Access.UserPaths[0] != "/team/provider" {
		t.Fatalf("row.Access.UserPaths = %#v, want [/team/provider]", row.Access.UserPaths)
	}
	if row.Access.Override != nil {
		t.Fatalf("row.Access.Override = %#v, want nil for provider-wide override", row.Access.Override)
	}
}

func TestListModels_AppliesGlobalOverrideToConcreteModels(t *testing.T) {
	registry := newVMModelRegistry(t)
	service := newVMServiceForRegistry(t, registry, true, virtualmodels.VirtualModel{
		Source:    "/",
		UserPaths: []string{"/team/global"},
		Enabled:   true,
	})

	h := NewHandler(nil, registry, WithVirtualModels(service))
	c, rec := newHandlerContext("/admin/models")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}

	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(body) != 1 {
		t.Fatalf("len(body) = %d, want 1", len(body))
	}
	row := body[0]
	if len(row.Access.UserPaths) != 1 || row.Access.UserPaths[0] != "/team/global" {
		t.Fatalf("row.Access.UserPaths = %#v, want [/team/global]", row.Access.UserPaths)
	}
	if row.Access.Override != nil {
		t.Fatalf("row.Access.Override = %#v, want nil for global override", row.Access.Override)
	}
}

func TestListModels_HidesHiddenByDefault(t *testing.T) {
	registry := newVMModelRegistry(t)
	svc, err := modelpreferences.NewService(
		mpHandlerFakeStore{rows: map[string]modelpreferences.Preference{
			"openai/gpt-4o": {Selector: "openai/gpt-4o", Hidden: true},
		}},
		mpHandlerFakeCatalog{names: []string{"openai"}},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("svc.Refresh: %v", err)
	}
	h := NewHandler(nil, registry, WithModelPreferences(svc))
	c, rec := newHandlerContext("/admin/models")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, row := range body {
		if row.Selector == "openai/gpt-4o" {
			t.Fatalf("hidden model appeared in default projection: %+v", row)
		}
	}
	if len(body) == 0 {
		t.Fatal("expected at least one visible model in the registry")
	}
}

func TestListModels_IncludeHiddenQueryShowsHiddenRows(t *testing.T) {
	registry := newVMModelRegistry(t)
	svc, err := modelpreferences.NewService(
		mpHandlerFakeStore{rows: map[string]modelpreferences.Preference{
			"openai/gpt-4o": {Selector: "openai/gpt-4o", Hidden: true},
		}},
		mpHandlerFakeCatalog{names: []string{"openai"}},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("svc.Refresh: %v", err)
	}
	h := NewHandler(nil, registry, WithModelPreferences(svc))
	c, rec := newHandlerContext("/admin/models?include_hidden=true")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	var body []modelInventoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	found := false
	for _, row := range body {
		if row.Selector == "openai/gpt-4o" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("include_hidden=true must surface hidden rows")
	}
}

func TestListModels_InvalidIncludeHiddenReturnsBadRequest(t *testing.T) {
	registry := newVMModelRegistry(t)
	svc, err := modelpreferences.NewService(
		mpHandlerFakeStore{},
		mpHandlerFakeCatalog{names: []string{"openai"}},
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if err := svc.Refresh(context.Background()); err != nil {
		t.Fatalf("svc.Refresh: %v", err)
	}
	h := NewHandler(nil, registry, WithModelPreferences(svc))
	c, rec := newHandlerContext("/admin/models?include_hidden=notabool")

	if err := h.ListModels(c); err != nil {
		t.Fatalf("ListModels() error = %v", err)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// mpHandlerFakeStore is a copy of mpFakeStore typed for the ListModels tests
// appended to this file. It mirrors the same shape but lives here to keep
// each test file self-contained for the package-level build.
type mpHandlerFakeStore struct {
	rows map[string]modelpreferences.Preference
}

func (s mpHandlerFakeStore) List(_ context.Context) ([]modelpreferences.Preference, error) {
	out := make([]modelpreferences.Preference, 0, len(s.rows))
	for _, p := range s.rows {
		out = append(out, p)
	}
	return out, nil
}

func (s mpHandlerFakeStore) Upsert(context.Context, modelpreferences.Preference) error {
	return nil
}

func (s mpHandlerFakeStore) Delete(context.Context, string) error { return nil }

func (s mpHandlerFakeStore) ResetAll(context.Context) error { return nil }

func (s mpHandlerFakeStore) Close() error { return nil }

type mpHandlerFakeCatalog struct{ names []string }

func (c mpHandlerFakeCatalog) ProviderNames() []string { return append([]string(nil), c.names...) }
