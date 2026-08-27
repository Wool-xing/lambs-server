package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/db"
)

// extraDBSetup gates on LAMBS_TEST_PG_DSN, inits the pool, ensures the users +
// audit_logs fixtures exist, clears users, and restores the unit-test
// invariant (db.DB nil) on cleanup. Returns a mustExec helper.
func extraDBSetup(t *testing.T) func(q string, args ...interface{}) {
	t.Helper()
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { db.DB = nil })
	mustExec := func(q string, args ...interface{}) {
		t.Helper()
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
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`DELETE FROM users`)
	return mustExec
}

// signTokenTV signs an HS256 token with an optional tv claim (nil = omitted).
// Distinct from auth_more_test.go's signToken, which never sets tv.
func signTokenTV(t *testing.T, uid, role string, tv interface{}, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{"user_id": uid, "username": "u", "role": role, "exp": exp.Unix()}
	if tv != nil {
		claims["tv"] = tv
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(JWTKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// TestRequireAuthRealDBBranches — the db.DB != nil path of RequireAuth: fresh
// role/status/token_version from the DB gate every request. Role from the DB
// must override the claim, disabled accounts 401, stale or missing tv 401,
// unknown user 401 (fail-closed).
func TestRequireAuthRealDBBranches(t *testing.T) {
	mustExec := extraDBSetup(t)
	oldKey := JWTKey
	JWTKey = []byte("test-key")
	t.Cleanup(func() { JWTKey = oldKey })

	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, token_version, project_access)
		VALUES ('dddddddd-0000-0000-0000-000000000001','req_active','u','req_a@t.c','x','viewer','active',1,'[]')`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, token_version, project_access)
		VALUES ('dddddddd-0000-0000-0000-000000000002','req_disabled','u','req_d@t.c','x','viewer','disabled',0,'[]')`)

	call := func(tokenStr string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Header.Set("Authorization", "Bearer "+tokenStr)
		w := httptest.NewRecorder()
		RequireAuth(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Result", r.Header.Get("X-Role")+"|"+r.Header.Get("X-User-ID"))
			w.WriteHeader(200)
		})(w, r)
		return w
	}
	valid := time.Now().Add(time.Hour)
	activeID := "dddddddd-0000-0000-0000-000000000001"

	// Claim role super_admin is overridden by the DB row's viewer.
	w := call(signTokenTV(t, activeID, "super_admin", 1, valid))
	if w.Code != 200 || w.Header().Get("X-Result") != "viewer|"+activeID {
		t.Errorf("active user = %d result %q, want 200 viewer|%s", w.Code, w.Header().Get("X-Result"), activeID)
	}
	if w := call(signTokenTV(t, "dddddddd-0000-0000-0000-000000000002", "viewer", 0, valid)); w.Code != 401 {
		t.Errorf("disabled user = %d, want 401", w.Code)
	}
	if w := call(signTokenTV(t, activeID, "viewer", 99, valid)); w.Code != 401 {
		t.Errorf("stale tv = %d, want 401", w.Code)
	}
	if w := call(signTokenTV(t, activeID, "viewer", nil, valid)); w.Code != 401 {
		t.Errorf("missing tv = %d, want 401", w.Code)
	}
	if w := call(signTokenTV(t, "dddddddd-0000-0000-0000-000000000099", "viewer", 0, valid)); w.Code != 401 {
		t.Errorf("unknown user = %d, want 401", w.Code)
	}
}

// TestRequireSuperAdminAcceptsSuperAdmin — the success path of the role gate
// (TestRequireSuperAdminForbidsViewer covers the 403).
func TestRequireSuperAdminAcceptsSuperAdmin(t *testing.T) {
	JWTKey = []byte("test-key")
	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+signTokenTV(t, "u1", "super_admin", nil, time.Now().Add(time.Hour)))
	rr := httptest.NewRecorder()
	called := false
	RequireSuperAdmin(func(w http.ResponseWriter, r *http.Request) { called = true })(rr, req)
	if rr.Code != 200 || !called {
		t.Errorf("super_admin token = %d called=%v, want 200 true", rr.Code, called)
	}
}

