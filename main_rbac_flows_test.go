package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
)

// TestForgotResetInvalidatesOldTokensAndNewLogin — the full forgot-password
// chain at route level: token issued before the reset dies (token_version
// bump, asserted both in the DB and by a 401 on an authed route), and the
// new password logs in successfully with a fresh token. (Handler-level
// verify branches live in internal/auth; this closes the route-level loop.)
func TestForgotResetInvalidatesOldTokensAndNewLogin(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	oldKey := auth.JWTKey
	auth.JWTKey = []byte("forgot-flow-jwt-secret-32-bytes-long")
	t.Cleanup(func() { auth.JWTKey = oldKey })

	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS audit_logs; DROP TABLE IF EXISTS notifications; DROP TABLE IF EXISTS projects; DROP TABLE IF EXISTS users; DROP TABLE IF EXISTS verification_codes;`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	auth.EnsureForgotSchema()
	mustExec(`DELETE FROM verification_codes`)

	// Seed fg-user with the legacy plaintext shape: bcrypt(sha256(pwd)).
	h, err := bcrypt.GenerateFromPassword([]byte(sha256HexForTests("oldpass123")), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access) VALUES
		('aaaaaaaa-0000-0000-0000-000000000001','fg-user','忘记','fg@t.c',$1,'viewer','active','[]')`, string(h))

	ts := httptest.NewServer(newMux())
	defer ts.Close()

	// Credential endpoints are IP-rate-limited (5/min); distinct
	// X-Forwarded-For values keep each test's calls in their own bucket
	// (same pattern as the auth package's own rate-limit tests).
	xff := func(ip string) map[string]string { return map[string]string{"X-Forwarded-For": ip} }

	// Login with the current password → working token.
	code, body := rbacDo(t, ts, "POST", "/api/auth/login", "", xff("10.9.9.1"), `{"username":"fg-user","password":"oldpass123"}`)
	if code != 200 {
		t.Fatalf("login = %d (%s)", code, body)
	}
	var loginResp struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &loginResp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	oldTok := loginResp.Data.AccessToken
	if oldTok == "" {
		t.Fatalf("no token: %s", body)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/me", oldTok, nil, ""); code != 200 {
		t.Fatalf("me with old token = %d, want 200", code)
	}

	// Issue a valid reset code (stored MAC = hmac-sha256(JWTKey, code)).
	mac := hmac.New(sha256.New, auth.JWTKey)
	mac.Write([]byte("654321"))
	mustExec(`INSERT INTO verification_codes (username, email, code, used, expires_at) VALUES ('fg-user','fg@t.c',$1,FALSE,NOW()+INTERVAL '10 minutes')`, hex.EncodeToString(mac.Sum(nil)))

	// Verify: plaintext new password gets wrapped with the account salt.
	code, body = rbacDo(t, ts, "POST", "/api/auth/forgot-password/verify", "", xff("10.9.9.2"), `{"username":"fg-user","email":"fg@t.c","code":"654321","new_password":"newpass123"}`)
	if code != 200 {
		t.Fatalf("forgot verify = %d (%s)", code, body)
	}
	var tv int
	db.DB.QueryRow("SELECT token_version FROM users WHERE username='fg-user'").Scan(&tv)
	if tv != 1 {
		t.Errorf("token_version = %d, want 1", tv)
	}

	// The pre-reset token is dead on authed routes.
	if code, _ := rbacDo(t, ts, "GET", "/api/me", oldTok, nil, ""); code != 401 {
		t.Errorf("old token after reset = %d, want 401", code)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/projects", oldTok, nil, ""); code != 401 {
		t.Errorf("old token projects = %d, want 401", code)
	}

	// The new password logs in and the fresh token works.
	code, body = rbacDo(t, ts, "POST", "/api/auth/login", "", xff("10.9.9.3"), `{"username":"fg-user","password":"newpass123"}`)
	if code != 200 {
		t.Fatalf("login with new password = %d (%s)", code, body)
	}
	var newLogin struct {
		Data struct {
			AccessToken string `json:"access_token"`
		} `json:"data"`
	}
	json.Unmarshal([]byte(body), &newLogin)
	if newLogin.Data.AccessToken == "" {
		t.Fatalf("no fresh token: %s", body)
	}
	if code, _ := rbacDo(t, ts, "GET", "/api/me", newLogin.Data.AccessToken, nil, ""); code != 200 {
		t.Errorf("me with fresh token = %d, want 200", code)
	}
	// The old password must be rejected now.
	if code, _ := rbacDo(t, ts, "POST", "/api/auth/login", "", xff("10.9.9.5"), `{"username":"fg-user","password":"oldpass123"}`); code != 401 {
		t.Errorf("old password login = %d, want 401", code)
	}
}

// TestRegisterFirstLoginEmptyProjects — a freshly registered user (not the
// first account, so viewer) auto-logs-in, /api/me works, and with empty
// project_access sees an empty project list (no data leak to a new user).
func TestRegisterFirstLoginEmptyProjects(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	oldKey := auth.JWTKey
	auth.JWTKey = []byte("register-flow-jwt-secret-32-bytes-long")
	t.Cleanup(func() { auth.JWTKey = oldKey })

	mustExec := func(q string, args ...interface{}) {
		t.Helper()
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
		last_login TIMESTAMPTZ DEFAULT now(),
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
	// Existing account makes the new registration a viewer, not super_admin.
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access) VALUES
		('aaaaaaaa-0000-0000-0000-000000000099','existing','已有','exist@t.c','x','super_admin','active','["all"]')`)
	mustExec(`INSERT INTO projects (id, name, repo, dsn, status) VALUES
		('proj-a','项目A','proj-a','—','online'), ('proj-b','项目B','proj-b','—','offline')`)

	ts := httptest.NewServer(newMux())
	defer ts.Close()

	code, body := rbacDo(t, ts, "POST", "/api/auth/register", "", map[string]string{"X-Forwarded-For": "10.9.9.4"}, `{"username":"fresh_viewer","email":"fresh@t.c","password":"secret123"}`)
	if code != 201 {
		t.Fatalf("register = %d (%s)", code, body)
	}
	var reg struct {
		Data struct {
			AccessToken string `json:"access_token"`
			User        map[string]interface{} `json:"user"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &reg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reg.Data.AccessToken == "" {
		t.Fatalf("register returned no token: %s", body)
	}
	if reg.Data.User["role"] != "viewer" {
		t.Errorf("registered role = %v, want viewer", reg.Data.User["role"])
	}

	// First login (auto-login token) reaches /api/me.
	if code, body := rbacDo(t, ts, "GET", "/api/me", reg.Data.AccessToken, nil, ""); code != 200 {
		t.Fatalf("me = %d (%s)", code, body)
	}

	// Empty project_access → empty project list; seeded projects are hidden.
	code, body = rbacDo(t, ts, "GET", "/api/projects", reg.Data.AccessToken, nil, "")
	if code != 200 {
		t.Fatalf("projects = %d (%s)", code, body)
	}
	var env struct {
		Data struct {
			Projects []interface{} `json:"projects"`
			Total    int           `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("unmarshal projects: %v", err)
	}
	if len(env.Data.Projects) != 0 || env.Data.Total != 0 {
		t.Errorf("new viewer projects = %v/%d, want empty", env.Data.Projects, env.Data.Total)
	}
}

// sha256HexForTests mirrors the auth package's wrapping for seed hashes.
func sha256HexForTests(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
