package db

import (
	"os"
	"testing"
)

// TestRedisSourceRealCRUD — real redis: hash-type insert/read/update/delete
// round-trip (collection IS the key). Gated on LAMBS_TEST_REDIS_ADDR
// (docker lambs-redis-test on 127.0.0.1:6380).
func TestRedisSourceRealCRUD(t *testing.T) {
	addr := os.Getenv("LAMBS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_REDIS_ADDR not set — real Redis verification skipped")
	}
	s := &RedisSource{dsn: "redis://" + addr}
	key := "lambs:probe:test-key"
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	c.Del(t.Context(), key).Result()

	if err := s.InsertItem(key, map[string]interface{}{"type": "hash", "field": "value", "value": "hello"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, cols, pk, err := s.ReadItems(key, 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0]["value"] != "hello" || len(cols) == 0 || pk == "" {
		t.Fatalf("read = %v cols=%v pk=%q", rows, cols, pk)
	}
	if n, err := s.CountItems(key); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if err := s.UpdateItem(key, pk, "value", map[string]interface{}{"type": "hash", "value": "world"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, _ = s.ReadItems(key, 10, 0)
	if rows[0]["value"] != "world" {
		t.Fatalf("updated value = %v", rows[0]["value"])
	}
	if err := s.DeleteItem(key, pk, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := s.CountItems(key); n != 0 {
		t.Fatalf("count after delete = %d", n)
	}
}

// TestMongoSourceRealCRUD — real mongo: insert/read/delete via the adapter
// (dsn needs an explicit database name). Gated on LAMBS_TEST_MONGO_ADDR.
func TestMongoSourceRealCRUD(t *testing.T) {
	addr := os.Getenv("LAMBS_TEST_MONGO_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_MONGO_ADDR not set — real MongoDB verification skipped")
	}
	s := &MongoSource{dsn: "mongodb://" + addr + "/lambs_probe_db"}
	col := "lambs_probe"
	ctx := t.Context()
	client, db, err := s.connect(ctx)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer client.Disconnect(ctx)
	db.Collection(col).Drop(ctx)

	if err := s.InsertItem(col, map[string]interface{}{"name": "中文文档", "n": 7}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, cols, pk, err := s.ReadItems(col, 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "中文文档" || rows[0]["n"] != int32(7) {
		t.Fatalf("read = %v", rows)
	}
	if len(cols) == 0 || pk == "" {
		t.Fatalf("cols=%v pk=%q", cols, pk)
	}
	if n, err := s.CountItems(col); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if err := s.DeleteItem(col, "_id", rows[0]["_id"].(string)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := s.CountItems(col); n != 0 {
		t.Fatalf("count after delete = %d", n)
	}
}