// TestWithAuthAndWithAdmin — the one-line wrapper middleware: preflight
// short-circuits, valid tokens pass, and WithAdmin enforces the role.
func TestWithAuthAndWithAdmin(t *testing.T) {
	JWTKey = []byte("test-key")

	req := httptest.NewRequest("OPTIONS", "/api/me", nil)
	rr := httptest.NewRecorder()
	WithAuth(func(w http.ResponseWriter, r *http.Request) { t.Error("handler must not run on preflight") })(rr, req)
	if rr.Code != 200 {
		t.Errorf("WithAuth preflight = %d, want 200", rr.Code)
	}

	req = httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+signTokenTV(t, "u1", "viewer", nil, time.Now().Add(time.Hour)))
	rr = httptest.NewRecorder()
	called := false
	WithAuth(func(w http.ResponseWriter, r *http.Request) { called = true })(rr, req)
	if rr.Code != 200 || !called {
		t.Errorf("WithAuth valid = %d called=%v, want 200 true", rr.Code, called)
	}

	req = httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+signTokenTV(t, "u1", "viewer", nil, time.Now().Add(time.Hour)))
	rr = httptest.NewRecorder()
	WithAdmin(func(w http.ResponseWriter, r *http.Request) { t.Error("viewer must not reach admin handler") })(rr, req)
	if rr.Code != 403 {
		t.Errorf("WithAdmin viewer = %d, want 403", rr.Code)
	}

	req = httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+signTokenTV(t, "u1", "super_admin", nil, time.Now().Add(time.Hour)))
	rr = httptest.NewRecorder()
	called = false
	WithAdmin(func(w http.ResponseWriter, r *http.Request) { called = true })(rr, req)
	if rr.Code != 200 || !called {
		t.Errorf("WithAdmin super_admin = %d called=%v, want 200 true", rr.Code, called)
	}
}

