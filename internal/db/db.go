package db

import (
	"bytes"
	"database/sql"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

// DB is the global database connection pool.
var DB *sql.DB

var httpClient = &http.Client{Timeout: 10 * time.Second}

// Init opens the PostgreSQL connection. dsn accepts postgresql+asyncpg:// prefix.
func Init(dsn string) error {
	dsn = strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)
	var err error
	DB, err = sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	DB.SetMaxOpenConns(5)
	DB.SetMaxIdleConns(2)
	DB.SetConnMaxLifetime(5 * time.Minute)
	if err = DB.Ping(); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	log.Println("DB connected")
	return nil
}

// TestDSN checks whether a project's datasource is reachable.
func TestDSN(dsn string) map[string]interface{} { return testDSNInternal(dsn, "") }

// TestHealth checks health_url first, then falls back to DSN.
func TestHealth(dsn, healthURL string) map[string]interface{} {
	if healthURL != "" {
		return testDSNInternal(healthURL, "health_url")
	}
	return testDSNInternal(dsn, "dsn")
}

func testDSNInternal(dsn, source string) map[string]interface{} {
	if dsn == "" || dsn == "—" {
		return map[string]interface{}{"reachable": false, "error": "未配置数据源"}
	}
	if strings.HasPrefix(dsn, "http") {
		resp, err := httpClient.Get(dsn)
		if err != nil {
			return map[string]interface{}{"reachable": false, "error": err.Error()}
		}
		resp.Body.Close()
		return map[string]interface{}{"reachable": resp.StatusCode < 500, "latency_ms": 0, "db_type": "rest_api"}
	}
	dsn2 := strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)
	dsn2 = strings.Replace(dsn2, "sqlite:///", "", 1)
	if strings.Contains(dsn, "postgres") {
		tdb, err := sql.Open("postgres", dsn2+"?connect_timeout=5")
		if err == nil {
			err = tdb.Ping()
			tdb.Close()
			if err == nil {
				return map[string]interface{}{"reachable": true, "latency_ms": 0, "db_type": "postgresql"}
			}
		}
		return map[string]interface{}{"reachable": false, "error": err.Error()}
	}
	// SQLite: file existence check
	if strings.Contains(dsn, "sqlite") {
		path := dsn2
		if idx := strings.Index(path, "?"); idx >= 0 {
			path = path[:idx]
		}
		if _, se := os.Stat(path); se == nil {
			return map[string]interface{}{"reachable": true, "latency_ms": 0, "db_type": "sqlite"}
		} else {
			return map[string]interface{}{"reachable": false, "error": se.Error(), "db_type": "sqlite"}
		}
	}
	// MySQL: TCP health check (no driver needed)
	if strings.Contains(dsn, "mysql") {
		host := "127.0.0.1:3306"
		if u, err := url.Parse(dsn); err == nil && u.Host != "" {
			host = u.Host
		} else if idx := strings.Index(dsn, "@tcp("); idx > 0 {
			rest := dsn[idx+5:]
			if end := strings.Index(rest, ")"); end > 0 {
				host = rest[:end]
			}
		}
		conn, err := net.DialTimeout("tcp", host, 5*time.Second)
		if err != nil {
			return map[string]interface{}{"reachable": false, "error": err.Error(), "db_type": "mysql"}
		}
		conn.Close()
		return map[string]interface{}{"reachable": true, "latency_ms": 0, "db_type": "mysql"}
	}
	return map[string]interface{}{"reachable": false, "error": "不支持的数据源类型，当前支持: postgresql, sqlite, http, mysql"}
}

// SyncUserData fetches user-like rows from a project's datasource.
func SyncUserData(dsn string) []map[string]interface{} {
	if dsn == "" || dsn == "—" {
		return nil
	}
	dsn2 := strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)
	dsn2 = strings.Replace(dsn2, "sqlite:///", "", 1)
	if strings.Contains(dsn, "postgres") {
		tdb, err := sql.Open("postgres", dsn2+"?connect_timeout=5")
		if err != nil {
			return nil
		}
		defer tdb.Close()
		for _, table := range []string{"users", "user", "accounts", "member"} {
			rows, err := tdb.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 500", table))
			if err != nil {
				continue
			}
			defer rows.Close()
			cols, _ := rows.Columns()
			var result []map[string]interface{}
			for rows.Next() {
				vals := make([]interface{}, len(cols))
				ptrs := make([]interface{}, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				rows.Scan(ptrs...)
				row := make(map[string]interface{})
				for i, c := range cols {
					if !strings.Contains(strings.ToLower(c), "password") && !strings.Contains(strings.ToLower(c), "token") {
						row[c] = fmt.Sprintf("%v", vals[i])
					}
				}
				result = append(result, row)
			}
			return result
		}
	}
	if strings.Contains(dsn, "sqlite") {
		if idx := strings.Index(dsn2, "?"); idx >= 0 {
			dsn2 = dsn2[:idx]
		}
		for _, table := range []string{"users", "user", "accounts", "member"} {
			cmd := exec.Command("sqlite3", dsn2, fmt.Sprintf("SELECT COUNT(*) FROM %s;", table))
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				continue
			}
			count, err := strconv.Atoi(strings.TrimSpace(out.String()))
			if err != nil || count == 0 {
				continue
			}
			result := make([]map[string]interface{}, count)
			for i := 0; i < count; i++ {
				result[i] = map[string]interface{}{}
			}
			return result
		}
	}
	return nil
}

var safeTableName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

func validateTable(table string) error {
	if !safeTableName.MatchString(table) {
		return fmt.Errorf("invalid table name")
	}
	return nil
}

func isNumeric(s string) bool {
	if s == "" { return false }
	for _, c := range s {
		if c < '0' || c > '9' { return false }
	}
	return true
}

func sqliteVal(v string) string {
	if isNumeric(v) { return v }
	return "'" + strings.Replace(v, "'", "''", -1) + "'"
}

// GetTableList returns all user table names in the database.
