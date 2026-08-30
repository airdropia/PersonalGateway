package modelpreferences

import (
	"context"
	"fmt"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/enterpilot/gomodel/internal/providers"
	"github.com/enterpilot/gomodel/internal/storage"
	"github.com/enterpilot/gomodel/internal/storage/sqlx"
)

// Result owns the model preference store and service.
type Result struct {
	Service *Service
	Store   Store

	closeOnce sync.Once
	closeErr  error
}

// Close releases resources owned by the subsystem.
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

// New initializes model visibility preferences against the shared backend.
func New(ctx context.Context, shared storage.Storage, catalog *providers.ModelRegistry) (*Result, error) {
	if shared == nil {
		return nil, fmt.Errorf("shared storage is required")
	}
	if catalog == nil {
		return nil, fmt.Errorf("model registry is required")
	}
	store, err := storage.ResolveSQLBackend[Store](ctx, shared,
		func(db sqlx.DB) (Store, error) { return NewSQLStore(ctx, db) },
		func(database *mongo.Database) (Store, error) { return NewMongoDBStore(database) },
	)
	if err != nil {
		return nil, err
	}
	service, err := NewService(store, catalog)
	if err != nil {
		return nil, err
	}
	if err := service.Refresh(ctx); err != nil {
		return nil, err
	}
	return &Result{Service: service, Store: store}, nil
}
