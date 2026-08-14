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
	keys, _, err := c.Scan(ctx, 0, "*", 200).Result()
	if err != nil {
		return []string{}, err
	}
	if keys == nil {
		keys = []string{}
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

func (s *RedisSource) ReadItems(collection string) ([]map[string]interface{}, []string, string, error) {
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
