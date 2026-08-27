package main

import (
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestCleanupOldData — real PostgreSQL: the hourly retention pass deletes
// notifications older than 30 days and audit logs older than 90 days while
// keeping fresh rows and rows just under the boundary (strict < comparison:
// a row exactly 30d old is already older than NOW()-30d by the time the
// cleanup runs, so the deterministic edge is 30d minus 1 minute).
// Gated on LAMBS_TEST_PG_DSN.
func TestCleanupOldData(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`DROP TABLE IF EXISTS notifications`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMPTZ NOT NULL DEFAULT now())`)
	mustExec(`DROP TABLE IF EXISTS audit_logs`)
	mustExec(`CREATE TABLE audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`DELETE FROM notifications; DELETE FROM audit_logs;`)
	mustExec(`INSERT INTO notifications (id, project_id, created_at) VALUES
		('n-old', 'p1', NOW() - INTERVAL '31 days'),
		('n-boundary', 'p1', NOW() - INTERVAL '30 days' + INTERVAL '1 minute'),
		('n-fresh', 'p1', NOW() - INTERVAL '1 hour')`)
	mustExec(`INSERT INTO audit_logs (user_id, action, created_at) VALUES
		('u1', 'old', NOW() - INTERVAL '91 days'),
		('u1', 'boundary', NOW() - INTERVAL '90 days' + INTERVAL '1 minute'),
		('u1', 'fresh', NOW() - INTERVAL '1 hour')`)

	cleanupOldData()

	var notifCount, auditCount int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications").Scan(&notifCount)
	if notifCount != 2 {
		t.Errorf("notifications left = %d, want 2 (boundary + fresh survive)", notifCount)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&auditCount)
	if auditCount != 2 {
		t.Errorf("audit_logs left = %d, want 2 (boundary + fresh survive)", auditCount)
	}
	// The surviving rows are the right ones.
	var n string
	db.DB.QueryRow("SELECT id FROM notifications ORDER BY created_at LIMIT 1").Scan(&n)
	if n != "n-boundary" {
		t.Errorf("oldest surviving notification = %q, want n-boundary", n)
	}
	var act string
	db.DB.QueryRow("SELECT action FROM audit_logs ORDER BY created_at LIMIT 1").Scan(&act)
	if act != "boundary" {
		t.Errorf("oldest surviving audit row = %q, want boundary", act)
	}

	// Idempotent: a second pass changes nothing.
	cleanupOldData()
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications").Scan(&notifCount)
	if notifCount != 2 {
		t.Errorf("notifications after second pass = %d, want 2", notifCount)
	}
}
