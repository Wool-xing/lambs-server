package runtime

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// TestExecuteTaskFailureNotifies — the write-side cron contract: a failed
// task persists last_status=failed AND lands exactly one alert notification
// with the right project (QA round 3 idea 1, runtime half). Success runs
// leave no notification. Real postgres, gated on LAMBS_TEST_PG_DSN.
func TestExecuteTaskFailureNotifies(t *testing.T) {
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
	mustExec(`DELETE FROM scheduled_tasks; DELETE FROM notifications;`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-fail','proj-x','会失败的任务','*/5 * * * *','exit 1','app1'), ('t-ok','proj-x','会成功的任务','*/5 * * * *','echo hi','app1')`)

	// Failure path.
	executeTask("t-fail", "proj-x", "会失败的任务", "exit 1", "app1")
	var status string
	db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-fail'").Scan(&status)
	if status != "failed" {
		t.Errorf("last_status = %q, want failed", status)
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='proj-x' AND type='alert'").Scan(&n)
	if n != 1 {
		t.Errorf("alert notifications = %d, want 1", n)
	}
	var title string
	db.DB.QueryRow("SELECT title FROM notifications WHERE project_id='proj-x'").Scan(&title)
	if title != "计划任务失败" {
		t.Errorf("title = %q", title)
	}

	// Success path adds no notification.
	executeTask("t-ok", "proj-x", "会成功的任务", "echo hi", "app1")
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='proj-x'").Scan(&n)
	if n != 1 {
		t.Errorf("notifications after success = %d, want still 1", n)
	}
}

// TestStartTaskRunAsync — a stored task runs asynchronously and lands
// last_status=success (real bash echo). Unknown id errors.
func TestStartTaskRunAsync(t *testing.T) {
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
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`DELETE FROM scheduled_tasks WHERE id='t-async'`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-async','proj-x','异步任务','*/5 * * * *','echo async-ok','app1')`)

	if err := StartTaskRun("t-async"); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := StartTaskRun("nope"); err == nil {
		t.Error("unknown task id should error")
	}
	// Async: poll for the status to land.
	deadline := time.Now().Add(10 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-async'").Scan(&status)
		if status == "success" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("last_status = %q, want success", status)
}

// TestStartTaskRunConcurrentGuard — a task already in flight must not start
// a second concurrent execution of the same command (manual double-click +
// cron firing in the same minute). QA full-round calibration candidate 4.
func TestStartTaskRunConcurrentGuard(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`DELETE FROM scheduled_tasks WHERE id='t-guard'`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-guard','proj-x','并发守卫','*/5 * * * *','sleep 3 && echo guard-ok','app1')`)

	if err := StartTaskRun("t-guard"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	if err := StartTaskRun("t-guard"); err == nil {
		t.Error("second start while in flight should be rejected")
	}
	// The guard must release after completion so the task can run again.
	deadline := time.Now().Add(15 * time.Second)
	var status string
	for time.Now().Before(deadline) {
		db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-guard'").Scan(&status)
		if status == "success" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != "success" {
		t.Fatalf("last_status = %q, want success", status)
	}
	if err := StartTaskRun("t-guard"); err != nil {
		t.Errorf("restart after completion should succeed, got: %v", err)
	}
}
