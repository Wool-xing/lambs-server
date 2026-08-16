package db

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisSource implements DataSource for Redis-managed projects.
// DSN format: redis://[user:pass@]host:6379/0
// Collections = keys. ReadItems expands a key by its type into rows.
type RedisSource struct {
	dsn string
}

// validateKey allows the characters legal in Redis keys (SQL table-name
// rules do not apply — keys routinely contain dashes, colons, dots).
func validateKey(k string) error {
	if k == "" {
		return fmt.Errorf("invalid key")
	}
	for _, c := range k {
		if c <= 32 || c == '"' || c == '\'' || c == '\\' {
			return fmt.Errorf("invalid key character")
		}
	}
	return nil
}

func (s *RedisSource) client() (*redis.Client, error) {
	u, err := url.Parse(s.dsn)
	if err != nil {
		return nil, fmt.Errorf("invalid redis dsn: %w", err)
	}
	db := 0
	if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			db = n
		}
	}
	opts := &redis.Options{Addr: u.Host, DB: db, DialTimeout: 5 * time.Second}
	if u.User != nil {
		opts.Username = u.User.Username()
		opts.Password, _ = u.User.Password()
	}
	return redis.NewClient(opts), nil
}

func (s *RedisSource) ListCollections() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.client()
	if err != nil {
		return []string{}, err
	}
	defer c.Close()
	// Full key sweep — a single SCAN call caps at count keys, silently
	// truncating larger keyspaces. Loop the cursor to completion.
	keys := []string{}
	cursor := uint64(0)
	for {
		batch, next, err := c.Scan(ctx, cursor, "*", 200).Result()
		if err != nil {
			return []string{}, err
		}
		keys = append(keys, batch...)
		if next == 0 {
			break
		}
		cursor = next
	}
	return keys, nil
}

// keyType returns the Redis type name of a key (empty if missing).
func (s *RedisSource) keyType(ctx context.Context, c *redis.Client, key string) string {
	t, err := c.Type(ctx, key).Result()
	if err != nil {
		return ""
	}
	return t
}

func (s *RedisSource) ReadItems(collection string, limit, offset int) ([]map[string]interface{}, []string, string, error) {
	if err := validateKey(collection); err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.client()
	if err != nil {
		return nil, nil, "", err
	}
	defer c.Close()
	key := collection
	t := s.keyType(ctx, c, key)
	rows := []map[string]interface{}{}
	switch t {
	case "string":
		v, err := c.Get(ctx, key).Result()
		if err != nil && err != redis.Nil {
			return nil, nil, "", err
		}
		rows = append(rows, map[string]interface{}{"key": key, "type": t, "value": v})
	case "hash":
		fields, err := c.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, nil, "", err
		}
		for f, v := range fields {
			rows = append(rows, map[string]interface{}{"key": key, "type": t, "field": f, "value": v})
		}
	case "list":
		vals, err := c.LRange(ctx, key, 0, -1).Result()
		if err != nil {
			return nil, nil, "", err
		}
		for i, v := range vals {
			rows = append(rows, map[string]interface{}{"key": key, "type": t, "index": i, "value": v})
		}
	case "set":
		members, err := c.SMembers(ctx, key).Result()
		if err != nil {
			return nil, nil, "", err
		}
		for _, m := range members {
			rows = append(rows, map[string]interface{}{"key": key, "type": t, "member": m})
		}
	case "zset":
		members, err := c.ZRangeWithScores(ctx, key, 0, -1).Result()
		if err != nil {
			return nil, nil, "", err
		}
		for _, m := range members {
			rows = append(rows, map[string]interface{}{"key": key, "type": t, "member": m.Member, "score": m.Score})
		}
	default:
		rows = append(rows, map[string]interface{}{"key": key, "type": t})
	}
	if rows == nil {
		rows = []map[string]interface{}{}
	}
	// Client-side paging: Redis reads expand the whole key; slice what we show.
	if limit > 0 && offset < len(rows) {
		end := offset + limit
		if end > len(rows) {
			end = len(rows)
		}
		rows = rows[offset:end]
	} else if limit > 0 {
		rows = []map[string]interface{}{}
	}
	return rows, []string{"key", "type", "value", "field", "member", "score", "index"}, "key", nil
}

func (s *RedisSource) InsertItem(collection string, data map[string]interface{}) error {
	if err := validateKey(collection); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.client()
	if err != nil {
		return err
	}
	defer c.Close()
	key := collection
	switch str(data["type"]) {
	case "string":
		return c.Set(ctx, key, str(data["value"]), 0).Err()
	case "hash":
		return c.HSet(ctx, key, str(data["field"]), str(data["value"])).Err()
	case "list":
		return c.RPush(ctx, key, str(data["value"])).Err()
	case "set":
		return c.SAdd(ctx, key, str(data["member"])).Err()
	case "zset":
		score, _ := strconv.ParseFloat(str(data["score"]), 64)
		return c.ZAdd(ctx, key, redis.Z{Score: score, Member: str(data["member"])}).Err()
	default:
		return fmt.Errorf("unsupported redis type for insert: %s", str(data["type"]))
	}
}

func (s *RedisSource) UpdateItem(collection, pkCol, pkVal string, data map[string]interface{}) error {
	// Redis values are keyed by field/member/index — reuse insert path for the new value.
	return s.InsertItem(collection, data)
}

func (s *RedisSource) DeleteItem(collection, pkCol, pkVal string) error {
	if err := validateKey(collection); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.client()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Del(ctx, collection).Err()
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', -1, 64)
	}
	return fmt.Sprintf("%v", v)
}

func (s *RedisSource) CountItems(collection string) (int, error) {
	if err := validateKey(collection); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := s.client()
	if err != nil {
		return 0, err
	}
	defer c.Close()
	key := collection
	// go-redis Type returns "none" (not an error, not "") for missing keys —
	// the error must propagate, and "none" is the empty-collection case.
	t, terr := c.Type(ctx, key).Result()
	if terr != nil {
		return 0, terr
	}
	switch t {
	case "none":
		return 0, nil // key does not exist
	case "string":
		return 1, nil
	case "hash":
		n, err := c.HLen(ctx, key).Result()
		return int(n), err
	case "list":
		n, err := c.LLen(ctx, key).Result()
		return int(n), err
	case "set":
		n, err := c.SCard(ctx, key).Result()
		return int(n), err
	case "zset":
		n, err := c.ZCard(ctx, key).Result()
		return int(n), err
	}
	return 0, fmt.Errorf("unknown redis key type")
}
