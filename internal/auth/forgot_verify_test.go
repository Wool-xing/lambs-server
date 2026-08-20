package auth

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestForgotVerifyRealDB — the reset-code flow end to end: valid code
// resets the password (and bumps token_version), wrong code 400s, expired
// code 400s. Real postgres, gated on LAMBS_TEST_PG_DSN.
func TestForgotVerifyRealDB(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	JWTKey = []byte("test-key")
	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS users CASCADE; DROP TABLE IF EXISTS verification_codes CASCADE;`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	EnsureForgotSchema()
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role) VALUES (gen_random_uuid(),'forgot-user','忘记密码用户','fu@t.c','x','viewer')`)

	// Valid code row (MAC stored, expires in 10 min).
	code := "123456"
	mustExec(`INSERT INTO verification_codes (username, email, code, used, expires_at) VALUES ('forgot-user','fu@t.c',$1,FALSE,NOW()+INTERVAL '10 minutes')`, codeMAC(code))
	// Expired code row.
	mustExec(`INSERT INTO verification_codes (username, email, code, used, expires_at) VALUES ('forgot-user','fu@t.c',$1,TRUE,NOW()-INTERVAL '1 minute')`, codeMAC("654321"))

	verify := func(username, email, code, newPwd string) (int, string) {
		b, _ := json.Marshal(map[string]string{"username": username, "email": email, "code": code, "new_password": sha256Hex(newPwd)})
		r := httptest.NewRequest("POST", "/api/auth/forgot-password/verify", bytes.NewReader(b))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		HandleForgotVerify(w, r)
		return w.Code, w.Body.String()
	}

	code2, body := verify("forgot-user", "fu@t.c", code, "newsecret")
	if code2 != 200 {
		t.Fatalf("valid verify = %d (%s)", code2, body)
	}
	var tv int
	db.DB.QueryRow("SELECT token_version FROM users WHERE username='forgot-user'").Scan(&tv)
	if tv != 1 {
		t.Errorf("token_version = %d, want 1 (old tokens must die)", tv)
	}

	// Wrong code 400s.
	if c, _ := verify("forgot-user", "fu@t.c", "000000", "newsecret"); c != 400 {
		t.Errorf("wrong code = %d, want 400", c)
	}
	// Expired/used code 400s.
	if c, _ := verify("forgot-user", "fu@t.c", "654321", "newsecret"); c != 400 {
		t.Errorf("expired code = %d, want 400", c)
	}

	db.DB = nil // restore unit-test invariant
}
