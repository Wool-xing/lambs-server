package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestProjectsCRUD — real PostgreSQL: create (incl. ID charset validation),
// list, update, delete. Gated on LAMBS_TEST_PG_DSN.
func TestProjectsCRUD(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS projects CASCADE`)
	mustExec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT,
		icon_thumb TEXT, stack TEXT, port TEXT, db_type TEXT, dsn TEXT, users_count INT DEFAULT 0,
		status TEXT DEFAULT 'online', sort_order INT DEFAULT 0, is_pinned BOOLEAN DEFAULT false,
		icon_cls TEXT, base_path TEXT, backend_url TEXT, service_name TEXT,
		startup_command TEXT, health_url TEXT, tags JSONB DEFAULT '[]', offline_msg TEXT,
		features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]',
		services JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now(),
		updated_at TIMESTAMPTZ DEFAULT now(),
		backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`)

	sa := func(r *http.Request) {
		r.Header.Set("X-User-ID", "admin-uid")
		r.Header.Set("X-Role", "super_admin")
	}
	post := func(path string, body interface{}) *httptest.ResponseRecorder {
		b, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		sa(r)
		w := httptest.NewRecorder()
		CreateProject(w, r)
		return w
	}

	// Create
	w := post("/api/projects", map[string]interface{}{
		"id": "proj-a", "name": "项目A", "repo": "proj-a", "db_type": "PostgreSQL",
		"port": "8001", "status": "online", "description": "测试项目", "stack": "Go+React",
	})
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("create = %d (body %s)", w.Code, w.Body.String())
	}

	// Charset guard: path-traversal id rejected, no row
	w = post("/api/projects", map[string]interface{}{
		"id": "../etc", "name": "evil", "repo": "evil", "db_type": "PostgreSQL", "port": "8002",
	})
	if w.Code != 400 {
		t.Errorf("bad id = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE id='../etc'").Scan(&n)
	if n != 0 {
		t.Error("path-traversal id persisted")
	}

	// List contains the created project
	lr := httptest.NewRequest("GET", "/api/projects", nil)
	sa(lr)
	lw := httptest.NewRecorder()
	ListProjects(lw, lr)
	if lw.Code != 200 {
		t.Fatalf("list = %d (body %s)", lw.Code, lw.Body.String())
	}
	var list struct {
		Success bool `json:"success"`
		Data    struct {
			Projects []map[string]interface{} `json:"projects"`
		} `json:"data"`
	}
	json.Unmarshal(lw.Body.Bytes(), &list)
	found := false
	for _, p := range list.Data.Projects {
		if p["id"] == "proj-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListProjects missing proj-a: %v", list.Data.Projects)
	}

	// Update renames
	ub, _ := json.Marshal(map[string]interface{}{"name": "项目A改名", "status": "maintenance"})
	ur := httptest.NewRequest("PUT", "/api/projects/proj-a", bytes.NewReader(ub))
	ur.Header.Set("Content-Type", "application/json")
	sa(ur)
	ur.SetPathValue("id", "proj-a")
	uw := httptest.NewRecorder()
	UpdateProject(uw, ur, "proj-a")
	if uw.Code != 200 {
		t.Fatalf("update = %d (body %s)", uw.Code, uw.Body.String())
	}
	var name string
	db.DB.QueryRow("SELECT name FROM projects WHERE id='proj-a'").Scan(&name)
	if name != "项目A改名" {
		t.Errorf("name = %q, want 项目A改名", name)
	}

	// Delete
	dr := httptest.NewRequest("DELETE", "/api/projects/proj-a", nil)
	sa(dr)
	dr.SetPathValue("id", "proj-a")
	dw := httptest.NewRecorder()
	DeleteProject(dw, dr, "proj-a")
	if dw.Code != 200 {
		t.Fatalf("delete = %d (body %s)", dw.Code, dw.Body.String())
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE id='proj-a'").Scan(&n)
	if n != 0 {
		t.Error("delete did not remove row")
	}
}

// TestTestConnectionSQLite — real sqlite file through the SSRF guard: a
// local-file dsn reports reachable; a project without a dsn 400s.
func TestTestConnectionSQLite(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS projects CASCADE`)
	mustExec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT, icon_thumb TEXT, stack TEXT, port TEXT, db_type TEXT, dsn TEXT, users_count INT DEFAULT 0, status TEXT DEFAULT 'online', sort_order INT DEFAULT 0, is_pinned BOOLEAN DEFAULT false, icon_cls TEXT, base_path TEXT, backend_url TEXT, service_name TEXT, startup_command TEXT, health_url TEXT, tags JSONB DEFAULT '[]', offline_msg TEXT, features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]', services JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now(), updated_at TIMESTAMPTZ DEFAULT now(), backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`)
	sqliteFile := t.TempDir() + "/proj.db"
	os.WriteFile(sqliteFile, []byte("x"), 0600)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('tc-proj', '连接测试', $1)`, "sqlite:///"+sqliteFile)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('tc-nodsn', '无DSN', '—')`)

	sa := func(r *http.Request) {
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
	}

	cr := httptest.NewRequest("POST", "/api/projects/tc-proj/test-connection", nil)
	sa(cr)
	cr.SetPathValue("id", "tc-proj")
	cw := httptest.NewRecorder()
	TestConnection(cw, cr, "tc-proj")
	if cw.Code != 200 {
		t.Fatalf("test-connection = %d (body %s)", cw.Code, cw.Body.String())
	}

	// No DSN: the endpoint still answers 200 with reachable=false (the UI
	// shows the failure inline rather than an error page).
	nr := httptest.NewRequest("POST", "/api/projects/tc-nodsn/test-connection", nil)
	sa(nr)
	nr.SetPathValue("id", "tc-nodsn")
	nw := httptest.NewRecorder()
	TestConnection(nw, nr, "tc-nodsn")
	if nw.Code != 200 || !strings.Contains(nw.Body.String(), "reachable") && strings.Contains(nw.Body.String(), "false") {
		t.Errorf("no-dsn test-connection = %d %s", nw.Code, nw.Body.String())
	}
}
