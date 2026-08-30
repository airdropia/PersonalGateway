package codexoauth

import (
	"context"
	"testing"

	"github.com/enterpilot/gomodel/internal/storage/sqlx"
	"github.com/enterpilot/gomodel/internal/storage/sqlx/sqlxtest"
)

// runSQLStoreTest is the SQL-dialect test helper that mirrors the
// pattern used by every other SQL-backed store in the codebase: the
// suite runs once per available dialect through sqlxtest.Run.
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

// TestSQLStore_UpsertGetDeleteRoundTrip pins the SQL store contract:
// one row per provider_name, Get returns ErrNotFound when missing,
// Delete removes the row, and a re-upsert after delete succeeds.
func TestSQLStore_UpsertGetDeleteRoundTrip(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		ctx := context.Background()

		// Initial get is ErrNotFound.
		if _, err := store.GetByProvider(ctx, "chatgpt"); err != ErrNotFound {
			t.Fatalf("initial Get err = %v, want ErrNotFound", err)
		}

		// Upsert + read back.
		want := Connection{
			ProviderName:    "chatgpt",
			AccountID:       "acc-1",
			Email:           "user@example.com",
			Plan:            "pro",
			AccessToken:     "access-1",
			RefreshToken:    "refresh-1",
			IDToken:         "id-1",
			AccessExpiresAt: 1700000000,
			LastRefreshAt:   1699999000,
			CreatedAt:       1699998000,
			UpdatedAt:       1699999000,
			Status:          "active",
		}
		if err := store.Upsert(ctx, want); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		got, err := store.GetByProvider(ctx, "chatgpt")
		if err != nil {
			t.Fatalf("GetByProvider: %v", err)
		}
		if got.ProviderName != want.ProviderName || got.AccountID != want.AccountID {
			t.Fatalf("Get mismatch: got %+v, want %+v", got, want)
		}
		if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
			t.Fatal("token roundtrip lost values")
		}

		// Delete removes the row.
		if err := store.Delete(ctx, "chatgpt"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, err := store.GetByProvider(ctx, "chatgpt"); err != ErrNotFound {
			t.Fatalf("after Delete Get err = %v, want ErrNotFound", err)
		}

		// Deleting again is ErrNotFound.
		if err := store.Delete(ctx, "chatgpt"); err != ErrNotFound {
			t.Fatalf("second Delete err = %v, want ErrNotFound", err)
		}

		// Re-upsert after delete succeeds.
		if err := store.Upsert(ctx, want); err != nil {
			t.Fatalf("post-delete Upsert: %v", err)
		}
	})
}

// TestSQLStore_UpsertReplaces pins the documented single-row-per-name
// contract: a second Upsert for the same provider name overwrites the
// existing row.
func TestSQLStore_UpsertReplaces(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		ctx := context.Background()

		first := Connection{
			ProviderName: "chatgpt",
			AccountID:    "acc-1",
			AccessToken:  "first",
			RefreshToken: "rt-first",
			Status:       "active",
		}
		if err := store.Upsert(ctx, first); err != nil {
			t.Fatalf("first Upsert: %v", err)
		}

		second := first
		second.AccessToken = "second"
		second.RefreshToken = "rt-second"
		if err := store.Upsert(ctx, second); err != nil {
			t.Fatalf("second Upsert: %v", err)
		}

		got, err := store.GetByProvider(ctx, "chatgpt")
		if err != nil {
			t.Fatalf("GetByProvider: %v", err)
		}
		if got.AccessToken != "second" || got.RefreshToken != "rt-second" {
			t.Fatalf("Upsert did not replace row: %+v", got)
		}
	})
}

// TestSQLStore_UpsertRejectsMissingAccessToken pins the API guard:
// the service writes connections through the store, and a token-less
// row must fail fast rather than land in the table.
func TestSQLStore_UpsertRejectsMissingAccessToken(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, store *SQLStore, _ sqlx.DB) {
		if err := store.Upsert(context.Background(), Connection{ProviderName: "chatgpt"}); err == nil {
			t.Fatal("Upsert without access_token should fail")
		}
	})
}

// TestSQLStore_SchemaIdempotent confirms the schema is applied
// repeatedly without error: a second NewSQLStore on the same database
// must not duplicate the table.
func TestSQLStore_SchemaIdempotent(t *testing.T) {
	runSQLStoreTest(t, func(t *testing.T, _ *SQLStore, db sqlx.DB) {
		if _, err := NewSQLStore(context.Background(), db); err != nil {
			t.Fatalf("second NewSQLStore: %v", err)
		}
	})
}