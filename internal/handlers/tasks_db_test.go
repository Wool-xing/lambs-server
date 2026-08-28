package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// TestTasksCRUD — real PostgreSQL: create validates cron, list returns the
// row, run-now 404s for a missing id (never executing a real command in
// tests). Gated on LAMBS_TEST_PG_DSN.
func TestTasksCRUD(t *testing.T) {
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
	mustExec(`DELETE FROM scheduled_tasks`)

	saHdr := func(r *http.Request) {
		r.Header.Set("X-User-ID", "admin-uid")
		r.Header.Set("X-Role", "super_admin")
	}

	// Valid create
	body, _ := json.Marshal(map[string]interface{}{
		"name": "验收-task", "cron": "*/5 * * * *", "command": "echo hi", "host": "app1", "enabled": true,
	})
	cr := httptest.NewRequest("POST", "/api/projects/p1/tasks", bytes.NewReader(body))
	cr.Header.Set("Content-Type", "application/json")
	saHdr(cr)
	cw := httptest.NewRecorder()
	cr.SetPathValue("id", "p1")
	CreateTask(cw, cr)
	if cw.Code != 200 && cw.Code != 201 {
		t.Fatalf("create = %d (body %s)", cw.Code, cw.Body.String())
	}

	// Invalid cron rejected without a row
	bad, _ := json.Marshal(map[string]interface{}{
		"name": "bad", "cron": "not a cron", "command": "echo", "host": "app1", "enabled": true,
	})
	br := httptest.NewRequest("POST", "/api/projects/p1/tasks", bytes.NewReader(bad))
	br.Header.Set("Content-Type", "application/json")
	saHdr(br)
	bw := httptest.NewRecorder()
	br.SetPathValue("id", "p1")
	CreateTask(bw, br)
	if bw.Code != 400 {
		t.Errorf("invalid cron = %d, want 400 (body %s)", bw.Code, bw.Body.String())
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM scheduled_tasks WHERE project_id='p1'").Scan(&n)
	if n != 1 {
		t.Errorf("rows = %d, want 1 (invalid create persisted?)", n)
	}

	// List returns the row
	lr := httptest.NewRequest("GET", "/api/projects/p1/tasks", nil)
	saHdr(lr)
	lw := httptest.NewRecorder()
	lr.SetPathValue("id", "p1")
	ListTasks(lw, lr)
	if lw.Code != 200 {
		t.Fatalf("list = %d (body %s)", lw.Code, lw.Body.String())
	}
	var list struct {
		Success bool `json:"success"`
		Data    struct {
			Tasks []map[string]interface{} `json:"tasks"`
		} `json:"data"`
	}
	json.Unmarshal(lw.Body.Bytes(), &list)
	if len(list.Data.Tasks) != 1 || list.Data.Tasks[0]["name"] != "验收-task" {
		t.Errorf("tasks = %v, want 1 row named 验收-task", list.Data.Tasks)
	}

	// RunTaskNow on a missing id → 404, nothing executed
	rr := httptest.NewRequest("POST", "/api/tasks/nonexistent/run", nil)
	rr.SetPathValue("id", "nonexistent")
	saHdr(rr)
	rw := httptest.NewRecorder()
	RunTaskNow(rw, rr)
	if rw.Code != 404 {
		t.Errorf("run-now missing = %d, want 404 (body %s)", rw.Code, rw.Body.String())
	}
}

// TestRunTaskNowHappyPath — real PostgreSQL: a stored task with a real
// executable command runs asynchronously; the handler answers 200 with
// {"started": id} and last_status/last_log land (bash -c on the Lambs host,
// same contract executeTask uses).
func TestRunTaskNowHappyPath(t *testing.T) {
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
	mustExec(`DELETE FROM scheduled_tasks WHERE id='t-run'`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-run','p1','立即运行','*/5 * * * *','echo run-task-now-ok','app1')`)
	defer mustExec(`DELETE FROM scheduled_tasks WHERE id='t-run'`)

	r := httptest.NewRequest("POST", "/api/tasks/t-run/run", nil)
	r.Header.Set("X-User-ID", "admin-uid")
	r.Header.Set("X-Role", "super_admin")
	r.SetPathValue("id", "t-run")
	w := httptest.NewRecorder()
	RunTaskNow(w, r)
	if w.Code != 200 {
		t.Fatalf("run-now = %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Started string `json:"started"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, w.Body.String())
	}
	if body.Data.Started != "t-run" {
		t.Errorf("started = %q, want t-run", body.Data.Started)
	}

	// The run is async — poll for the outcome to land.
	deadline := time.Now().Add(10 * time.Second)
	var status, logline string
	for time.Now().Before(deadline) {
		db.DB.QueryRow("SELECT last_status, last_log FROM scheduled_tasks WHERE id='t-run'").Scan(&status, &logline)
		if status != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status != "success" {
		t.Errorf("last_status = %q, want success", status)
	}
	if !strings.Contains(logline, "run-task-now-ok") {
		t.Errorf("last_log = %q, want command output", logline)
	}
}
