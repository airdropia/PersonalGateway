package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/enterpilot/gomodel/internal/storage/sqlx"
)

// Store persists one Codex OAuth connection per provider name. The
// package never indexes by account id at the SQL level: a single
// provider-name row can be replaced when the operator runs a fresh
// login. Multi-account support is a Stage 8 concern.
type Store interface {
	GetByProvider(ctx context.Context, providerName string) (*Connection, error)
	Upsert(ctx context.Context, c Connection) error
	Delete(ctx context.Context, providerName string) error
	Close() error
}

// ErrNotFound is returned when no connection exists for the requested
// provider name. Callers should surface this as a "not connected" hint.
var ErrNotFound = errors.New("codex oauth: no connection stored for this provider")

// SQLStore is the sqlite/postgres implementation of Store. The schema
// mirrors the SQL convention used by modelpreferences: a single table
// with the provider name as the natural primary key.
type SQLStore struct {
	db sqlx.DB
}

// sqlSchema is the CREATE TABLE statement applied during NewSQLStore. The
// table lives in the shared gateway database so storage cleanup helpers
// (vacuum, retention, backup) treat it as part of the same SQL graph.
var sqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS codex_oauth_connections (
		provider_name TEXT PRIMARY KEY,
		account_id TEXT NOT NULL,
		email TEXT NOT NULL,
		plan TEXT NOT NULL,
		access_token TEXT NOT NULL,
		refresh_token TEXT NOT NULL,
		id_token TEXT NOT NULL,
		access_expires_at ` + sqlx.TypeInt64 + ` NOT NULL,
		last_refresh_at ` + sqlx.TypeInt64 + ` NOT NULL,
		created_at ` + sqlx.TypeInt64 + ` NOT NULL,
		updated_at ` + sqlx.TypeInt64 + ` NOT NULL,
		status TEXT NOT NULL DEFAULT 'active'
	)`,
}

// NewSQLStore creates the schema if needed and returns the store. The
// caller owns the sqlx.DB and is responsible for closing it.
func NewSQLStore(ctx context.Context, db sqlx.DB) (*SQLStore, error) {
	if db == nil {
		return nil, fmt.Errorf("database connection is required")
	}
	if err := db.Schema(ctx, sqlSchema...); err != nil {
		return nil, fmt.Errorf("create codex oauth schema: %w", err)
	}
	return &SQLStore{db: db}, nil
}

// Close releases the underlying sqlx database reference. The caller
// owns the underlying connection and is responsible for closing it.
func (s *SQLStore) Close() error { return nil }

// GetByProvider loads the single row for a provider name. Returns
// ErrNotFound when no row exists so callers can branch cleanly.
func (s *SQLStore) GetByProvider(ctx context.Context, providerName string) (*Connection, error) {
	rows, err := s.db.Query(ctx, `
		SELECT provider_name, account_id, email, plan,
		       access_token, refresh_token, id_token,
		       access_expires_at, last_refresh_at,
		       created_at, updated_at, status
		FROM codex_oauth_connections
		WHERE provider_name = ?
	`, providerName)
	if err != nil {
		return nil, fmt.Errorf("query codex oauth connection: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	var (
		c                    Connection
		accessExpiresAt      int64
		lastRefreshAt        int64
		createdAt, updatedAt int64
	)
	if err := rows.Scan(
		&c.ProviderName, &c.AccountID, &c.Email, &c.Plan,
		&c.AccessToken, &c.RefreshToken, &c.IDToken,
		&accessExpiresAt, &lastRefreshAt,
		&createdAt, &updatedAt, &c.Status,
	); err != nil {
		return nil, fmt.Errorf("scan codex oauth connection: %w", err)
	}
	c.AccessExpiresAt = accessExpiresAt
	c.LastRefreshAt = lastRefreshAt
	c.CreatedAt = createdAt
	c.UpdatedAt = updatedAt
	return &c, nil
}

// Upsert writes the connection under its provider name, replacing any
// existing row. Refresh metadata is preserved when the caller passes
// zero values.
func (s *SQLStore) Upsert(ctx context.Context, c Connection) error {
	if c.AccessToken == "" {
		return fmt.Errorf("access_token is required")
	}
	now := time.Now().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = "active"
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO codex_oauth_connections (
			provider_name, account_id, email, plan,
			access_token, refresh_token, id_token,
			access_expires_at, last_refresh_at,
			created_at, updated_at, status
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(provider_name) DO UPDATE SET
			account_id = excluded.account_id,
			email = excluded.email,
			plan = excluded.plan,
			access_token = excluded.access_token,
			refresh_token = excluded.refresh_token,
			id_token = excluded.id_token,
			access_expires_at = excluded.access_expires_at,
			last_refresh_at = excluded.last_refresh_at,
			updated_at = excluded.updated_at,
			status = excluded.status
	`,
		c.ProviderName, c.AccountID, c.Email, c.Plan,
		c.AccessToken, c.RefreshToken, c.IDToken,
		c.AccessExpiresAt, c.LastRefreshAt,
		c.CreatedAt, c.UpdatedAt, c.Status,
	)
	if err != nil {
		return fmt.Errorf("upsert codex oauth connection: %w", err)
	}
	return nil
}

