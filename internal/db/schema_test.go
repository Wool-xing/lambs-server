package db

import (
	"os"
	"testing"
)

// TestEnsureCoreSchema — fresh database gets the four core tables; the
// function is idempotent (2026-08-29 open-source audit: fresh installs
// previously had no users/projects/notifications/audit_logs tables).
func TestEnsureCoreSchema(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	DB.Exec("DROP TABLE IF EXISTS audit_logs, notifications, projects, users CASCADE")

	EnsureCoreSchema()
	EnsureCoreSchema() // idempotent — second run must not fail

	var n int
	if err := DB.QueryRow("SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('users','projects','notifications','audit_logs')").Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 4 {
		t.Errorf("core tables = %d, want 4", n)
	}
}
