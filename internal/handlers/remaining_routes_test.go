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

// TestProjectMembersAndClone — real PostgreSQL: member listing includes
// super_admins and project-access users; clone copies a project with a new
// id. Gated on LAMBS_TEST_PG_DSN.
func TestProjectMembersAndClone(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS users CASCADE`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`INSERT INTO projects (id, name, repo, description, icon_url, icon_thumb, stack, port, db_type, dsn, icon_cls, base_path, backend_url, service_name, tags, offline_msg, features, tabs) VALUES ('src-proj', '源项目', 'src', '', '', '', '', '', 'SQLite', 'sqlite:///x.db', '', '', '', '', '[]', '', '[]', '[]')`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, project_access)
		VALUES (gen_random_uuid(),'sa1','超管','sa1@t.c','x','super_admin','[]'),
		       (gen_random_uuid(),'v1','成员','v1@t.c','x','viewer','["src-proj"]'),
		       (gen_random_uuid(),'v2','外人','v2@t.c','x','viewer','[]')`)

	sa := func(r *http.Request) {
		r.Header.Set("X-User-ID", "sa1")
		r.Header.Set("X-Role", "super_admin")
	}

	// Members: super_admin + project member; outsider excluded.
	mr := httptest.NewRequest("GET", "/api/projects/src-proj/members", nil)
	sa(mr)
	mr.SetPathValue("id", "src-proj")
	mw := httptest.NewRecorder()
	ProjectMembers(mw, mr, "src-proj")
	if mw.Code != 200 {
		t.Fatalf("members = %d (body %s)", mw.Code, mw.Body.String())
	}
	var mbody struct {
		Data struct {
			Members []map[string]interface{} `json:"members"`
		} `json:"data"`
	}
	json.Unmarshal(mw.Body.Bytes(), &mbody)
	names := map[string]bool{}
	for _, m := range mbody.Data.Members {
		if u, ok := m["username"].(string); ok {
			names[u] = true
		}
	}
	if !names["sa1"] || !names["v1"] {
		t.Errorf("members missing sa1/v1: %v", names)
	}
	if names["v2"] {
		t.Errorf("outsider leaked into members: %v", names)
	}

	// Clone: new id exists with copied fields.
	body, _ := json.Marshal(map[string]string{"new_id": "clone-proj", "new_name": "克隆项目"})
	cr := httptest.NewRequest("POST", "/api/projects/src-proj/clone", bytes.NewReader(body))
	cr.Header.Set("Content-Type", "application/json")
	sa(cr)
	cr.SetPathValue("id", "src-proj")
	cw := httptest.NewRecorder()
	CloneProject(cw, cr, "src-proj")
	if cw.Code != 200 && cw.Code != 201 {
		t.Fatalf("clone = %d (body %s)", cw.Code, cw.Body.String())
	}
	// CloneProject derives the new id as repo + "-clone" (payload ids are
	// ignored by design) and copies the name.
	var cname string
	if err := db.DB.QueryRow("SELECT name FROM projects WHERE id='src-clone'").Scan(&cname); err != nil {
		t.Fatalf("clone row missing: %v", err)
	}
	if cname != "源项目 (副本)" {
		t.Errorf("clone name = %q", cname)
	}
}

// TestVectorSearchNoDSN — vector search on a project without a vector
// datasource returns a clean error, not a panic.
func TestVectorSearchNoDSN(t *testing.T) {
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
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('no-vec', '无向量', '—')`)

	r := httptest.NewRequest("POST", "/api/projects/no-vec/vector-search", bytes.NewReader([]byte(`{"query":"测试"}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "sa1")
	r.Header.Set("X-Role", "super_admin")
	r.SetPathValue("id", "no-vec")
	w := httptest.NewRecorder()
	VectorSearch(w, r, "no-vec")
	if w.Code < 400 || w.Code >= 500 {
		t.Errorf("vector search no-dsn = %d, want 4xx (body %s)", w.Code, w.Body.String())
	}
}
