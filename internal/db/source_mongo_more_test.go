package db

import (
	"os"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func mongoTestSource(t *testing.T) (*MongoSource, string) {
	t.Helper()
	addr := os.Getenv("LAMBS_TEST_MONGO_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_MONGO_ADDR not set — real MongoDB verification skipped")
	}
	return &MongoSource{dsn: "mongodb://" + addr + "/lambs_probe_db"}, "lambs_more_probe"
}

// TestMongoListCollections — real Mongo: created collections are listed.
func TestMongoListCollections(t *testing.T) {
	s, col := mongoTestSource(t)
	ctx := t.Context()
	client, mdb, err := s.connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)
	defer mdb.Collection(col).Drop(ctx)
	if err := mdb.CreateCollection(ctx, col); err != nil {
		t.Fatalf("create: %v", err)
	}

	names, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	found := false
	for _, n := range names {
		if n == col {
			found = true
		}
	}
	if !found {
		t.Fatalf("collection %q missing from %v", col, names)
	}
}

// TestMongoUpdateItem — update by ObjectID _id and by string pk; pk column
// must be excluded from the $set document.
func TestMongoUpdateItem(t *testing.T) {
	s, col := mongoTestSource(t)
	ctx := t.Context()
	client, mdb, err := s.connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)
	defer mdb.Collection(col).Drop(ctx)

	// _id path (ObjectIDFromHex branch inside pkFilter)
	res, err := mdb.Collection(col).InsertOne(ctx, bson.M{"name": "old", "n": 1})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	oid := res.InsertedID.(primitive.ObjectID)
	if err := s.UpdateItem(col, "_id", oid.Hex(), map[string]interface{}{"name": "new", "n": 2}); err != nil {
		t.Fatalf("update _id: %v", err)
	}
	var doc bson.M
	if err := mdb.Collection(col).FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		t.Fatalf("find: %v", err)
	}
	if doc["name"] != "new" || doc["n"] != int32(2) {
		t.Fatalf("doc = %v", doc)
	}

	// string pk path + pk excluded from $set
	if err := s.InsertItem(col, map[string]interface{}{"code": "k1", "name": "old2"}); err != nil {
		t.Fatalf("insert2: %v", err)
	}
	if err := s.UpdateItem(col, "code", "k1", map[string]interface{}{"name": "new2", "code": "should-not-move"}); err != nil {
		t.Fatalf("update string pk: %v", err)
	}
	var doc2 bson.M
	if err := mdb.Collection(col).FindOne(ctx, bson.M{"code": "k1"}).Decode(&doc2); err != nil {
		t.Fatalf("find2: %v", err)
	}
	if doc2["name"] != "new2" || doc2["code"] != "k1" {
		t.Fatalf("doc2 = %v", doc2)
	}
}

// TestMongoConnectError — unreachable addr surfaces connect errors from
// every CRUD entry point (dial timeout is bounded by the driver default).
func TestMongoConnectError(t *testing.T) {
	// serverSelectionTimeoutMS keeps 6 dial-failure probes at ~3s total
	// instead of 6 × 10s context timeouts.
	s := &MongoSource{dsn: "mongodb://127.0.0.1:1/lambs_probe_db?serverSelectionTimeoutMS=500"}
	if _, err := s.ListCollections(); err == nil {
		t.Fatal("ListCollections on closed port should error")
	}
	if _, err := s.CountItems("c"); err == nil {
		t.Fatal("CountItems on closed port should error")
	}
	if _, _, _, err := s.ReadItems("c", 10, 0); err == nil {
		t.Fatal("ReadItems on closed port should error")
	}
	if err := s.InsertItem("c", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("InsertItem on closed port should error")
	}
	if err := s.UpdateItem("c", "id", "1", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("UpdateItem on closed port should error")
	}
	if err := s.DeleteItem("c", "id", "1"); err == nil {
		t.Fatal("DeleteItem on closed port should error")
	}
}
