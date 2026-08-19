package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

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
