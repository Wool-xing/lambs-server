package db

import (
	"os"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"
)

// redisTestSource returns a RedisSource gated on LAMBS_TEST_REDIS_ADDR
// plus a fixed key prefix; sub-keys differ per test so constant prefix
// is collision-free.
func redisTestSource(t *testing.T) (*RedisSource, string) {
	t.Helper()
	addr := os.Getenv("LAMBS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_REDIS_ADDR not set — real Redis verification skipped")
	}
	return &RedisSource{dsn: "redis://" + addr}, "lambs:probe:types:"
}

// TestRedisReadTypeMatrix — ReadItems across string/hash/list/set/zset and
// the missing-key default branch, plus client-side paging.
func TestRedisReadTypeMatrix(t *testing.T) {
	s, prefix := redisTestSource(t)
	ctx := t.Context()
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	defer c.Del(ctx, prefix+"string", prefix+"hash", prefix+"list", prefix+"set", prefix+"zset")

	c.Set(ctx, prefix+"string", "hello", 0)
	c.HSet(ctx, prefix+"hash", "f1", "v1")
	c.RPush(ctx, prefix+"list", "a", "b", "c")
	c.SAdd(ctx, prefix+"set", "m1", "m2")
	c.ZAdd(ctx, prefix+"zset", redisZ(1.5, "z1"))

	rows, _, _, err := s.ReadItems(prefix+"string", 10, 0)
	if err != nil || len(rows) != 1 || rows[0]["type"] != "string" {
		t.Fatalf("string rows = %v, %v", rows, err)
	}
	rows, _, _, err = s.ReadItems(prefix+"hash", 10, 0)
	if err != nil || len(rows) != 1 || rows[0]["field"] != "f1" {
		t.Fatalf("hash rows = %v, %v", rows, err)
	}
	rows, _, _, err = s.ReadItems(prefix+"list", 10, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("list rows = %v, %v", rows, err)
	}
	rows, _, _, err = s.ReadItems(prefix+"set", 10, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("set rows = %v, %v", rows, err)
	}
	rows, _, _, err = s.ReadItems(prefix+"zset", 10, 0)
	if err != nil || len(rows) != 1 || rows[0]["member"] != "z1" {
		t.Fatalf("zset rows = %v, %v", rows, err)
	}
	// missing key → go-redis Type returns "none" → default branch emits
	// one {key, type:"none"} row (CountItems treats none as empty).
	rows, _, _, err = s.ReadItems(prefix+"ghost", 10, 0)
	if err != nil || len(rows) != 1 || rows[0]["type"] != "none" {
		t.Fatalf("missing rows = %#v, %v", rows, err)
	}
	// paging: slice middle + offset past end → empty
	rows, _, _, err = s.ReadItems(prefix+"list", 1, 1)
	if err != nil || len(rows) != 1 || rows[0]["value"] != "b" {
		t.Fatalf("paged list rows = %v, %v", rows, err)
	}
	rows, _, _, err = s.ReadItems(prefix+"list", 1, 99)
	if err != nil || len(rows) != 0 {
		t.Fatalf("offset-past-end rows = %v, %v", rows, err)
	}
	if _, _, _, err := s.ReadItems("bad key with space", 10, 0); err == nil {
		t.Fatal("invalid key should error")
	}
}

// TestRedisInsertTypeMatrix — all five insert types + unsupported type error.
func TestRedisInsertTypeMatrix(t *testing.T) {
	s, prefix := redisTestSource(t)
	ctx := t.Context()
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	defer c.Del(ctx, prefix+"s", prefix+"h", prefix+"l", prefix+"se", prefix+"z")

	if err := s.InsertItem(prefix+"s", map[string]interface{}{"type": "string", "value": "v"}); err != nil {
		t.Fatalf("insert string: %v", err)
	}
	if err := s.InsertItem(prefix+"h", map[string]interface{}{"type": "hash", "field": "f", "value": "v"}); err != nil {
		t.Fatalf("insert hash: %v", err)
	}
	if err := s.InsertItem(prefix+"l", map[string]interface{}{"type": "list", "value": "v"}); err != nil {
		t.Fatalf("insert list: %v", err)
	}
	if err := s.InsertItem(prefix+"se", map[string]interface{}{"type": "set", "member": "m"}); err != nil {
		t.Fatalf("insert set: %v", err)
	}
	if err := s.InsertItem(prefix+"z", map[string]interface{}{"type": "zset", "score": "2.5", "member": "m"}); err != nil {
		t.Fatalf("insert zset: %v", err)
	}
	if err := s.InsertItem(prefix+"z", map[string]interface{}{"type": "stream", "value": "v"}); err == nil {
		t.Fatal("unsupported type should error")
	}
	if err := s.InsertItem("bad key", map[string]interface{}{"type": "string"}); err == nil {
		t.Fatal("invalid key should error")
	}
}

