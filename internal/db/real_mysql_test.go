package db

import (
	"os"
	"testing"
)

// TestMySQLSourceRealCRUD — real MySQL: insert/read/update/delete/count
// via the adapter. Gated on LAMBS_TEST_MYSQL_DSN (docker lambs-mysql-test
// on 127.0.0.1:3307, db lambs_test).
func TestMySQLSourceRealCRUD(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_MYSQL_DSN not set — real MySQL verification skipped")
	}
	s := &MySQLSource{dsn: dsn}
	tdb, err := s.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	if _, err := tdb.Exec("CREATE TABLE IF NOT EXISTS lambs_probe (id INT PRIMARY KEY AUTO_INCREMENT, name VARCHAR(100))"); err != nil {
		t.Fatalf("create: %v", err)
	}
	defer tdb.Exec("DROP TABLE lambs_probe")
	tdb.Exec("DELETE FROM lambs_probe")

	if err := s.InsertItem("lambs_probe", map[string]interface{}{"name": "中文测试"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, cols, pk, err := s.ReadItems("lambs_probe", 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || rows[0]["name"] != "中文测试" || len(cols) == 0 || pk == "" {
		t.Fatalf("read = %v cols=%v pk=%q", rows, cols, pk)
	}
	if n, err := s.CountItems("lambs_probe"); err != nil || n != 1 {
		t.Fatalf("count = %d, %v", n, err)
	}
	if err := s.UpdateItem("lambs_probe", pk, rows[0][pk].(string), map[string]interface{}{"name": "更新后"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, _ = s.ReadItems("lambs_probe", 10, 0)
	if rows[0]["name"] != "更新后" {
		t.Fatalf("updated = %v", rows[0]["name"])
	}
	if err := s.DeleteItem("lambs_probe", pk, rows[0][pk].(string)); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n, _ := s.CountItems("lambs_probe"); n != 0 {
		t.Fatalf("count after delete = %d", n)
	}
}
