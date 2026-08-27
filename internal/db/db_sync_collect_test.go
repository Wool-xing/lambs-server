package db

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// --- SyncUserData / SyncUserCount branch matrix ---

func TestSyncUserDataEmpty(t *testing.T) {
	if got := SyncUserData(""); got != nil {
		t.Fatalf("empty dsn = %v, want nil", got)
	}
	if got := SyncUserData("—"); got != nil {
		t.Fatalf("dash dsn = %v, want nil", got)
	}
	if got := SyncUserCount(""); got != 0 {
		t.Fatalf("empty dsn count = %d", got)
	}
	if got := SyncUserCount("—"); got != 0 {
		t.Fatalf("dash dsn count = %d", got)
	}
}

func TestSyncUserGuardReject(t *testing.T) {
	// SSRF guard blocks RFC1918 private targets before any dial
	// (public IPs are allowed by design and would be dialed).
	if got := SyncUserData("postgres://u:p@10.1.2.3/db"); got != nil {
		t.Fatal("guard should reject private host")
	}
	if got := SyncUserCount("postgres://u:p@10.1.2.3/db"); got != 0 {
		t.Fatal("guard should reject private host count")
	}
}

func TestSyncUserOtherScheme(t *testing.T) {
	// redis DSN hits no sync branch → nil / 0 (no dial attempted)
	if got := SyncUserData("redis://127.0.0.1:1/0"); got != nil {
		t.Fatalf("redis dsn = %v, want nil", got)
	}
	if got := SyncUserCount("redis://127.0.0.1:1/0"); got != 0 {
		t.Fatalf("redis dsn count = %d", got)
	}
}

