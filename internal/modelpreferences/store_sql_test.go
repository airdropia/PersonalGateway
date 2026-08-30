package modelpreferences

import (
	"context"
	"testing"
	"time"

	"github.com/enterpilot/gomodel/internal/storage/sqlx"
	"github.com/enterpilot/gomodel/internal/storage/sqlx/sqlxtest"
)

// runSQLStoreTest executes body once per available SQL dialect. SQLite always
// runs against an in-memory database; PostgreSQL runs only when the
// GOMODEL_TEST_POSTGRES_URL environment variable points at a reachable
// server. This keeps a single test definition covering both backends without
// doubling the suite.
func runSQLStoreTest(t *testing.T, body func(t *testing.T, store *SQLStore, db sqlx.DB)) {
	t.Helper()
	sqlxtest.Run(t, func(t *testing.T, db sqlx.DB) {
		store, err := NewSQLStore(context.Background(), db)
		if err != nil {
			t.Fatalf("NewSQLStore: %v", err)
		}
		body(t, store, db)
	})
}

func TestSQLStore_UpsertListDeleteRoundTrip(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		exact := Preference{Selector: "openai/gpt-4o", Hidden: true, CreatedAt: now, UpdatedAt: now}
		provider := Preference{Selector: "openai/", Hidden: false, CreatedAt: now, UpdatedAt: now}
		if err := store.Upsert(ctx, exact); err != nil {
			t.Fatalf("Upsert exact: %v", err)
		}
		if err := store.Upsert(ctx, provider); err != nil {
			t.Fatalf("Upsert provider: %v", err)
		}

		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 2 {
			t.Fatalf("len(list) = %d, want 2", len(list))
		}
		// List must be ordered by selector for stable UI rendering.
		if list[0].Selector != "openai/" || list[1].Selector != "openai/gpt-4o" {
			t.Fatalf("list order = %q,%q", list[0].Selector, list[1].Selector)
		}
		if !list[1].Hidden {
			t.Fatal("hidden flag lost on read")
		}
		if list[1].CreatedAt.IsZero() || list[1].UpdatedAt.IsZero() {
			t.Fatal("timestamps lost on read")
		}

		if err := store.Delete(ctx, "openai/gpt-4o"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		list, err = store.List(ctx)
		if err != nil {
			t.Fatalf("List after delete: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("len(list) = %d, want 1", len(list))
		}
	})
}

// TestSQLStore_UpsertIsIdempotent makes sure re-applying the same selector
// does not produce a duplicate row and updates the timestamp.
func TestSQLStore_UpsertIsIdempotent(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		ctx := context.Background()
		first := Preference{Selector: "openai/gpt-4o", Hidden: true, CreatedAt: time.Now().UTC()}
		if err := store.Upsert(ctx, first); err != nil {
			t.Fatalf("first Upsert: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
		second := Preference{Selector: "openai/gpt-4o", Hidden: false, CreatedAt: time.Now().UTC()}
		if err := store.Upsert(ctx, second); err != nil {
			t.Fatalf("second Upsert: %v", err)
		}

		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 1 {
			t.Fatalf("len(list) = %d, want 1 (Upsert must be idempotent)", len(list))
		}
		if list[0].Hidden {
			t.Fatal("hidden flag not updated by second Upsert")
		}
		if !list[0].UpdatedAt.After(first.UpdatedAt) {
			t.Fatalf("UpdatedAt not advanced: first=%v second=%v", first.UpdatedAt, list[0].UpdatedAt)
		}
	})
}

// TestSQLStore_DeleteUnknownReturnsNotFound pins the contract that DELETE on
// a missing row is a typed error rather than a silent no-op. The admin
// endpoint relies on this to return 404.
func TestSQLStore_DeleteUnknownReturnsNotFound(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		err := store.Delete(context.Background(), "nope/missing")
		if err != ErrNotFound {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})
}

// TestSQLStore_ResetAllClearsAllRows verifies the reset path leaves the
// table empty and ready for new writes.
func TestSQLStore_ResetAllClearsAllRows(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		ctx := context.Background()
		for _, sel := range []string{"openai/gpt-4o", "openai/", "gpt-4o-mini", "/"} {
			if err := store.Upsert(ctx, Preference{Selector: sel, Hidden: true}); err != nil {
				t.Fatalf("Upsert(%q): %v", sel, err)
			}
		}
		if err := store.ResetAll(ctx); err != nil {
			t.Fatalf("ResetAll: %v", err)
		}
		list, err := store.List(ctx)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) != 0 {
			t.Fatalf("len(list) = %d, want 0", len(list))
		}
		// After reset, writes must continue to succeed.
		if err := store.Upsert(ctx, Preference{Selector: "openai/gpt-4o", Hidden: true}); err != nil {
			t.Fatalf("post-reset Upsert: %v", err)
		}
	})
}

// TestSQLStore_SchemaIsIdempotent confirms repeated Schema calls do not
// error, since NewSQLStore is invoked once per process but tests may create
// several stores against the same database.
func TestSQLStore_SchemaIsIdempotent(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, db sqlx.DB) {
		ctx := context.Background()
		if err := db.Schema(ctx, sqlSchema...); err != nil {
			t.Fatalf("re-applying schema: %v", err)
		}
		// Smoke test: a write still works after re-applying the schema.
		if err := store.Upsert(ctx, Preference{Selector: "openai/gpt-4o", Hidden: true}); err != nil {
			t.Fatalf("Upsert after re-schema: %v", err)
		}
	})
}