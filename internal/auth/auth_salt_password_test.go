package auth

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/db"
)

func saltPwdSetup(t *testing.T) {
	t.Helper()
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.DB = nil }) // restore unit-test invariant
	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`DELETE FROM users WHERE username='salt-user'`)
	// Seed with the register-pipeline shape: bcrypt(sha256(old+salt)).
	h, err := bcrypt.GenerateFromPassword([]byte(sha256Hex("oldpass-1"+"salt-1")), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, pwd_salt)
		VALUES ('aaaaaaaa-2222-3333-4444-555555555555','salt-user','盐','s@t.st',$1,'viewer','salt-1')`, string(h))
}

// TestHandleSalt — known user returns their salt; unknown user returns an
// empty salt with 200 (no username enumeration via error codes).
func TestHandleSalt(t *testing.T) {
	saltPwdSetup(t)
	req := httptest.NewRequest("GET", "/api/auth/salt?username=salt-user", nil)
	w := httptest.NewRecorder()
	HandleSalt(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"salt":"salt-1"`) {
		t.Errorf("known = %d (%s)", w.Code, w.Body.String())
	}

	req = httptest.NewRequest("GET", "/api/auth/salt?username=no-such-user", nil)
	w = httptest.NewRecorder()
	HandleSalt(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"salt":""`) {
		t.Errorf("unknown = %d (%s)", w.Code, w.Body.String())
	}
}

// TestHandleMePassword — short new password 400, unknown user 404, wrong old
// password 400, success replaces the hash with a verifiable new one.
func TestHandleMePassword(t *testing.T) {
	saltPwdSetup(t)

	call := func(body, userID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("PUT", "/api/auth/me/password", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-User-ID", userID)
		w := httptest.NewRecorder()
		HandleMePassword(w, req)
		return w
	}
	id := "aaaaaaaa-2222-3333-4444-555555555555"

	if w := call(`{"old":"x","new":"123"}`, id); w.Code != 400 {
		t.Errorf("short = %d, want 400", w.Code)
	}
	if w := call(`{"old":"x","new":"newpass-9"}`, "00000000-0000-0000-0000-000000000000"); w.Code != 404 {
		t.Errorf("unknown user = %d, want 404", w.Code)
	}
	if w := call(`{"old":"wrong-old","new":"newpass-9"}`, id); w.Code != 400 {
		t.Errorf("wrong old = %d, want 400", w.Code)
	}

	if w := call(`{"old":"oldpass-1","new":"newpass-9"}`, id); w.Code != 200 {
		t.Fatalf("change = %d (%s)", w.Code, w.Body.String())
	}
	var hash string
	db.DB.QueryRow("SELECT password_hash FROM users WHERE id=$1", id).Scan(&hash)
	ok, _ := VerifyPassword(hash, "newpass-9", "salt-1")
	if !ok {
		t.Error("new password does not verify after change")
	}
}