func TestSyncUserDataPostgres(t *testing.T) {
	dsn := postgresTestDSN(t)
	// NOTE: this test DROPs and CREATEs a table literally named "users" —
	// LAMBS_TEST_PG_DSN must point at a disposable test database only.
	s := &PostgresSource{dsn: dsn}
	tdb, err := postgresOpen(s)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	tdb.Exec("DROP TABLE IF EXISTS users")
	if _, err := tdb.Exec(`CREATE TABLE users (id SERIAL PRIMARY KEY, name TEXT, password TEXT, token TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer tdb.Exec("DROP TABLE IF EXISTS users")
	for _, name := range []string{"u1", "u2", "u3"} {
		if _, err := tdb.Exec(`INSERT INTO users (name, password, token) VALUES ($1, 'p', 't')`, name); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows := SyncUserData(dsn)
	if len(rows) != 3 {
		t.Fatalf("SyncUserData rows = %d, want 3", len(rows))
	}
	if _, ok := rows[0]["password"]; ok {
		t.Fatal("password leaked into sync rows")
	}
	if _, ok := rows[0]["token"]; ok {
		t.Fatal("token leaked into sync rows")
	}
	if got := SyncUserCount(dsn); got != 3 {
		t.Fatalf("SyncUserCount = %d, want 3", got)
	}
}

func TestSyncUserDataSQLite(t *testing.T) {
	bin := sqliteBin(t)
	dbFile := filepath.Join(t.TempDir(), "sync.db")
	mustSQLite(t, bin, dbFile, `CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT, password TEXT, token TEXT);`)
	mustSQLite(t, bin, dbFile, `INSERT INTO users(name, password, token) VALUES ('a','p','t'), ('b','p','t');`)
	dsn := "sqlite:///" + filepath.ToSlash(dbFile) + "?mode=ro"
	rows := SyncUserData(dsn)
	if len(rows) != 2 {
		t.Fatalf("SyncUserData sqlite rows = %v", rows)
	}
	if _, ok := rows[0]["password"]; ok {
		t.Fatal("password leaked into sync rows")
	}
	if got := SyncUserCount(dsn); got != 2 {
		t.Fatalf("SyncUserCount sqlite = %d, want 2", got)
	}
}

func TestSyncUserDataMySQL(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_MYSQL_DSN not set — real MySQL verification skipped")
	}
	// NOTE: DROPs and CREATEs a table literally named "users" —
	// LAMBS_TEST_MYSQL_DSN must point at a disposable test database only.
	s := &MySQLSource{dsn: dsn}
	tdb, err := s.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	tdb.Exec("DROP TABLE IF EXISTS users")
	if _, err := tdb.Exec(`CREATE TABLE users (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(100), password VARCHAR(100), token VARCHAR(100))`); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer tdb.Exec("DROP TABLE IF EXISTS users")
	tdb.Exec(`INSERT INTO users (name, password, token) VALUES ('a','p','t'), ('b','p','t')`)
	rows := SyncUserData(dsn)
	if len(rows) != 2 {
		t.Fatalf("SyncUserData mysql rows = %v", rows)
	}
	if _, ok := rows[0]["password"]; ok {
		t.Fatal("password leaked into sync rows")
	}
	if got := SyncUserCount(dsn); got != 2 {
		t.Fatalf("SyncUserCount mysql = %d, want 2", got)
	}
}

// --- CollectStats dispatch ---

func TestCollectStatsSQLPostgres(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	stats, err := CollectStats("postgres", dsn)
	if err != nil {
		t.Fatalf("CollectStats pg: %v", err)
	}
	if _, ok := stats["tables"]; !ok {
		t.Fatalf("missing tables key: %v", stats)
	}
}

func TestCollectStatsSQLSQLite(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	if err := s.InsertItem("notes", map[string]interface{}{"body": "x"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	stats, err := CollectStats("sqlite", s.dsn)
	if err != nil {
		t.Fatalf("CollectStats sqlite: %v", err)
	}
	if stats["tables"].(int) != 2 || stats["rows"].(int) != 1 {
		t.Fatalf("sqlite stats = %v", stats)
	}
}

func TestCollectStatsSQLMySQL(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_MYSQL_DSN not set — real MySQL verification skipped")
	}
	stats, err := CollectStats("mysql", dsn)
	if err != nil {
		t.Fatalf("CollectStats mysql: %v", err)
	}
	if _, ok := stats["rows"]; !ok {
		t.Fatalf("missing rows key: %v", stats)
	}
}

func TestCollectStatsMongo(t *testing.T) {
	addr := os.Getenv("LAMBS_TEST_MONGO_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_MONGO_ADDR not set — real MongoDB verification skipped")
	}
	stats, err := CollectStats("mongodb", "mongodb://"+addr+"/lambs_probe_db")
	if err != nil {
		t.Fatalf("CollectStats mongo: %v", err)
	}
	if _, ok := stats["collections"]; !ok {
		t.Fatalf("missing collections key: %v", stats)
	}
}

func TestCollectStatsRedis(t *testing.T) {
	addr := os.Getenv("LAMBS_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("LAMBS_TEST_REDIS_ADDR not set — real Redis verification skipped")
	}
	stats, err := CollectStats("redis", "redis://"+addr)
	if err != nil {
		t.Fatalf("CollectStats redis: %v", err)
	}
	if _, ok := stats["uptime_sec"]; !ok {
		t.Fatalf("missing uptime_sec key: %v", stats)
	}
}

func TestCollectStatsREST(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer healthy.Close()
	stats, err := CollectStats("rest", healthy.URL)
	if err != nil {
		t.Fatalf("CollectStats rest: %v", err)
	}
	if stats["status"] != "healthy" {
		t.Fatalf("healthy status = %v", stats["status"])
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer broken.Close()
	stats, err = CollectStats("rest", broken.URL)
	if err != nil {
		t.Fatalf("CollectStats rest 500: %v", err)
	}
	if stats["status"] != "unhealthy" {
		t.Fatalf("500 status = %v, want unhealthy", stats["status"])
	}
}

func TestCollectStatsMismatchAndUnknown(t *testing.T) {
	// Only NewDataSource + typeKind run before the error — the DSN is never
	// dialed, so a syntactically valid placeholder suffices (no env gate).
	dsn := "postgres://u:p@127.0.0.1:1/db"
	if _, err := CollectStats("mongodb", dsn); err == nil {
		t.Fatal("type/dsn mismatch should error")
	}
	if _, err := CollectStats("oracle", dsn); err == nil {
		t.Fatal("unknown db type should error")
	}
}

func TestMysqlSumRowsReal(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_MYSQL_DSN not set — real MySQL verification skipped")
	}
	s := &MySQLSource{dsn: dsn}
	if _, err := mysqlSumRows(s); err != nil {
		t.Fatalf("mysqlSumRows: %v", err)
	}
}
