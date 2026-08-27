package db

import (
	"database/sql"
	"os"
	"strings"
	"testing"
)

// postgresTestDSN returns the real-PG gate; tests skip without it.
func postgresTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	return dsn
}

// postgresOpen mirrors the production inline sql.Open call.
func postgresOpen(s *PostgresSource) (*sql.DB, error) {
	return sql.Open("postgres", s.normDSN())
}

// newPostgresProbe creates the probe table with full DDL (pk, sensitive
// columns) and registers cleanup.
func newPostgresProbe(t *testing.T, s *PostgresSource) {
	t.Helper()
	tdb, err := postgresOpen(s)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if _, err := tdb.Exec("DROP TABLE IF EXISTS lambs_probe"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := tdb.Exec(`CREATE TABLE lambs_probe (
		id SERIAL PRIMARY KEY,
		name TEXT NOT NULL,
		password TEXT,
		token TEXT
	)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		tdb, err := postgresOpen(s)
		if err != nil {
			return
		}
		defer tdb.Close()
		tdb.Exec("DROP TABLE IF EXISTS lambs_probe")
	})
}

func TestPostgresNormDSN(t *testing.T) {
	s := &PostgresSource{dsn: "postgresql+asyncpg://u:p@h/db"}
	if got := s.normDSN(); got != "postgres://u:p@h/db?connect_timeout=5" {
		t.Fatalf("normDSN asyncpg = %q", got)
	}
	s2 := &PostgresSource{dsn: "postgres://u@h/db?sslmode=disable"}
	if got := s2.normDSN(); got != "postgres://u@h/db?sslmode=disable&connect_timeout=5" {
		t.Fatalf("normDSN with query = %q", got)
	}
}

func TestPostgresListCollections(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	tables, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	found := false
	for _, tb := range tables {
		if tb == "lambs_probe" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lambs_probe missing from %v", tables)
	}
	// unreachable-host DSN: port 1 refuses fast, no 5s timeout wait
	bad := &PostgresSource{dsn: "postgres://u:p@127.0.0.1:1/db"}
	if _, err := bad.ListCollections(); err == nil {
		t.Fatal("ListCollections on closed port should error")
	}
}

func TestPostgresReadItems(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	for _, name := range []string{"a", "b", "c"} {
		if err := s.InsertItem("lambs_probe", map[string]interface{}{"name": name, "password": "p", "token": "t"}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	rows, cols, pk, err := s.ReadItems("lambs_probe", 2, 1)
	if err != nil {
		t.Fatalf("ReadItems: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("paged rows = %d, want 2", len(rows))
	}
	if pk != "id" {
		t.Fatalf("pk = %q, want id", pk)
	}
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c), "password") || strings.Contains(strings.ToLower(c), "token") {
			t.Fatalf("sensitive col %q leaked into %v", c, cols)
		}
	}
	if rows[0]["name"] == "" {
		t.Fatalf("name not decoded: %v", rows[0])
	}
	// LIMIT 500 default-paging branch
	if _, _, _, err := s.ReadItems("lambs_probe", 0, 0); err != nil {
		t.Fatalf("ReadItems default paging: %v", err)
	}
	// missing table → continue loop → empty result
	empty, _, _, err := s.ReadItems("no_such_table", 10, 0)
	if err != nil {
		t.Fatalf("missing table should return empty, got %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("missing-table rows = %#v", empty)
	}
	if _, _, _, err := s.ReadItems("bad; name", 10, 0); err == nil {
		t.Fatal("injection name should error")
	}
}

func TestPostgresCountItems(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	if err := s.InsertItem("lambs_probe", map[string]interface{}{"name": "x"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := s.CountItems("lambs_probe")
	if err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := s.CountItems("bad; name"); err == nil {
		t.Fatal("injection name should error")
	}
}

func TestPostgresPkColumn(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	tdb, err := postgresOpen(s)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if got := s.pkColumn(tdb, "lambs_probe"); got != "id" {
		t.Fatalf("pkColumn = %q, want id", got)
	}
	if got := s.pkColumn(tdb, "no_such_table"); got != "" {
		t.Fatalf("pkColumn missing table = %q, want empty", got)
	}
}

func TestPostgresCRUD(t *testing.T) {
	dsn := postgresTestDSN(t)
	s := &PostgresSource{dsn: dsn}
	newPostgresProbe(t, s)
	if err := s.InsertItem("lambs_probe", map[string]interface{}{"name": "old"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateItem("lambs_probe", "id", "1", map[string]interface{}{"name": "new", "id": "999"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, err := s.ReadItems("lambs_probe", 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after update: %v rows=%v", err, rows)
	}
	if rows[0]["name"] != "new" {
		t.Fatalf("name = %v, want new", rows[0]["name"])
	}
	if err := s.DeleteItem("lambs_probe", "id", "1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	n, err := s.CountItems("lambs_probe")
	if err != nil || n != 0 {
		t.Fatalf("count after delete = %d, %v", n, err)
	}
	if err := s.InsertItem("bad; name", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("insert injection name should error")
	}
	if err := s.UpdateItem("bad; name", "id", "1", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("update injection name should error")
	}
	if err := s.DeleteItem("bad; name", "id", "1"); err == nil {
		t.Fatal("delete injection name should error")
	}
}
