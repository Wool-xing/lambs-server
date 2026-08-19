package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// ensureUsersFixture creates the users table shape the handlers expect
// (real PostgreSQL, LAMBS_TEST_PG_DSN-gated) without dropping tables other
// tests in this package may rely on.
func ensureUsersFixture(t *testing.T) {
	t.Helper()
	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	// users_test runs last in this package (alphabetical), so a full rebuild
	// is safe — earlier tests create a 2-column users fixture.
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
	mustExec(`DELETE FROM users`)
}

func postJSON(t *testing.T, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "admin-uid")
	r.Header.Set("X-Role", "super_admin")
	w := httptest.NewRecorder()
	CreateUser(w, r)
	return w
}

func TestUsersCRUD(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	ensureUsersFixture(t)

	// CreateUser
	w := postJSON(t, "/api/users", map[string]interface{}{
		"username": "newuser", "name": "新用户", "email": "new@test.com",
		"password": "secret123", "role": "viewer",
	})
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("create = %d (body %s)", w.Code, w.Body.String())
	}
	var uid string
	db.DB.QueryRow("SELECT id::text FROM users WHERE username='newuser'").Scan(&uid)
	if uid == "" {
		t.Fatal("user row not persisted")
	}
	var hash string
	db.DB.QueryRow("SELECT password_hash FROM users WHERE id=$1", uid).Scan(&hash)
	if hash == "" {
		t.Error("password_hash empty — password not hashed")
	}

	// ListUsers includes the new row
	r := httptest.NewRequest("GET", "/api/users", nil)
	r.Header.Set("X-User-ID", "admin-uid")
	r.Header.Set("X-Role", "super_admin")
	lw := httptest.NewRecorder()
	ListUsers(lw, r)
	var list struct {
		Success bool `json:"success"`
		Data    struct {
			Users []map[string]interface{} `json:"users"`
		} `json:"data"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, lw.Body.String())
	}
	found := false
	for _, u := range list.Data.Users {
		if u["username"] == "newuser" {
			found = true
		}
	}
	if !found {
		t.Errorf("ListUsers missing newuser: %v", list.Data.Users)
	}

	// UpdateUser renames
	ub, _ := json.Marshal(map[string]interface{}{"username": "newuser", "name": "改名后", "email": "new@test.com", "role": "viewer", "status": "active", "project_access": "[]", "avatar_url": "", "avatar_thumb": ""})
	ur := httptest.NewRequest("PUT", "/api/users/"+uid, bytes.NewReader(ub))
	ur.Header.Set("Content-Type", "application/json")
	ur.Header.Set("X-User-ID", "admin-uid")
	ur.Header.Set("X-Role", "super_admin")
	uw := httptest.NewRecorder()
	UpdateUser(uw, ur, uid)
	if uw.Code != 200 {
		t.Fatalf("update = %d (body %s)", uw.Code, uw.Body.String())
	}
	var name string
	db.DB.QueryRow("SELECT name FROM users WHERE id=$1", uid).Scan(&name)
	if name != "改名后" {
		t.Errorf("name = %q, want 改名后", name)
	}

	// DeleteUser then 404 on repeat
	dr := httptest.NewRequest("DELETE", "/api/users/"+uid, nil)
	dr.Header.Set("X-User-ID", "admin-uid")
	dr.Header.Set("X-Role", "super_admin")
	dw := httptest.NewRecorder()
	DeleteUser(dw, dr, uid)
	if dw.Code != 200 {
		t.Fatalf("delete = %d (body %s)", dw.Code, dw.Body.String())
	}
	dr2 := httptest.NewRequest("DELETE", "/api/users/"+uid, nil)
	dr2.Header.Set("X-User-ID", "admin-uid")
	dr2.Header.Set("X-Role", "super_admin")
	dw2 := httptest.NewRecorder()
	DeleteUser(dw2, dr2, uid)
	if dw2.Code != 404 {
		t.Errorf("repeat delete = %d, want 404", dw2.Code)
	}
}
