package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
)

// TestRuntimeRoutesMux — the 8 runtime closure routes registered in newMux()
// (ports/allocate, proc/start|stop|restart|status|list, proxy/start|stop)
// must answer through the real mux + super_admin gate with the documented
// JSON shapes. Real PostgreSQL fixture for the projects rows; the proc routes
// use a project whose startup_command really starts (sleep) so Status sees a
// live process. Gated on LAMBS_TEST_PG_DSN.
func TestRuntimeRoutesMux(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not present")
	}
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	oldKey := auth.JWTKey
	auth.JWTKey = []byte("runtime-routes-test-jwt-secret")
	t.Cleanup(func() { auth.JWTKey = oldKey })
	t.Setenv("LAMBS_MIN_FREE_MB", "0") // CI boxes may sit under the 100MB floor

	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS audit_logs; DROP TABLE IF EXISTS notifications; DROP TABLE IF EXISTS projects; DROP TABLE IF EXISTS users;`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
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

	ts := httptest.NewServer(newMux())
	defer ts.Close()

	do := func(method, path, token string, body interface{}) (int, map[string]interface{}) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, ts.URL+path, rd)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var parsed map[string]interface{}
		json.Unmarshal(raw, &parsed)
		return resp.StatusCode, parsed
	}

	// Super-admin user inserted directly + JWT minted with auth.JWTKey — the
	// register endpoint shares a 5/min per-IP limiter with the route sweep and
	// smoke tests (each POST /api/auth/* consumes a slot), so hitting it here
	// would tip the shared window over 5 and 429 TestRealBackendSmoke.
	mustExec(`INSERT INTO users (id, username, email, password_hash, role, status) VALUES
		('11111111-2222-3333-4444-555555555555', 'runtime_admin', 'runtime@smoke.test', 'x', 'super_admin', 'active')`)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"user_id":  "11111111-2222-3333-4444-555555555555",
		"username": "runtime_admin",
		"role":     "super_admin",
		"tv":       0,
		"exp":      jwt.NewNumericDate(time.Now().Add(8 * time.Hour)),
		"iat":      jwt.NewNumericDate(time.Now()),
	}).SignedString(auth.JWTKey)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	var code int
	var body map[string]interface{}

	// Projects: rt-noproc has neither service_name nor startup_command (Start
	// must refuse → 500); rt-proc has a real long-running startup_command;
	// rt-proxy carries a port + backend_url for a real listener; rt-alloc is
	// the allocation target.
	mustExec(`INSERT INTO projects (id, name, port) VALUES
		('rt-alloc', 'alloc', ''),
		('rt-proxy', 'proxy', '35600')`)
	mustExec(`UPDATE projects SET backend_url='http://127.0.0.1:9' WHERE id='rt-proxy'`)
	mustExec(`INSERT INTO projects (id, name, port, startup_command) VALUES ('rt-proc', 'proc', '3607', 'sleep 30')`)
	mustExec(`INSERT INTO projects (id, name) VALUES ('rt-noproc', 'noproc')`)

	// ── proxy/start + proxy/stop: real listener on :35600, then teardown.
	code, body = do("POST", "/api/runtime/proxy/start/rt-proxy", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["status"] != "started" {
		t.Fatalf("proxy/start = %d (%v)", code, body)
	}
	code, body = do("POST", "/api/runtime/proxy/stop/rt-proxy", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["status"] != "stopped" {
		t.Fatalf("proxy/stop = %d (%v)", code, body)
	}

	// ── ports/allocate: in-range port persists; re-allocation is idempotent.
	code, body = do("POST", "/api/runtime/ports/allocate/rt-alloc", token, nil)
	if code != 200 {
		t.Fatalf("ports/allocate = %d (%v)", code, body)
	}
	d := body["data"].(map[string]interface{})
	port, _ := d["port"].(float64)
	if port < 3510 || port > 3599 || d["project_id"] != "rt-alloc" {
		t.Fatalf("allocate data = %v, want in-range port for rt-alloc", d)
	}
	code, body = do("POST", "/api/runtime/ports/allocate/rt-alloc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["port"] != port {
		t.Errorf("re-allocate = %d (%v), want same port %.0f", code, body, port)
	}

	// ── proc/start failure: no service_name/startup_command → 500 JSON.
	code, body = do("POST", "/api/runtime/proc/start/rt-noproc", token, nil)
	if code != 500 || body["success"] != false {
		t.Fatalf("proc/start noproc = %d (%v), want 500", code, body)
	}

	// ── proc/start success → status reports running through the routes.
	code, body = do("POST", "/api/runtime/proc/start/rt-proc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["running"] != true {
		t.Fatalf("proc/start = %d (%v)", code, body)
	}
	code, body = do("GET", "/api/runtime/proc/status/rt-proc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["running"] != true {
		t.Fatalf("proc/status = %d (%v)", code, body)
	}
	// Unknown project: status degrades to running:false, never 500.
	code, body = do("GET", "/api/runtime/proc/status/rt-missing", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["running"] != false {
		t.Fatalf("proc/status missing = %d (%v)", code, body)
	}

	// ── proc/list: the tracked process is visible with a count.
	code, body = do("GET", "/api/runtime/proc/list", token, nil)
	if code != 200 {
		t.Fatalf("proc/list = %d (%v)", code, body)
	}
	ld := body["data"].(map[string]interface{})
	procs := ld["processes"].([]interface{})
	found := false
	for _, p := range procs {
		if pm := p.(map[string]interface{}); pm["project_id"] == "rt-proc" {
			found = true
		}
	}
	if !found || int(ld["count"].(float64)) < 1 {
		t.Errorf("proc/list = %v, want rt-proc present", ld)
	}

	// ── proc/restart: stop+start round trip, still running afterwards.
	code, body = do("POST", "/api/runtime/proc/restart/rt-proc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["running"] != true {
		t.Fatalf("proc/restart = %d (%v)", code, body)
	}

	// ── proc/stop: confirmed stopped, status flips to running:false.
	code, body = do("POST", "/api/runtime/proc/stop/rt-proc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["stopped"] != "rt-proc" {
		t.Fatalf("proc/stop = %d (%v)", code, body)
	}
	code, body = do("GET", "/api/runtime/proc/status/rt-proc", token, nil)
	if code != 200 || body["data"].(map[string]interface{})["running"] != false {
		t.Fatalf("proc/status after stop = %d (%v)", code, body)
	}
}
