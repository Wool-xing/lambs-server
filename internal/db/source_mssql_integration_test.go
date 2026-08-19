package db

import (
	"os"
	"testing"
)

// Real SQL Server verification — gated on LAMBS_MSSQL_DSN (e.g.
// mssql://sa:LambsTest2026!@127.0.0.1:14333/master). Skipped by default in
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
