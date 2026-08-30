package modelpreferences

import (
	"context"
	"errors"

	"github.com/enterpilot/gomodel/internal/modelselectors"
)

// ErrNotFound indicates that a preference row does not exist.
var ErrNotFound = errors.New("model preference not found")

// ValidationError identifies invalid model preference input.
type ValidationError = modelselectors.ValidationError

// IsValidationError reports whether err is a model selector validation error.
func IsValidationError(err error) bool { return modelselectors.IsValidationError(err) }

// Store defines persistence operations for model visibility preferences.
type Store interface {
	List(ctx context.Context) ([]Preference, error)
	Upsert(ctx context.Context, preference Preference) error
	Delete(ctx context.Context, selector string) error
	ResetAll(ctx context.Context) error
	Close() error
}