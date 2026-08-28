package db

import (
	"os"
	"testing"
)

// Real SQL Server verification — gated on LAMBS_MSSQL_DSN (e.g.
// mssql://sa:CHANGE_ME@127.0.0.1:14333/master). Skipped by default in
// CI; run manually against the docker mssql-lambs-test container.
func TestMSSQLIntegrationCRUD(t *testing.T) {
	dsn := os.Getenv("LAMBS_MSSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_MSSQL_DSN not set — real SQL Server verification skipped")
	}
	src := &MSSQLSource{dsn: dsn}

	// Fixture table with an identity-free explicit pk + Chinese text.
	tdb, err := src.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if _, err := tdb.Exec("IF OBJECT_ID('dbo.lambs_probe','U') IS NULL CREATE TABLE dbo.lambs_probe (id INT PRIMARY KEY, name NVARCHAR(50))"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	defer tdb.Exec("DROP TABLE dbo.lambs_probe")

	// Insert (with Chinese) via adapter.
	if err := src.InsertItem("lambs_probe", map[string]interface{}{"id": 1, "name": "中文测试"}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Read back: rows, cols, pk.
	rows, cols, pk, err := src.ReadItems("lambs_probe", 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("read rows = %d, want 1", len(rows))
	}
	if got := rows[0]["name"]; got != "中文测试" {
		t.Errorf("name = %v, want 中文测试", got)
	}
	if pk != "id" {
		t.Errorf("pk = %q, want id", pk)
	}
	foundID := false
	for _, c := range cols {
		if c == "id" {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("cols missing id: %v", cols)
	}

	// Count.
	if n, err := src.CountItems("lambs_probe"); err != nil || n != 1 {
		t.Errorf("count = %d, %v; want 1", n, err)
	}

	// Update.
	if err := src.UpdateItem("lambs_probe", "id", "1", map[string]interface{}{"name": "更新后"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, _ = src.ReadItems("lambs_probe", 10, 0)
	if rows[0]["name"] != "更新后" {
		t.Errorf("after update name = %v, want 更新后", rows[0]["name"])
	}

	// Delete.
	if err := src.DeleteItem("lambs_probe", "id", "1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := src.CountItems("lambs_probe"); n != 0 {
		t.Errorf("after delete count = %d, want 0", n)
	}

	// ListCollections includes the probe table.
	tables, err := src.ListCollections()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, tb := range tables {
		if tb == "lambs_probe" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListCollections missing lambs_probe: %v", tables)
	}
}

// mssqlTestDSN returns the real-MSSQL gate; tests skip without it.
func mssqlTestDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LAMBS_MSSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_MSSQL_DSN not set — real SQL Server verification skipped")
	}
	return dsn
}

// newMSSQLProbe2 creates a probe table with pk + sensitive columns and
// registers cleanup. Distinct lambs_probe2 name (no identity/auto-increment —
// ids are explicit) so it never collides with the lambs_probe fixture.
func newMSSQLProbe2(t *testing.T, src *MSSQLSource) {
	t.Helper()
	tdb, err := src.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if _, err := tdb.Exec("IF OBJECT_ID('dbo.lambs_probe2','U') IS NULL CREATE TABLE dbo.lambs_probe2 (id INT PRIMARY KEY, name NVARCHAR(50), password NVARCHAR(50), token NVARCHAR(50))"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		tdb, err := src.open()
		if err != nil {
			return
		}
		defer tdb.Close()
		tdb.Exec("DROP TABLE dbo.lambs_probe2")
	})
}

func TestMSSQLIntegrationPkColumn(t *testing.T) {
	src := &MSSQLSource{dsn: mssqlTestDSN(t)}
	newMSSQLProbe2(t, src)
	tdb, err := src.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if got := src.pkColumn(tdb, "lambs_probe2"); got != "id" {
		t.Fatalf("pkColumn = %q, want id", got)
	}
	if got := src.pkColumn(tdb, "no_such_table"); got != "" {
		t.Fatalf("pkColumn missing table = %q, want empty", got)
	}
}

func TestMSSQLIntegrationReadItemsBranches(t *testing.T) {
	src := &MSSQLSource{dsn: mssqlTestDSN(t)}
	newMSSQLProbe2(t, src)
	for i, name := range []string{"a", "b", "c"} {
		if err := src.InsertItem("lambs_probe2", map[string]interface{}{"id": i + 1, "name": name, "password": "p", "token": "t"}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	rows, cols, pk, err := src.ReadItems("lambs_probe2", 2, 1)
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
		if IsSensitiveKey(c) {
			t.Fatalf("sensitive col %q leaked into %v", c, cols)
		}
	}
	for _, r := range rows {
		for k := range r {
			if IsSensitiveKey(k) {
				t.Fatalf("sensitive key %q leaked into %v", k, r)
			}
		}
	}
	// default 500-row branch (limit 0)
	if _, _, _, err := src.ReadItems("lambs_probe2", 0, 0); err != nil {
		t.Fatalf("ReadItems default paging: %v", err)
	}
	// missing table → continue loop → empty result
	empty, _, _, err := src.ReadItems("no_such_table", 10, 0)
	if err != nil {
		t.Fatalf("missing table should return empty, got %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("missing-table rows = %#v", empty)
	}
	// injection names rejected by validateTable on every entry point
	if _, _, _, err := src.ReadItems("bad; name", 10, 0); err == nil {
		t.Fatal("read injection name should error")
	}
	if _, err := src.CountItems("bad; name"); err == nil {
		t.Fatal("count injection name should error")
	}
	if err := src.InsertItem("bad; name", map[string]interface{}{"id": 9}); err == nil {
		t.Fatal("insert injection name should error")
	}
	if err := src.UpdateItem("bad; name", "id", "1", map[string]interface{}{"name": "x"}); err == nil {
		t.Fatal("update injection name should error")
	}
	if err := src.DeleteItem("bad; name", "id", "1"); err == nil {
		t.Fatal("delete injection name should error")
	}
}
