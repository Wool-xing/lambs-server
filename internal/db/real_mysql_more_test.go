package db

import (
	"os"
	"strings"
	"testing"
)

// mysqlTestDSN returns the real-MySQL gate; tests skip without it.
func mysqlTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LAMBS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_MYSQL_DSN not set — real MySQL verification skipped")
	}
	return dsn
}

// newMySQLProbe2 creates the probe table (pk + sensitive columns) with full
// DDL and registers cleanup. Distinct lambs_probe2 name so it never collides
// with other probe tables in the shared lambs_test database.
func newMySQLProbe2(t *testing.T, s *MySQLSource) {
	t.Helper()
	tdb, err := s.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if _, err := tdb.Exec("DROP TABLE IF EXISTS lambs_probe2"); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := tdb.Exec(`CREATE TABLE lambs_probe2 (
		id INT PRIMARY KEY AUTO_INCREMENT,
		name VARCHAR(100) NOT NULL,
		password VARCHAR(100),
		token VARCHAR(100)
	)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		tdb, err := s.open()
		if err != nil {
			return
		}
		defer tdb.Close()
		tdb.Exec("DROP TABLE IF EXISTS lambs_probe2")
	})
}

func TestMySQLSourceRealListCollections(t *testing.T) {
	s := &MySQLSource{dsn: mysqlTestDSN(t)}
	newMySQLProbe2(t, s)
	tables, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	found := false
	for _, tb := range tables {
		if tb == "lambs_probe2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("lambs_probe2 missing from %v", tables)
	}
}

func TestMySQLSourceRealPkColumn(t *testing.T) {
	s := &MySQLSource{dsn: mysqlTestDSN(t)}
	newMySQLProbe2(t, s)
	tdb, err := s.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if got := s.pkColumn(tdb, "lambs_probe2"); got != "id" {
		t.Fatalf("pkColumn = %q, want id", got)
	}
	if got := s.pkColumn(tdb, "no_such_table"); got != "" {
		t.Fatalf("pkColumn missing table = %q, want empty", got)
	}
}

func TestMySQLSourceRealReadItemsBranches(t *testing.T) {
	s := &MySQLSource{dsn: mysqlTestDSN(t)}
	newMySQLProbe2(t, s)
	for _, name := range []string{"a", "b", "c"} {
		if err := s.InsertItem("lambs_probe2", map[string]interface{}{"name": name, "password": "p", "token": "t"}); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	rows, cols, pk, err := s.ReadItems("lambs_probe2", 2, 1)
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
	for _, r := range rows {
		for k := range r {
			if strings.Contains(strings.ToLower(k), "password") || strings.Contains(strings.ToLower(k), "token") {
				t.Fatalf("sensitive key %q leaked into %v", k, r)
			}
		}
	}
	// default LIMIT 500 branch (limit 0)
	if _, _, _, err := s.ReadItems("lambs_probe2", 0, 0); err != nil {
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
	// injection name rejected by validateTable
	if _, _, _, err := s.ReadItems("bad; name", 10, 0); err == nil {
		t.Fatal("injection name should error")
	}
}
