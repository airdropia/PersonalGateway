package modelpreferences

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoPreferenceDocument struct {
	ID        string    `bson:"_id"`
	Hidden    bool      `bson:"hidden"`
	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// MongoDBStore stores model preferences in MongoDB.
type MongoDBStore struct{ collection *mongo.Collection }

// NewMongoDBStore creates the model preference collection and index.
func NewMongoDBStore(database *mongo.Database) (*MongoDBStore, error) {
	if database == nil {
		return nil, fmt.Errorf("database is required")
	}
	collection := database.Collection("model_preferences")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{Keys: bson.D{{Key: "updated_at", Value: -1}}}); err != nil {
		return nil, fmt.Errorf("create model preference index: %w", err)
	}
	return &MongoDBStore{collection: collection}, nil
}

func (s *MongoDBStore) List(ctx context.Context) ([]Preference, error) {
	cursor, err := s.collection.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}))
	if err != nil {
		return nil, fmt.Errorf("list model preferences: %w", err)
	}
	defer cursor.Close(ctx)
	result := make([]Preference, 0)
	for cursor.Next(ctx) {
		var document mongoPreferenceDocument
		if err := cursor.Decode(&document); err != nil {
			return nil, fmt.Errorf("decode model preference: %w", err)
		}
		result = append(result, Preference{Selector: document.ID, Hidden: document.Hidden, CreatedAt: document.CreatedAt.UTC(), UpdatedAt: document.UpdatedAt.UTC()})
	}
	if err := cursor.Err(); err != nil {
		return nil, fmt.Errorf("iterate model preferences: %w", err)
	}
	return result, nil
}

func (s *MongoDBStore) Upsert(ctx context.Context, preference Preference) error {
	now := time.Now().UTC()
	if preference.CreatedAt.IsZero() {
		preference.CreatedAt = now
	}
	preference.UpdatedAt = now
	_, err := s.collection.UpdateOne(ctx, bson.M{"_id": strings.TrimSpace(preference.Selector)}, bson.M{
		"$set": bson.M{"hidden": preference.Hidden, "updated_at": preference.UpdatedAt},
		"$setOnInsert": bson.M{"created_at": preference.CreatedAt},
	}, options.UpdateOne().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("upsert model preference: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Delete(ctx context.Context, selector string) error {
	result, err := s.collection.DeleteOne(ctx, bson.M{"_id": strings.TrimSpace(selector)})
	if err != nil {
		return fmt.Errorf("delete model preference: %w", err)
	}
	if result.DeletedCount == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *MongoDBStore) ResetAll(ctx context.Context) error {
	if _, err := s.collection.DeleteMany(ctx, bson.M{}); err != nil {
		return fmt.Errorf("reset model preferences: %w", err)
	}
	return nil
}

func (s *MongoDBStore) Close() error { return nil }