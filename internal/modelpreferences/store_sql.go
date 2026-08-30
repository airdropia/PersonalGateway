package modelpreferences

import (
	"context"
	"fmt"
	"time"

	"github.com/enterpilot/gomodel/internal/storage/sqlx"
)

// SQLStore stores model preferences in SQLite or PostgreSQL through sqlx.
type SQLStore struct {
	db sqlx.DB
}

var sqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS model_preferences (
		selector TEXT PRIMARY KEY,
		hidden ` + sqlx.TypeBool + ` NOT NULL DEFAULT FALSE,
		created_at ` + sqlx.TypeInt64 + ` NOT NULL,
		updated_at ` + sqlx.TypeInt64 + ` NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_model_preferences_updated_at ON model_preferences(updated_at DESC)`,
}

// NewSQLStore creates the model_preferences table and indexes if needed.
func NewSQLStore(ctx context.Context, db sqlx.DB) (*SQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if err := db.Schema(ctx, sqlSchema...); err != nil {
		return nil, fmt.Errorf("create model preferences schema: %w", err)
	}
	return &SQLStore{db: db}, nil
}

func (s *SQLStore) List(ctx context.Context) ([]Preference, error) {
	rows, err := s.db.Query(ctx, `
		SELECT selector, hidden, created_at, updated_at
		FROM model_preferences
		ORDER BY selector ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("list model preferences: %w", err)
	}
	defer rows.Close()

	result := make([]Preference, 0)
	for rows.Next() {
		var preference Preference
		var createdAt, updatedAt int64
		if err := rows.Scan(&preference.Selector, &preference.Hidden, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan model preference: %w", err)
		}
		preference.CreatedAt = time.Unix(createdAt, 0).UTC()
		preference.UpdatedAt = time.Unix(updatedAt, 0).UTC()
		result = append(result, preference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate model preferences: %w", err)
	}
	return result, nil
}

func (s *SQLStore) Upsert(ctx context.Context, preference Preference) error {
	now := time.Now().UTC()
	if preference.CreatedAt.IsZero() {
		preference.CreatedAt = now
	}
	preference.UpdatedAt = now
	_, err := s.db.Exec(ctx, `
		INSERT INTO model_preferences (selector, hidden, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(selector) DO UPDATE SET
			hidden = excluded.hidden,
			updated_at = excluded.updated_at
	`, preference.Selector, preference.Hidden, preference.CreatedAt.Unix(), preference.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert model preference: %w", err)
	}
	return nil
}

func (s *SQLStore) Delete(ctx context.Context, selector string) error {
	affected, err := s.db.Exec(ctx, `DELETE FROM model_preferences WHERE selector = ?`, selector)
	if err != nil {
		return fmt.Errorf("delete model preference: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLStore) ResetAll(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM model_preferences`); err != nil {
		return fmt.Errorf("reset model preferences: %w", err)
	}
	return nil
}

func (s *SQLStore) Close() error { return nil }
