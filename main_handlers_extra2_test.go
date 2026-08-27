package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

func TestEnvOr(t *testing.T) {
	if got := envOr("LAMBS_TEST_ENVOR_ABSENT_KEY", "def"); got != "def" {
		t.Fatalf("envOr absent = %q", got)
	}
	t.Setenv("LAMBS_TEST_ENVOR_PRESENT_KEY", "val")
	if got := envOr("LAMBS_TEST_ENVOR_PRESENT_KEY", "def"); got != "val" {
		t.Fatalf("envOr present = %q", got)
	}
}

// TestAggregatedLogsNonSAWithAccess — the viewer branch WITH project access:
// own audit rows + status rows for accessible projects only (round-10 gap:
// the non-SA path previously only had the empty-access case).
func TestAggregatedLogsNonSAWithAccess(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE IF NOT EXISTS users (id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE, password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active', token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '', project_access JSONB NOT NULL DEFAULT '[]', avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '', last_login TIMESTAMPTZ DEFAULT now(), created_at TIMESTAMPTZ DEFAULT now())`)
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
	mustExec(`DELETE FROM audit_logs; DELETE FROM users`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, project_access) VALUES (gen_random_uuid(),'viewer1','观众','v1@t.c','x','["acc-proj"]')`)
	mustExec(`INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ((SELECT id::text FROM users WHERE username='viewer1'),'观众','登录','Lambs','登录成功')`)
	// accessible project: one offline (warn level), one not accessible
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('acc-proj','可达项目','offline')`)
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('other-proj','不可达项目','online')`)

	r := httptest.NewRequest("GET", "/api/logs/aggregated?lines=20", nil)
	r.Header.Set("X-Role", "viewer")
	r.Header.Set("X-User-ID", mustID(t, "viewer1"))
	w := httptest.NewRecorder()
	handleAggregatedLogs(w, r)
	if w.Code != 200 {
		t.Fatalf("code = %d (%s)", w.Code, w.Body.String())
	}
	var body struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	foundAudit, foundAcc, foundOther := false, false, false
	for _, row := range body.Data {
		switch row["project_name"] {
		case "可达项目":
			foundAcc = true
			if row["level"] != "warn" {
				t.Fatalf("offline project level = %v, want warn", row["level"])
			}
		case "不可达项目":
			foundOther = true
		case "观众":
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Error("own audit rows missing from viewer logs")
	}
	if !foundAcc {
		t.Error("accessible project status row missing")
	}
	if foundOther {
		t.Error("inaccessible project leaked into viewer logs")
	}
}

func mustID(t *testing.T, username string) string {
	t.Helper()
	var id string
	if err := db.DB.QueryRow("SELECT id::text FROM users WHERE username=$1", username).Scan(&id); err != nil {
		t.Fatalf("user id: %v", err)
	}
	return id
}

// TestNewMuxRouteSweep — every registered route must answer without panic
// through the real mux + auth gate. Codes vary by gate/validation; the
// contract is an HTTP response (no 500 panic).
func TestNewMuxRouteSweep(t *testing.T) {
	ts := httptest.NewServer(newMux())
	defer ts.Close()
	// Full registration table (method + path), params replaced with "x".
	// The contract: every route answers through the real mux + gate without
	// a 500 panic. Codes vary by gate/validation.
	routes := []struct{ method, path string }{
		{"POST", "/api/auth/login"}, {"GET", "/api/auth/salt"}, {"GET", "/api/health"},
		{"GET", "/api/gate/check-internal"}, {"GET", "/api/gate/offline-page"},
		{"GET", "/api/gate/project-logo"}, {"GET", "/api/projects/x/logo"},
		{"POST", "/api/auth/register"}, {"POST", "/api/auth/forgot-password/request"},
		{"POST", "/api/auth/forgot-password/verify"}, {"GET", "/api/auth/me"},
		{"GET", "/api/me"}, {"PUT", "/api/auth/me/password"},
		{"GET", "/api/projects"}, {"GET", "/api/projects/stats"}, {"GET", "/api/projects/x"},
		{"POST", "/api/projects"}, {"PUT", "/api/projects/x"}, {"DELETE", "/api/projects/x"},
		{"PATCH", "/api/projects/x/status"}, {"PATCH", "/api/projects/x/pin"},
		{"PATCH", "/api/projects/reorder"}, {"POST", "/api/projects/x/test-connection"},
		{"POST", "/api/projects/x/sync"}, {"POST", "/api/projects/refresh-all"},
		{"GET", "/api/projects/x/logs"}, {"GET", "/api/projects/x/tables"},
		{"GET", "/api/projects/x/tables/list"}, {"PUT", "/api/projects/x/data/row"},
		{"DELETE", "/api/projects/x/data/row"}, {"POST", "/api/projects/x/data/row"},
		{"GET", "/api/projects/x/members"}, {"POST", "/api/projects/x/members"},
		{"DELETE", "/api/projects/x/members/x"}, {"POST", "/api/projects/x/clone"},
		{"POST", "/api/projects/x/vector-search"},
		{"GET", "/api/users"}, {"POST", "/api/users"}, {"PUT", "/api/users/x"},
		{"DELETE", "/api/users/x"}, {"POST", "/api/users/x/reset-password"},
		{"GET", "/api/gate/check"}, {"GET", "/api/settings/config"},
		{"PUT", "/api/settings/config"}, {"GET", "/api/settings/export/projects"},
		{"GET", "/api/settings/export/users"}, {"GET", "/api/settings/export/project-users/x"},
		{"GET", "/api/settings/audit-logs"}, {"GET", "/api/settings/datasources"},
		{"POST", "/api/backups/x"}, {"GET", "/api/backups/x"},
		{"GET", "/api/backups/x/download/x.db"}, {"DELETE", "/api/backups/x/download/x.db"},
		{"POST", "/api/backups/x/restore/x.db"}, {"POST", "/api/backups/x/upload-tg/x.db"},
		{"GET", "/api/notifications"}, {"POST", "/api/notifications/x/read"},
		{"POST", "/api/notifications/read-all"}, {"DELETE", "/api/notifications/x"},
		{"GET", "/api/system/health"},
	}
	for _, rt := range routes {
		req, err := http.NewRequest(rt.method, ts.URL+rt.path, nil)
		if err != nil {
			t.Fatalf("%s %s: %v", rt.method, rt.path, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", rt.method, rt.path, err)
		}
		resp.Body.Close()
		if resp.StatusCode == 500 {
			t.Fatalf("%s %s panicked (500)", rt.method, rt.path)
		}
	}
}