// TestRedisUpdateDeleteMatrix — list index replace, hash field replace,
// string falls through to insert path; delete removes the whole key.
func TestRedisUpdateDeleteMatrix(t *testing.T) {
	s, prefix := redisTestSource(t)
	ctx := t.Context()
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	defer c.Del(ctx, prefix+"ul", prefix+"uh", prefix+"us")

	c.RPush(ctx, prefix+"ul", "old")
	if err := s.UpdateItem(prefix+"ul", "index", "0", map[string]interface{}{"type": "list", "value": "new"}); err != nil {
		t.Fatalf("update list: %v", err)
	}
	if v, _ := c.LIndex(ctx, prefix+"ul", 0).Result(); v != "new" {
		t.Fatalf("list[0] = %q", v)
	}
	if err := s.UpdateItem(prefix+"ul", "index", "notanumber", map[string]interface{}{"type": "list", "value": "x"}); err == nil {
		t.Fatal("invalid list index should error")
	}
	c.HSet(ctx, prefix+"uh", "f", "old")
	if err := s.UpdateItem(prefix+"uh", "f", "f", map[string]interface{}{"type": "hash", "value": "new"}); err != nil {
		t.Fatalf("update hash: %v", err)
	}
	if v, _ := c.HGet(ctx, prefix+"uh", "f").Result(); v != "new" {
		t.Fatalf("hash f = %q", v)
	}
	// string type → default branch → insert path
	if err := s.UpdateItem(prefix+"us", "key", "ignored", map[string]interface{}{"type": "string", "value": "sv"}); err != nil {
		t.Fatalf("update string fallback: %v", err)
	}
	if v, _ := c.Get(ctx, prefix+"us").Result(); v != "sv" {
		t.Fatalf("string value = %q", v)
	}
	if err := s.DeleteItem(prefix+"us", "key", "ignored"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := c.Exists(ctx, prefix+"us").Result(); n != 0 {
		t.Fatal("key still exists after delete")
	}
}

// TestRedisCountTypeMatrix — none/string/hash/list/set/zset counts.
func TestRedisCountTypeMatrix(t *testing.T) {
	s, prefix := redisTestSource(t)
	ctx := t.Context()
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	defer c.Del(ctx, prefix+"c-s", prefix+"c-h", prefix+"c-l", prefix+"c-se", prefix+"c-z")

	if n, err := s.CountItems(prefix + "c-s"); err != nil || n != 0 {
		t.Fatalf("missing count = %d, %v", n, err)
	}
	c.Set(ctx, prefix+"c-s", "v", 0)
	if n, _ := s.CountItems(prefix + "c-s"); n != 1 {
		t.Fatalf("string count = %d", n)
	}
	c.HSet(ctx, prefix+"c-h", "a", "1", "b", "2")
	if n, _ := s.CountItems(prefix + "c-h"); n != 2 {
		t.Fatalf("hash count = %d", n)
	}
	c.RPush(ctx, prefix+"c-l", "1", "2", "3")
	if n, _ := s.CountItems(prefix + "c-l"); n != 3 {
		t.Fatalf("list count = %d", n)
	}
	c.SAdd(ctx, prefix+"c-se", "1", "2", "3", "4")
	if n, _ := s.CountItems(prefix + "c-se"); n != 4 {
		t.Fatalf("set count = %d", n)
	}
	c.ZAdd(ctx, prefix+"c-z", redisZ(1, "a"), redisZ(2, "b"))
	if n, _ := s.CountItems(prefix + "c-z"); n != 2 {
		t.Fatalf("zset count = %d", n)
	}
	if _, err := s.CountItems("bad key"); err == nil {
		t.Fatal("invalid key should error")
	}
}

// TestRedisListCollectionsCursorLoop — >200 keys force the SCAN cursor to
// iterate more than once; the full sweep must return every key.
func TestRedisListCollectionsCursorLoop(t *testing.T) {
	s, prefix := redisTestSource(t)
	ctx := t.Context()
	c, err := s.client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	defer c.Close()
	var keys []string
	for i := 0; i < 215; i++ {
		k := prefix + "scan:" + strconv.Itoa(i)
		keys = append(keys, k)
	}
	defer c.Del(ctx, keys...)
	pipe := c.Pipeline()
	for _, k := range keys {
		pipe.Set(ctx, k, "v", 0)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	cols, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	got := map[string]bool{}
	for _, k := range cols {
		got[k] = true
	}
	for _, k := range keys {
		if !got[k] {
			t.Fatalf("key %s missing from scan (got %d keys)", k, len(cols))
		}
	}
}

// redisZ helper keeps ZAdd call sites terse.
func redisZ(score float64, member string) redis.Z {
	return redis.Z{Score: score, Member: member}
}
