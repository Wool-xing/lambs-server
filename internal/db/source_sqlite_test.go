package db

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sqliteBin resolves the sqlite3 CLI (env override first, then PATH) and
// skips the test when the binary is unavailable (CI runners without
// sqlite3, minimal dev boxes).
func sqliteBin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("LAMBS_SQLITE3_PATH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("sqlite3"); err == nil {
		return p
	}
	t.Skip("sqlite3 CLI not found — set LAMBS_SQLITE3_PATH to run")
	return ""
}

// newSQLiteSource creates a temp SQLite db with a users table and returns
// the source plus cleanup. Table covers: pk column, password/token columns
// (must be filtered from ReadItems), plain data column.
func newSQLiteSource(t *testing.T, bin string) *SQLiteSource {
	t.Helper()
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "test.db")
	s := &SQLiteSource{dsn: "sqlite:///" + filepath.ToSlash(dbFile)}
	mustSQLite(t, bin, dbFile,
		"CREATE TABLE users(id INTEGER PRIMARY KEY, name TEXT, password TEXT, token TEXT);")
	mustSQLite(t, bin, dbFile, "CREATE TABLE notes(id INTEGER PRIMARY KEY, body TEXT);")
	return s
}

func mustSQLite(t *testing.T, bin, dbFile, sql string) {
	t.Helper()
	cmd := exec.Command(bin, dbFile, sql)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sqlite %q: %v (%s)", sql, err, out)
	}
}

func TestSQLiteDBPath(t *testing.T) {
	s := &SQLiteSource{dsn: "sqlite:///C:/data/app.db?mode=ro"}
	if got := s.dbPath(); got != "C:/data/app.db" {
		t.Fatalf("dbPath with query = %q, want C:/data/app.db", got)
	}
	s2 := &SQLiteSource{dsn: "sqlite:///C:/data/app.db"}
	if got := s2.dbPath(); got != "C:/data/app.db" {
		t.Fatalf("dbPath plain = %q", got)
	}
}

func TestSQLiteListCollections(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	tables, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	want := map[string]bool{"notes": true, "users": true}
	if len(tables) != len(want) {
		t.Fatalf("tables = %v", tables)
	}
	for _, tb := range tables {
		if !want[tb] {
			t.Fatalf("unexpected table %q in %v", tb, tables)
		}
	}
}

func TestSQLiteListCollectionsError(t *testing.T) {
	sqliteBin(t) // gate: skip when CLI missing
	s := &SQLiteSource{dsn: "sqlite:///" + filepath.ToSlash(filepath.Join(t.TempDir(), "missing", "x.db"))}
	if _, err := s.ListCollections(); err == nil {
		t.Fatal("ListCollections on missing db should error")
	}
}

func TestSQLiteCountItems(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	if err := s.InsertItem("users", map[string]interface{}{"name": "a", "password": "p1", "token": "t1"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.InsertItem("users", map[string]interface{}{"name": "b"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	n, err := s.CountItems("users")
	if err != nil || n != 2 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if _, err := s.CountItems("users; DROP TABLE users"); err == nil {
		t.Fatal("injection table name should error")
	}
}

func TestSQLiteReadItemsFull(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	for i := 1; i <= 3; i++ {
		if err := s.InsertItem("users", map[string]interface{}{
			"name": fmt.Sprintf("user%d", i), "password": "secret", "token": "tok"}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	rows, cols, pk, err := s.ReadItems("users", 2, 1)
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
			t.Fatalf("sensitive column %q leaked into cols %v", c, cols)
		}
	}
	if len(cols) == 0 {
		t.Fatal("cols empty")
	}
	// default paging branch: limit <= 0 uses LIMIT 500
	if _, _, _, err := s.ReadItems("users", 0, 0); err != nil {
		t.Fatalf("ReadItems default paging: %v", err)
	}
}

// NOTE: ReadItems("") fallback (users/user/accounts/member probe) is
// unreachable in production — validateTable rejects the empty string
// before the branch. Left as dead code per surgical-changes policy.

func TestSQLiteInsertItemQuoteEscape(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	if err := s.InsertItem("notes", map[string]interface{}{"body": "it's O'Brien"}); err != nil {
		t.Fatalf("insert with quotes: %v", err)
	}
	rows, _, _, err := s.ReadItems("notes", 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after quoted insert: %v rows=%v", err, rows)
	}
	if rows[0]["body"] != "it's O'Brien" {
		t.Fatalf("body = %v, quote mangled", rows[0]["body"])
	}
}

func TestSQLiteInsertItemError(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	// table does not exist → sqlite CLI errors, message prefixed "sqlite:"
	if err := s.InsertItem("ghost_table", map[string]interface{}{"a": 1}); err == nil ||
		!strings.HasPrefix(err.Error(), "sqlite:") {
		t.Fatalf("insert missing table err = %v", err)
	}
}

func TestSQLiteUpdateItem(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	if err := s.InsertItem("users", map[string]interface{}{"name": "old", "password": "p"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateItem("users", "id", "1", map[string]interface{}{"name": "new", "id": "999"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, err := s.ReadItems("users", 10, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("read after update: %v rows=%v", err, rows)
	}
	if rows[0]["name"] != "new" {
		t.Fatalf("name = %v, want new", rows[0]["name"])
	}
	// pk column in data must be skipped, not used as SET assignment: id stays 1
	if fmt.Sprint(rows[0]["id"]) != "1" {
		t.Fatalf("id = %v, want 1 (pk column must be skipped in SET)", rows[0]["id"])
	}
}

func TestSQLiteDeleteItem(t *testing.T) {
	bin := sqliteBin(t)
	s := newSQLiteSource(t, bin)
	if err := s.InsertItem("users", map[string]interface{}{"name": "doomed"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.DeleteItem("users", "id", "1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	n, err := s.CountItems("users")
	if err != nil || n != 0 {
		t.Fatalf("count after delete = %d, %v", n, err)
	}
	if err := s.DeleteItem("users; DROP TABLE users", "id", "1"); err == nil {
		t.Fatal("injection table name should error")
	}
}
