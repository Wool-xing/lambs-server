package db

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// typeKind classifies a project db_type string into a stats category.
// Empty result = no stats support for this type.
func typeKind(dbType string) string {
	lt := strings.ToLower(dbType)
	switch {
	case strings.Contains(lt, "mysql"), strings.Contains(lt, "postgres"), strings.Contains(lt, "sqlite"),
		strings.Contains(lt, "mssql"), strings.Contains(lt, "sql server"):
		return "sql"
	case strings.Contains(lt, "mongo"):
		return "mongo"
	case strings.Contains(lt, "redis"):
		return "redis"
	case strings.Contains(lt, "rest"):
		return "rest"
	}
	return ""
}

func dbKindName(dbType string) string {
	lt := strings.ToLower(dbType)
	switch {
	case strings.Contains(lt, "mysql"):
		return "MySQL"
	case strings.Contains(lt, "postgres"):
		return "PostgreSQL"
	case strings.Contains(lt, "sqlite"):
		return "SQLite"
	}
	return dbType
}

func toInt(v interface{}) int {
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func toFloat(v interface{}) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	}
	return 0
}

// redisInfoField extracts an int field like "uptime_in_seconds: 1234" from
// redis INFO output. Returns 0 when the field is absent or malformed.
func redisInfoInt(info, key string) int {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, key+":") {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, key+":"))); err == nil {
			return n
		}
	}
	return 0
}

// redisInfoField extracts a string field like "used_memory_human:1.2M".
func redisInfoStr(info, key string) string {
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, key+":") {
			return strings.TrimSpace(strings.TrimPrefix(line, key+":"))
		}
	}
	return ""
}

// BuildStatCards maps collected stats into the feature cards the frontend
// renders. Returns nil for db_types without a card mapping — callers keep
// whatever cards are already stored.
func BuildStatCards(dbType string, stats map[string]interface{}) []map[string]interface{} {
	switch typeKind(dbType) {
	case "sql":
		return []map[string]interface{}{
			{"label": "表数量", "value": stats["tables"]},
			{"label": "数据行数", "value": stats["rows"]},
			{"label": "数据库", "value": dbKindName(dbType)},
		}
	case "mongo":
		return []map[string]interface{}{
			{"label": "集合数", "value": stats["collections"]},
			{"label": "文档数", "value": stats["documents"]},
			{"label": "存储占用", "value": fmt.Sprintf("%.1f MB", toFloat(stats["storage_mb"]))},
		}
	case "redis":
		secs := toInt(stats["uptime_sec"])
		return []map[string]interface{}{
			{"label": "键数量", "value": stats["keys"]},
			{"label": "内存占用", "value": stats["memory"]},
			{"label": "运行时长", "value": fmt.Sprintf("%d小时%d分", secs/3600, secs%3600/60)},
		}
	case "rest":
		status := "正常"
		if fmt.Sprint(stats["status"]) != "healthy" {
			status = "异常"
		}
		return []map[string]interface{}{
			{"label": "健康状态", "value": status},
			{"label": "响应延迟", "value": fmt.Sprintf("%d ms", toInt(stats["latency_ms"]))},
		}
	}
	return nil
}

// CollectStats gathers cheap aggregate stats for a project, dispatched by
// db_type. All queries are server-side aggregates (no full-table scans):
// SQL uses information_schema/pg_class estimates, Mongo dbStats, Redis
// DBSIZE/INFO, REST a single health probe.
func CollectStats(dbType, dsn string) (map[string]interface{}, error) {
	src, err := NewDataSource(dsn)
	if err != nil {
		return nil, err
	}
	switch typeKind(dbType) {
	case "sql":
		cols, err := src.ListCollections()
		if err != nil {
			return nil, err
		}
		rows := 0
		switch s := src.(type) {
		case *MySQLSource:
			rows, err = mysqlSumRows(s)
		case *PostgresSource:
			rows, err = pgSumRows(s)
		default:
			// SQLite and anything else: per-table COUNT(*) — local file, fast.
			for _, c := range cols {
				if n, e := src.CountItems(c); e == nil {
					rows += n
				}
			}
		}
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"tables": len(cols), "rows": rows}, nil
	case "mongo":
		ms, ok := src.(*MongoSource)
		if !ok {
			return nil, fmt.Errorf("数据源与类型不匹配: %s", dbType)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client, mdb, err := ms.connect(ctx)
		if err != nil {
			return nil, err
		}
		defer client.Disconnect(ctx)
		var res bson.M
		if err := mdb.RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&res); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"collections": res["collections"],
			"documents":   res["objects"],
			"storage_mb":  toFloat(res["storageSize"]) / 1048576,
		}, nil
	case "redis":
		rs, ok := src.(*RedisSource)
		if !ok {
			return nil, fmt.Errorf("数据源与类型不匹配: %s", dbType)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := rs.client()
		if err != nil {
			return nil, err
		}
		defer c.Close() // throwaway struct — no pooling benefit, must not leak conns
		keys, err := c.DBSize(ctx).Result()
		if err != nil {
			return nil, err
		}
		info, err := c.Info(ctx, "memory", "server").Result()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"keys":       int(keys),
			"memory":     redisInfoStr(info, "used_memory_human"),
			"uptime_sec": redisInfoInt(info, "uptime_in_seconds"),
		}, nil
	case "rest":
		rs, ok := src.(*RESTSource)
		if !ok {
			return nil, fmt.Errorf("数据源与类型不匹配: %s", dbType)
		}
		start := time.Now()
		_, status, err := rs.do("GET", rs.base()+"/", nil)
		latency := int(time.Since(start).Milliseconds())
		st := "healthy"
		if err != nil || status >= 400 {
			st = "unhealthy"
		}
		return map[string]interface{}{"status": st, "latency_ms": latency}, nil
	}
	return nil, fmt.Errorf("暂不支持该类型的统计: %s", dbType)
}

func mysqlSumRows(s *MySQLSource) (int, error) {
	tdb, err := s.open()
	if err != nil {
		return 0, err
	}
	defer tdb.Close()
	var n int
	err = tdb.QueryRow("SELECT COALESCE(SUM(TABLE_ROWS),0) FROM information_schema.tables WHERE table_schema = DATABASE()").Scan(&n)
	return n, err
}

func pgSumRows(s *PostgresSource) (int, error) {
	tdb, err := sql.Open("postgres", s.normDSN())
	if err != nil {
		return 0, err
	}
	defer tdb.Close()
	var n int
	err = tdb.QueryRow("SELECT COALESCE(SUM(reltuples)::bigint,0) FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.relkind='r' AND n.nspname NOT IN ('pg_catalog','information_schema')").Scan(&n)
	return n, err
}