// TestHandleRegisterValidation — every pre-DB rejection path of the
// registration pipeline (no PostgreSQL needed: all return before the query).
func TestHandleRegisterValidation(t *testing.T) {
	longName := strings.Repeat("u", 65)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{`, 400},
		{"empty username", `{"username":"","email":"a@b.c","password":"secret123"}`, 400},
		{"long username", `{"username":"` + longName + `","email":"a@b.c","password":"secret123"}`, 400},
		{"bad username chars", `{"username":"bad user!","email":"a@b.c","password":"secret123"}`, 400},
		{"bad email", `{"username":"okuser","email":"a@b","password":"secret123"}`, 400},
		{"short password", `{"username":"okuser","email":"a@b.c","password":"12345"}`, 400},
		{"bad salt", `{"username":"okuser","email":"a@b.c","password":"secret123","salt":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(c.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			HandleRegister(w, r)
			if w.Code != c.want {
				t.Errorf("code = %d, want %d (body %s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

// TestHandleRegisterSaltedAndDuplicate — R7 salted registration stores the
// client-computed sha256(payload) with the provided salt; a duplicate
// username is rejected 400 after the hash is built.
func TestHandleRegisterSaltedAndDuplicate(t *testing.T) {
	mustExec := extraDBSetup(t)
	oldKey := JWTKey
	JWTKey = []byte("test-key")
	t.Cleanup(func() { JWTKey = oldKey })
	t.Setenv("LAMBS_ALLOW_REGISTER", "")
	// Seed one user so the new registration is a viewer, not super_admin.
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access)
		VALUES ('aaaaaaaa-0000-0000-0000-000000000000','seed','seed','seed@t.c','x','viewer','active','[]')`)

	salt := strings.Repeat("a1", 16) // 32-char lowercase hex
	payload := sha256Hex("secret123" + salt)
	body := `{"username":"salted_user","email":"salted@t.c","password":"` + payload + `","salt":"` + salt + `"}`

	req := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandleRegister(w, req)
	if w.Code != 201 {
		t.Fatalf("salted register = %d (body %s)", w.Code, w.Body.String())
	}
	var gotSalt, gotRole string
	db.DB.QueryRow("SELECT pwd_salt, role FROM users WHERE username='salted_user'").Scan(&gotSalt, &gotRole)
	if gotSalt != salt || gotRole != "viewer" {
		t.Errorf("stored salt=%q role=%q, want %q viewer", gotSalt, gotRole, salt)
	}

	req = httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	HandleRegister(w, req)
	if w.Code != 400 {
		t.Errorf("duplicate register = %d, want 400 (body %s)", w.Code, w.Body.String())
	}
}

// TestHandleMePasswordBadJSON — decode failure short-circuits before any DB
// access.
func TestHandleMePasswordBadJSON(t *testing.T) {
	r := httptest.NewRequest("PUT", "/api/auth/me/password", strings.NewReader(`{`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	HandleMePassword(w, r)
	if w.Code != 400 {
		t.Errorf("bad json = %d, want 400", w.Code)
	}
}

// TestHandleMePasswordLegacyNoSalt — an account without pwd_salt (old data)
// gets a fresh salt generated and its hash re-wrapped on password change;
// the new password verifies against the new salt.
func TestHandleMePasswordLegacyNoSalt(t *testing.T) {
	mustExec := extraDBSetup(t)
	h, err := bcrypt.GenerateFromPassword([]byte(sha256Hex("oldpass-1")), bcrypt.DefaultCost)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access)
		VALUES ('bbbbbbbb-0000-0000-0000-000000000000','legacy_user','u','legacy@t.c',$1,'viewer','active','[]')`, string(h))

	req := httptest.NewRequest("PUT", "/api/auth/me/password", strings.NewReader(`{"old":"oldpass-1","new":"newpass-9"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "bbbbbbbb-0000-0000-0000-000000000000")
	w := httptest.NewRecorder()
	HandleMePassword(w, req)
	if w.Code != 200 {
		t.Fatalf("legacy change = %d (body %s)", w.Code, w.Body.String())
	}
	var hash, salt string
	db.DB.QueryRow("SELECT password_hash, COALESCE(pwd_salt,'') FROM users WHERE id='bbbbbbbb-0000-0000-0000-000000000000'").Scan(&hash, &salt)
	if salt == "" {
		t.Error("legacy account still has no salt after password change")
	}
	if ok, _ := VerifyPassword(hash, "newpass-9", salt); !ok {
		t.Error("new password does not verify against the generated salt")
	}
}

// TestHandleMe — /api/me returns the user row (avatar set or NULL), and an
// unknown id 404s.
func TestHandleMe(t *testing.T) {
	mustExec := extraDBSetup(t)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access, avatar_url)
		VALUES ('cccccccc-0000-0000-0000-000000000001','me_no_avatar','u','me1@t.c','x','viewer','active','["p1"]',NULL)`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, project_access, avatar_url)
		VALUES ('cccccccc-0000-0000-0000-000000000002','me_avatar','u','me2@t.c','x','viewer','active','["p2"]','a.png')`)

	call := func(userID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/me", nil)
		r.Header.Set("X-User-ID", userID)
		w := httptest.NewRecorder()
		HandleMe(w, r)
		return w
	}

	w := call("cccccccc-0000-0000-0000-000000000001")
	if w.Code != 200 {
		t.Fatalf("me no-avatar = %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data["username"] != "me_no_avatar" || body.Data["project_access"] != `["p1"]` {
		t.Errorf("data = %v", body.Data)
	}
	if av, ok := body.Data["avatar_url"].(string); !ok || av != "" {
		t.Errorf("NULL avatar_url = %v, want empty string", body.Data["avatar_url"])
	}

	w = call("cccccccc-0000-0000-0000-000000000002")
	var body2 struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &body2)
	if body2.Data["avatar_url"] != "a.png" {
		t.Errorf("avatar_url = %v, want a.png", body2.Data["avatar_url"])
	}

	if w := call("cccccccc-0000-0000-0000-000000000099"); w.Code != 404 {
		t.Errorf("unknown user = %d, want 404", w.Code)
	}
}
