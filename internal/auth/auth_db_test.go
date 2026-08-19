package auth

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/db"
)

// TestHandleLoginRealDB — real PostgreSQL: correct password issues a token,
// wrong password 401s, disabled account 403s. Gated on LAMBS_TEST_PG_DSN.
func TestHandleLoginRealDB(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`DELETE FROM users`)
	JWTKey = []byte("test-key")

	// Seed one active + one disabled user with the register-pipeline hash
	// shape: bcrypt(sha256(plaintext)).
	seed := func(username, status string) {
		h, err := bcrypt.GenerateFromPassword([]byte(sha256Hex("secret123")), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access)
			VALUES (gen_random_uuid(), $1, $1, $1||'@t.c', $2, 'viewer', $3, '[]')`, username, string(h), status)
	}
	seed("active_user", "active")
	seed("disabled_user", "disabled")

	login := func(username, password string) (int, map[string]interface{}) {
		b, _ := json.Marshal(map[string]string{"username": username, "password": password})
		r := httptest.NewRequest("POST", "/api/auth/login", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		HandleLogin(w, r)
		var body map[string]interface{}
		json.Unmarshal(w.Body.Bytes(), &body)
		return w.Code, body
	}

	code, body := login("active_user", "secret123")
	if code != 200 {
		t.Fatalf("correct password = %d (body %v)", code, body)
	}
	if d, ok := body["data"].(map[string]interface{}); !ok || d["access_token"] == "" {
		t.Errorf("no access_token in %v", body)
	}

	if code, _ := login("active_user", "wrongpass"); code != 401 {
		t.Errorf("wrong password = %d, want 401", code)
	}
	if code, _ := login("no_such_user", "secret123"); code != 401 {
		t.Errorf("unknown user = %d, want 401", code)
	}
	if code, _ := login("disabled_user", "secret123"); code != 403 {
		t.Errorf("disabled user = %d, want 403", code)
	}

	// Restore the unit-test invariant: with db.DB nil, RequireAuth follows
	// the JWT-claims path (auth_test.go / auth_more_test.go depend on it).
	// This file sorts before them, so cleanup must land before they run.
	db.DB = nil
}