// Delete removes the connection for a provider name. Returns
// ErrNotFound when no row existed.
func (s *SQLStore) Delete(ctx context.Context, providerName string) error {
	affected, err := s.db.Exec(ctx, `DELETE FROM codex_oauth_connections WHERE provider_name = ?`, providerName)
	if err != nil {
		return fmt.Errorf("delete codex oauth connection: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// MongoStore is the MongoDB implementation of Store. It is wired in the
// same way as the SQL store so the personal gateway can pick the backend
// via the existing storage selector.
type MongoStore struct {
	db *mongo.Database
}

// NewMongoDBStore creates a MongoDB-backed Store. The database is the
// caller-owned connection; the store does not close it.
func NewMongoDBStore(database *mongo.Database) (*MongoStore, error) {
	if database == nil {
		return nil, fmt.Errorf("mongo database is required")
	}
	return &MongoStore{db: database}, nil
}

// Close releases the underlying mongo collection reference. The
// caller owns the *mongo.Database and is responsible for closing it.
func (s *MongoStore) Close() error { return nil }

// GetByProvider loads the single document keyed by provider_name.
func (s *MongoStore) GetByProvider(ctx context.Context, providerName string) (*Connection, error) {
	var conn Connection
	err := s.db.Collection("codex_oauth_connections").
		FindOne(ctx, bson.M{"provider_name": providerName}).
		Decode(&conn)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("query codex oauth connection: %w", err)
	}
	return &conn, nil
}

// Upsert writes the document under its provider name, replacing any
// existing record.
func (s *MongoStore) Upsert(ctx context.Context, c Connection) error {
	if c.AccessToken == "" {
		return fmt.Errorf("access_token is required")
	}
	now := time.Now().Unix()
	if c.CreatedAt == 0 {
		c.CreatedAt = now
	}
	c.UpdatedAt = now
	if c.Status == "" {
		c.Status = "active"
	}
	_, err := s.db.Collection("codex_oauth_connections").ReplaceOne(
		ctx, bson.M{"provider_name": c.ProviderName}, c,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("upsert codex oauth connection: %w", err)
	}
	return nil
}

// Delete removes the document for a provider name. Returns
// ErrNotFound when no document existed.
func (s *MongoStore) Delete(ctx context.Context, providerName string) error {
	res, err := s.db.Collection("codex_oauth_connections").DeleteOne(ctx, bson.M{"provider_name": providerName})
	if err != nil {
		return fmt.Errorf("delete codex oauth connection: %w", err)
	}
	if res.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}