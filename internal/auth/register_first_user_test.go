package auth

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestFirstUserBecomesSuperAdmin — a fresh deployment must bootstrap: the
// first registered account becomes super_admin with 'all' access, otherwise
// an open-source user registers a viewer and hits a dead end (viewers can't
// create projects or users). QA round 4 onboarding audit.
func TestFirstUserBecomesSuperAdmin(t *testing.T) {
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
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS users CASCADE`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)

	reg := func(username string) (int, map[string]interface{}) {
		b, _ := json.Marshal(map[string]string{"username": username, "email": username + "@t.c", "password": "secret123"})
		r := httptest.NewRequest("POST", "/api/auth/register", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		HandleRegister(w, r)
		var body map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	code, body := reg("first_user")
	if code != 201 {
		t.Fatalf("first register = %d (body %v)", code, body)
	}
	var role string
	var access string
	db.DB.QueryRow("SELECT role, COALESCE(project_access::text,'[]') FROM users WHERE username='first_user'").Scan(&role, &access)
	if role != "super_admin" {
		t.Errorf("first user role = %q, want super_admin", role)
	}
	if access != `["all"]` {
		t.Errorf("first user project_access = %q, want [\"all\"]", access)
	}

	// Second user stays a viewer.
	code2, _ := reg("second_user")
	if code2 != 201 {
		t.Fatalf("second register = %d", code2)
	}
	var role2 string
	db.DB.QueryRow("SELECT role FROM users WHERE username='second_user'").Scan(&role2)
	if role2 != "viewer" {
		t.Errorf("second user role = %q, want viewer", role2)
	}

	// Restore the unit-test invariant (db.DB nil for the claims path).
	db.DB = nil
}
