package runtime

import (
	"os"
	"os/exec"
	"testing"

	"lambs-server-go/internal/db"
)

// TestAgentBacklogFiveFailuresThenRecovery — the alert backlog contract: a
// task failing five times lands exactly five alert notifications (one per
// failed execution, no cross-run dedupe by design), and the recovery run
// clears the failure state without adding a sixth notification. The
// one-failure variant is covered by TestExecuteTaskFailureNotifies; this
// pins the backlog and recovery shapes.
func TestAgentBacklogFiveFailuresThenRecovery(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not present")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`CREATE TABLE IF NOT EXISTS notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`DELETE FROM scheduled_tasks; DELETE FROM notifications`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-backlog','bk-proj','积压任务','*/5 * * * *','exit 1','app1')`)

	// Five consecutive failures → five alert notifications (no dedupe).
	for i := 0; i < 5; i++ {
		executeTask("t-backlog", "bk-proj", "积压任务", "exit 1", "app1")
	}
	var status string
	db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-backlog'").Scan(&status)
	if status != "failed" {
		t.Errorf("last_status = %q, want failed", status)
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='bk-proj' AND type='alert'").Scan(&n)
	if n != 5 {
		t.Errorf("alert notifications after 5 failures = %d, want 5 (one per run)", n)
	}

	// Recovery run: success clears the failure state, adds no notification.
	executeTask("t-backlog", "bk-proj", "积压任务", "echo recovered", "app1")
	db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-backlog'").Scan(&status)
	if status != "success" {
		t.Errorf("last_status after recovery = %q, want success", status)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='bk-proj' AND type='alert'").Scan(&n)
	if n != 5 {
		t.Errorf("alert notifications after recovery = %d, want still 5", n)
	}
}
