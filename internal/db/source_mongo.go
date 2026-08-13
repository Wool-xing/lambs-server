package db

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoSource implements DataSource for MongoDB-managed projects.
// DSN format: mongodb://user:pass@host:27017/dbname
type MongoSource struct {
	dsn string
}

func (s *MongoSource) connect(ctx context.Context) (*mongo.Client, *mongo.Database, error) {
	u, err := url.Parse(s.dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid mongodb dsn: %w", err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	if dbName == "" {
		return nil, nil, fmt.Errorf("mongodb dsn missing database name")
	}
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(s.dsn).SetConnectTimeout(5*time.Second))
	if err != nil {
		return nil, nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		client.Disconnect(ctx)
		return nil, nil, err
	}
	return client, client.Database(dbName), nil
}

// docToRow converts a BSON document to a JSON-friendly map.
func docToRow(doc bson.M) map[string]interface{} {
	row := make(map[string]interface{}, len(doc))
	for k, v := range doc {
		if id, ok := v.(primitive.ObjectID); ok {
			row[k] = id.Hex()
		} else {
			row[k] = v
		}
	}
	return row
}

// pkFilter builds the query filter for _id or a string/int field.
func pkFilter(pkCol, pkVal string) bson.M {
	if pkCol == "_id" {
		if id, err := primitive.ObjectIDFromHex(pkVal); err == nil {
			return bson.M{"_id": id}
		}
	}
	return bson.M{pkCol: pkVal}
}

func (s *MongoSource) ListCollections() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, db, err := s.connect(ctx)
	if err != nil {
		return []string{}, err
	}
	defer client.Disconnect(ctx)
	names, err := db.ListCollectionNames(ctx, bson.D{})
	if err != nil {
		return []string{}, err
	}
	return names, nil
}

func (s *MongoSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
	if err := validateTable(collection); err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, db, err := s.connect(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	defer client.Disconnect(ctx)
	cursor, err := db.Collection(collection).Find(ctx, bson.D{}, options.Find().SetLimit(200))
	if err != nil {
		return nil, nil, "", err
	}
	defer cursor.Close(ctx)
	rows := []map[string]interface{}{}
	colSet := map[string]bool{}
	for cursor.Next(ctx) {
		var doc bson.M
		if err := cursor.Decode(&doc); err != nil {
			continue
		}
		row := docToRow(doc)
		for k := range row {
			colSet[k] = true
		}
		rows = append(rows, row)
	}
	cols := make([]string, 0, len(colSet))
	for c := range colSet {
		cols = append(cols, c)
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	return rows, cols, "_id", nil
}

func (s *MongoSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, db, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect(ctx)
	if _, err := db.Collection(collection).InsertOne(ctx, data); err != nil {
		return err
	}
	return nil
}

func (s *MongoSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, db, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect(ctx)
	set := bson.M{}
	for k, v := range data {
		if k != pkCol {
			set[k] = v
		}
	}
	if _, err := db.Collection(collection).UpdateOne(ctx, pkFilter(pkCol, pkVal), bson.M{"$set": set}); err != nil {
		return err
	}
	return nil
}

func (s *MongoSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateTable(collection); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, db, err := s.connect(ctx)
	if err != nil {
		return err
	}
	defer client.Disconnect(ctx)
	if _, err := db.Collection(collection).DeleteOne(ctx, pkFilter(pkCol, pkVal)); err != nil {
		return err
	}
	return nil
}
