package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestPatchProjectStatusNotifies — the write-side contract for status
// changes (QA round 3 idea 1): one notification row with the right
// project_id/type/title must land when a status flips. Read-side authz
// is covered elsewhere; the write side had zero guards.
func TestPatchProjectStatusNotifies(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS projects (
		id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT,
		icon_thumb TEXT, stack TEXT, port TEXT, db_type TEXT, dsn TEXT, users_count INT DEFAULT 0,
		status TEXT DEFAULT 'online', sort_order INT DEFAULT 0, is_pinned BOOLEAN DEFAULT false,
		icon_cls TEXT, base_path TEXT, backend_url TEXT, service_name TEXT,
		startup_command TEXT, health_url TEXT, tags JSONB DEFAULT '[]', offline_msg TEXT,
		features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]',
		services JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now(),
		updated_at TIMESTAMPTZ DEFAULT now(),
		backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`)
	mustExec(`CREATE TABLE IF NOT EXISTS notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`DELETE FROM projects WHERE id='proj-x'; DELETE FROM notifications WHERE project_id='proj-x';`)
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('proj-x', '项目X', 'online')`)

	body, _ := json.Marshal(map[string]string{"status": "offline"})
	r := httptest.NewRequest("PATCH", "/api/projects/proj-x/status", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
	r.SetPathValue("id", "proj-x")
	w := httptest.NewRecorder()
	PatchProjectStatus(w, r, "proj-x")
	if w.Code != 200 {
		t.Fatalf("patch = %d (body %s)", w.Code, w.Body.String())
	}

	var status string
	db.DB.QueryRow("SELECT status FROM projects WHERE id='proj-x'").Scan(&status)
	if status != "offline" {
		t.Errorf("status = %q, want offline", status)
	}
	var nid, ntype, title, content string
	err := db.DB.QueryRow("SELECT id, type, title, content FROM notifications WHERE project_id='proj-x' ORDER BY created_at DESC LIMIT 1").
		Scan(&nid, &ntype, &title, &content)
	if err != nil {
		t.Fatalf("no notification row written: %v", err)
	}
	if nid == "" || ntype != "status" || title != "项目状态变更" {
		t.Errorf("notification contract broken: id=%q type=%q title=%q", nid, ntype, title)
	}
	if content == "" {
		t.Error("notification content empty")
	}

	// Non-admin (viewer) is rejected before any write.
	r2 := httptest.NewRequest("PATCH", "/api/projects/proj-x/status", bytes.NewReader(body))
	r2.Header.Set("X-User-ID", "viewer")
	r2.Header.Set("X-Role", "viewer")
	r2.SetPathValue("id", "proj-x")
	w2 := httptest.NewRecorder()
	PatchProjectStatus(w2, r2, "proj-x")
	if w2.Code != 403 {
		t.Errorf("viewer patch = %d, want 403", w2.Code)
	}
}
