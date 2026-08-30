package codexoauth

import (
	"context"
	"fmt"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/enterpilot/gomodel/internal/storage"
	"github.com/enterpilot/gomodel/internal/storage/sqlx"
)

// Result owns the Codex OAuth service and its store. It mirrors the
// modelpreferences.Result shape so the personal app wiring code can
// treat the two subsystems identically.
type Result struct {
	Service *Service
	Store   Store

	closeOnce sync.Once
	closeErr  error
}

// Close releases the underlying store. The Service has no goroutines
// that need explicit shutdown.
func (r *Result) Close() error {
	if r == nil {
		return nil
	}
	r.closeOnce.Do(func() {
		if r.Store != nil {
			r.closeErr = r.Store.Close()
		}
	})
	return r.closeErr
}

// New initializes the Codex OAuth subsystem against the shared storage
// backend. The token client is the production HTTP client; tests build
// their own Service via NewService.
func New(ctx context.Context, shared storage.Storage) (*Result, error) {
	if shared == nil {
		return nil, fmt.Errorf("shared storage is required")
	}
	store, err := storage.ResolveSQLBackend[Store](ctx, shared,
		func(db sqlx.DB) (Store, error) { return NewSQLStore(ctx, db) },
		func(database *mongo.Database) (Store, error) { return NewMongoDBStore(database) },
	)
	if err != nil {
		return nil, fmt.Errorf("resolve codex oauth backend: %w", err)
	}
	client := NewHTTPTokenClient(nil)
	service, err := NewService(store, client)
	if err != nil {
		return nil, fmt.Errorf("create codex oauth service: %w", err)
	}
	return &Result{Service: service, Store: store}, nil
}