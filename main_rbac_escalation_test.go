package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
)

// rbacFixture seeds the four-role access matrix (super_admin, project_admin
// with proj-a, viewer with proj-b, no-access viewer) against two projects.
// Route-level RBAC tests all share this shape; tokens carry tv claims that
// match the seeded token_version.
func rbacFixture(t *testing.T) func(string, ...interface{}) {
	t.Helper()
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	oldKey := auth.JWTKey
	auth.JWTKey = []byte("rbac-test-jwt-secret-32-bytes-long")
	t.Cleanup(func() { auth.JWTKey = oldKey })

	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS audit_logs; DROP TABLE IF EXISTS notifications; DROP TABLE IF EXISTS scheduled_tasks; DROP TABLE IF EXISTS projects; DROP TABLE IF EXISTS users;`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`CREATE TABLE scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT, icon_thumb TEXT,
		stack TEXT, port TEXT, db_type TEXT, dsn TEXT, users_count INT DEFAULT 0,
		status TEXT DEFAULT 'online', sort_order INT DEFAULT 0, is_pinned BOOLEAN DEFAULT false,
		icon_cls TEXT, base_path TEXT, backend_url TEXT, service_name TEXT,
		startup_command TEXT, health_url TEXT, tags JSONB DEFAULT '[]', offline_msg TEXT,
		features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]',
		services JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now(),
		updated_at TIMESTAMPTZ DEFAULT now(),
		backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, token_version, project_access) VALUES
		('11111111-1111-1111-1111-111111111111','rbac_sa','超管','sa@t.c','x','super_admin','active',1,'["all"]'),
		('22222222-2222-2222-2222-222222222222','rbac_pa','管理员','pa@t.c','x','project_admin','active',1,'["proj-a"]'),
		('33333333-3333-3333-3333-333333333333','rbac_view','查看','view@t.c','x','viewer','active',1,'["proj-b"]'),
		('44444444-4444-4444-4444-444444444444','rbac_none','无权限','none@t.c','x','viewer','active',1,'[]')`)
	mustExec(`INSERT INTO projects (id, name, repo, dsn, status) VALUES
		('proj-a','项目A','proj-a','—','online'),
		('proj-b','项目B','proj-b','—','offline')`)
	return mustExec
}

// signRBACToken signs an HS256 token with the tv claim the seeded users'
// token_version expects (RequireAuth rejects missing tv against a live DB).
func signRBACToken(t *testing.T, uid, role string, tv int) string {
	t.Helper()
	claims := jwt.MapClaims{"user_id": uid, "username": "u", "role": role, "tv": tv, "exp": time.Now().Add(time.Hour).Unix()}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// rbacDo drives one request through the real mux (real server, real JWT).
// Headers beyond Authorization are for the forged-header scenarios.
func rbacDo(t *testing.T, ts *httptest.Server, method, path, token string, headers map[string]string, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// TestRBACVerticalEscalationMatrix — the full role × super-admin-route
// matrix: viewer and project_admin must be rejected on every WithAdmin route
// (403), an anonymous caller gets 401, and super_admin still reaches the
// handler (sanity on safe GETs). Route-level, real mux, real DB.
func TestRBACVerticalEscalationMatrix(t *testing.T) {
	rbacFixture(t)
	ts := httptest.NewServer(newMux())
	defer ts.Close()

	viewerTok := signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1)
	paTok := signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1)
	saTok := signRBACToken(t, "11111111-1111-1111-1111-111111111111", "super_admin", 1)

	saOnly := []struct {
		name, method, path, body string
	}{
		{"create project", "POST", "/api/projects", `{"name":"x","repo":"x"}`},
		{"delete project", "DELETE", "/api/projects/proj-a", ""},
		{"reorder projects", "PATCH", "/api/projects/reorder", `{"ordered_ids":["proj-a"]}`},
		{"refresh all", "POST", "/api/projects/refresh-all", ""},
		{"clone project", "POST", "/api/projects/proj-a/clone", ""},
		{"list users", "GET", "/api/users", ""},
		{"create user", "POST", "/api/users", `{"username":"x","email":"x@t.c","role":"viewer"}`},
		{"update user", "PUT", "/api/users/33333333-3333-3333-3333-333333333333", `{"username":"x","role":"viewer","status":"active"}`},
		{"delete user", "DELETE", "/api/users/44444444-4444-4444-4444-444444444444", ""},
		{"reset password", "POST", "/api/users/33333333-3333-3333-3333-333333333333/reset-password", `{"new_password":"abcdef"}`},
		{"get settings config", "GET", "/api/settings/config", ""},
		{"update settings config", "PUT", "/api/settings/config", `{}`},
		{"export projects", "GET", "/api/settings/export/projects", ""},
		{"export users", "GET", "/api/settings/export/users", ""},
		{"export project-users", "GET", "/api/settings/export/project-users/proj-a", ""},
		{"audit logs", "GET", "/api/settings/audit-logs", ""},
		{"datasources", "GET", "/api/settings/datasources", ""},
		{"list tasks", "GET", "/api/projects/proj-a/tasks", ""},
		{"create task", "POST", "/api/projects/proj-a/tasks", `{"name":"t","cron":"* * * * *","command":"echo"}`},
		{"update task", "PUT", "/api/tasks/t1", `{"name":"t","cron":"* * * * *","command":"echo"}`},
		{"delete task", "DELETE", "/api/tasks/t1", ""},
		{"run task", "POST", "/api/tasks/t1/run", ""},
		{"runtime detect", "POST", "/api/runtime/detect", ""},
		{"runtime local-services", "GET", "/api/runtime/local-services", ""},
		{"runtime allocate port", "POST", "/api/runtime/ports/allocate/proj-a", ""},
		{"runtime proc start", "POST", "/api/runtime/proc/start/proj-a", ""},
		{"runtime proc stop", "POST", "/api/runtime/proc/stop/proj-a", ""},
		{"runtime proc restart", "POST", "/api/runtime/proc/restart/proj-a", ""},
		{"runtime proc list", "GET", "/api/runtime/proc/list", ""},
		{"runtime proxy start", "POST", "/api/runtime/proxy/start/proj-a", ""},
		{"runtime proxy stop", "POST", "/api/runtime/proxy/stop/proj-a", ""},
	}

	for _, c := range saOnly {
		t.Run("viewer_"+c.name, func(t *testing.T) {
			if code, _ := rbacDo(t, ts, c.method, c.path, viewerTok, nil, c.body); code != 403 {
				t.Errorf("%s %s with viewer = %d, want 403", c.method, c.path, code)
			}
		})
		t.Run("pa_"+c.name, func(t *testing.T) {
			if code, _ := rbacDo(t, ts, c.method, c.path, paTok, nil, c.body); code != 403 {
				t.Errorf("%s %s with project_admin = %d, want 403", c.method, c.path, code)
			}
		})
		t.Run("anon_"+c.name, func(t *testing.T) {
			if code, _ := rbacDo(t, ts, c.method, c.path, "", nil, c.body); code != 401 {
				t.Errorf("%s %s without token = %d, want 401", c.method, c.path, code)
			}
		})
	}

	// Sanity: the routes are alive — super_admin reaches the handlers.
	for _, c := range []struct{ name, method, path string }{
		{"list users", "GET", "/api/users"},
		{"settings config", "GET", "/api/settings/config"},
		{"audit logs", "GET", "/api/settings/audit-logs"},
		{"proc list", "GET", "/api/runtime/proc/list"},
	} {
		if code, body := rbacDo(t, ts, c.method, c.path, saTok, nil, ""); code != 200 {
			t.Errorf("super_admin %s %s = %d (body %s), want 200", c.method, c.path, code, body)
		}
	}
}

// TestRBACHorizontalEscalationMatrix — project_admin(proj-a) against proj-b
// and viewer(proj-b) against proj-a, across every project-scoped route.
// Cross-project access must be denied by the handlers' own guards.
func TestRBACHorizontalEscalationMatrix(t *testing.T) {
	rbacFixture(t)
	ts := httptest.NewServer(newMux())
	defer ts.Close()

	paTok := signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1)
	viewTok := signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1)

	// pa(proj-a) poking at proj-b.
	crossPA := []struct {
		name, method, path, body string
		want                     int
	}{
		{"get project", "GET", "/api/projects/proj-b", "", 403},
		{"update project", "PUT", "/api/projects/proj-b", `{"name":"x"}`, 403},
		{"status patch", "PATCH", "/api/projects/proj-b/status", `{"status":"online"}`, 403},
		{"pin", "PATCH", "/api/projects/proj-b/pin", "", 403},
		{"test-connection", "POST", "/api/projects/proj-b/test-connection", "", 403},
		{"sync", "POST", "/api/projects/proj-b/sync", "", 403},
		{"insert row", "POST", "/api/projects/proj-b/data/row?table=t", `{"a":1}`, 403},
		{"delete row", "DELETE", "/api/projects/proj-b/data/row?table=t&pk=id&pkval=1", "", 403},
		{"update row", "PUT", "/api/projects/proj-b/data/row?table=t", `{"a":1}`, 403},
		{"list members", "GET", "/api/projects/proj-b/members", "", 403},
		{"add member", "POST", "/api/projects/proj-b/members", `{"user_id":"33333333-3333-3333-3333-333333333333"}`, 403},
		{"remove member", "DELETE", "/api/projects/proj-b/members/33333333-3333-3333-3333-333333333333", "", 403},
		{"list backups", "GET", "/api/backups/proj-b", "", 403},
		{"create backup", "POST", "/api/backups/proj-b", "", 403},
		{"tables", "GET", "/api/projects/proj-b/tables", "", 403},
		{"project logs", "GET", "/api/projects/proj-b/logs", "", 404},
	}
	for _, c := range crossPA {
		t.Run("pa_"+c.name, func(t *testing.T) {
			if code, _ := rbacDo(t, ts, c.method, c.path, paTok, nil, c.body); code != c.want {
				t.Errorf("project_admin %s %s = %d, want %d", c.method, c.path, code, c.want)
			}
		})
	}

	// viewer(proj-b) poking at proj-a.
	crossView := []struct {
		name, method, path, body string
		want                     int
	}{
		{"get project", "GET", "/api/projects/proj-a", "", 403},
		{"status patch", "PATCH", "/api/projects/proj-a/status", `{"status":"offline"}`, 403},
		{"sync", "POST", "/api/projects/proj-a/sync", "", 403},
		{"list members", "GET", "/api/projects/proj-a/members", "", 403},
		{"list backups", "GET", "/api/backups/proj-a", "", 403},
		{"project logs", "GET", "/api/projects/proj-a/logs", "", 404},
		{"tables", "GET", "/api/projects/proj-a/tables", "", 403},
	}
	for _, c := range crossView {
		t.Run("view_"+c.name, func(t *testing.T) {
			if code, _ := rbacDo(t, ts, c.method, c.path, viewTok, nil, c.body); code != c.want {
				t.Errorf("viewer %s %s = %d, want %d", c.method, c.path, code, c.want)
			}
		})
	}

	// Positive controls: own-project access works, proving the 403s above
	// come from the guards, not a blanket denial.
	if code, _ := rbacDo(t, ts, "GET", "/api/projects/proj-a", paTok, nil, ""); code != 200 {
		t.Errorf("pa own project = %d, want 200", code)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/projects/proj-b", viewTok, nil, ""); code != 200 {
		t.Errorf("viewer own project = %d, want 200", code)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/projects/proj-b/members", viewTok, nil, ""); code != 200 {
		t.Errorf("viewer own members = %d, want 200", code)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/backups/proj-a", paTok, nil, ""); code != 200 {
		t.Errorf("pa own backups = %d, want 200", code)
	}
}

// TestRBACForgedHeadersRouteLevel — a viewer's real token plus forged
// X-Role/X-User-ID headers must not grant admin routes (RequireAuth
// overwrites the headers from the DB row before any handler runs). The
// middleware-level overwrite is unit-tested in internal/auth; this locks
// the same property at the route level through the real mux.
func TestRBACForgedHeadersRouteLevel(t *testing.T) {
	rbacFixture(t)
	ts := httptest.NewServer(newMux())
	defer ts.Close()

	viewTok := signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1)
	forged := map[string]string{"X-Role": "super_admin", "X-User-ID": "11111111-1111-1111-1111-111111111111"}

	// Admin-only routes with forged super_admin headers: still 403.
	for _, c := range []struct{ name, method, path string }{
		{"list users", "GET", "/api/users"},
		{"create project", "POST", "/api/projects"},
		{"audit logs", "GET", "/api/settings/audit-logs"},
		{"settings config", "GET", "/api/settings/config"},
	} {
		if code, _ := rbacDo(t, ts, c.method, c.path, viewTok, forged, `{"name":"x","repo":"x"}`); code != 403 {
			t.Errorf("forged %s %s = %d, want 403", c.method, c.path, code)
		}
	}

	// Forged X-User-ID must not widen project visibility either: the viewer
	// still sees only their own access list (proj-b).
	code, body := rbacDo(t, ts, "GET", "/api/projects", viewTok, forged, "")
	if code != 200 {
		t.Fatalf("forged list projects = %d, want 200", code)
	}
	var env struct {
		Data struct {
			Projects []map[string]interface{} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, body)
	}
	if len(env.Data.Projects) != 1 || env.Data.Projects[0]["id"] != "proj-b" {
		ids := []string{}
		for _, p := range env.Data.Projects {
			ids = append(ids, p["id"].(string))
		}
		t.Errorf("forged viewer list = %v, want only [proj-b]", ids)
	}
}

// TestRBACTokenTamperRouteLevel — a JWT signed with a valid key but
// role=super_admin in the payload, while the DB row is a viewer: the
// per-request DB lookup must override the claim (RequireAuthRealDBBranches
// covers the middleware; this pins the route-level outcome).
func TestRBACTokenTamperRouteLevel(t *testing.T) {
	rbacFixture(t)
	ts := httptest.NewServer(newMux())
	defer ts.Close()

	// DB row 44444444 is viewer with [] access; claims claim super_admin.
	tampered := signRBACToken(t, "44444444-4444-4444-4444-444444444444", "super_admin", 1)

	if code, _ := rbacDo(t, ts, "GET", "/api/users", tampered, nil, ""); code != 403 {
		t.Errorf("tampered token /api/users = %d, want 403", code)
	}
	if code, _ := rbacDo(t, ts, "POST", "/api/projects", tampered, nil, `{"name":"x","repo":"x"}`); code != 403 {
		t.Errorf("tampered token create project = %d, want 403", code)
	}
	// The same token is scoped as the DB viewer on authed routes: empty list.
	code, body := rbacDo(t, ts, "GET", "/api/projects", tampered, nil, "")
	if code != 200 {
		t.Fatalf("tampered list projects = %d, want 200", code)
	}
	var env struct {
		Data struct {
			Projects []interface{} `json:"projects"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(env.Data.Projects) != 0 {
		t.Errorf("tampered viewer projects = %v, want empty (DB role rules)", env.Data.Projects)
	}
}

// TestRBACTokenVersionInvalidationRouteLevel — a valid token works, then a
// token_version bump in the DB kills it on the next request (password
// change / reset revocation, pinned at the route level).
func TestRBACTokenVersionInvalidationRouteLevel(t *testing.T) {
	mustExec := rbacFixture(t)
	ts := httptest.NewServer(newMux())
	defer ts.Close()

	tok := signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1)
	if code, _ := rbacDo(t, ts, "GET", "/api/projects", tok, nil, ""); code != 200 {
		t.Fatalf("valid token = %d, want 200", code)
	}

	mustExec(`UPDATE users SET token_version=token_version+1 WHERE id='33333333-3333-3333-3333-333333333333'`)

	if code, body := rbacDo(t, ts, "GET", "/api/projects", tok, nil, ""); code != 401 {
		t.Errorf("stale token = %d (body %s), want 401", code, body)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/users", tok, nil, ""); code != 401 {
		t.Errorf("stale token admin route = %d, want 401", code)
	}
}
