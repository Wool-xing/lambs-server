package auth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func signToken(t *testing.T, uid, role string, exp time.Time) string {
	t.Helper()
	claims := jwt.MapClaims{"user_id": uid, "username": "u", "role": role, "exp": exp.Unix()}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(JWTKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

// TestRequireAuthOverwritesForgedHeaders — client-supplied X-User-ID /
// X-Role must never survive the middleware (QA round 2 calibration claimed
// a CRITICAL header-spoofing hole; verdict: Header.Set overwrites them —
// this test is the regression guard for that verdict).
func TestRequireAuthOverwritesForgedHeaders(t *testing.T) {
	old := JWTKey
	JWTKey = []byte("test-key")
	t.Cleanup(func() { JWTKey = old })
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, "u1", "viewer", time.Now().Add(time.Hour)))
	req.Header.Set("X-User-ID", "evil-admin")
	req.Header.Set("X-Role", "super_admin")
	rr := httptest.NewRecorder()
	RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-User-ID"); got != "u1" {
			t.Errorf("X-User-ID = %q, want u1 (forged header survived)", got)
		}
		if got := r.Header.Get("X-Role"); got != "viewer" {
			t.Errorf("X-Role = %q, want viewer (forged header survived)", got)
		}
		w.WriteHeader(200)
	})(rr, req)
	if rr.Code != 200 {
		t.Fatalf("code = %d, want 200", rr.Code)
	}
}

func TestRequireAuthRejectsMissingAndExpired(t *testing.T) {
	JWTKey = []byte("test-key")
	cases := []struct {
		name  string
		token string
	}{
		{"no authorization header", ""},
		{"not a bearer token", "Basic abc"},
		{"expired token", signToken(t, "u1", "viewer", time.Now().Add(-time.Hour))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/me", nil)
			if c.token != "" {
				req.Header.Set("Authorization", c.token)
			}
			rr := httptest.NewRecorder()
			RequireAuth(func(w http.ResponseWriter, r *http.Request) { t.Error("handler must not run") })(rr, req)
			if rr.Code != 401 {
				t.Errorf("code = %d, want 401", rr.Code)
			}
		})
	}
}

func TestRequireSuperAdminForbidsViewer(t *testing.T) {
	JWTKey = []byte("test-key")
	req := httptest.NewRequest("GET", "/api/users", nil)
	req.Header.Set("Authorization", "Bearer "+signToken(t, "u1", "viewer", time.Now().Add(time.Hour)))
	rr := httptest.NewRecorder()
	RequireSuperAdmin(func(w http.ResponseWriter, r *http.Request) { t.Error("handler must not run") })(rr, req)
	if rr.Code != 403 {
		t.Errorf("code = %d, want 403", rr.Code)
	}
}

func TestCORSPreflightAndHeaders(t *testing.T) {
	corsOrigin = "https://example.com" // env default is empty in unit tests
	// OPTIONS short-circuits with 200 + CORS headers, handler never runs.
	req := httptest.NewRequest("OPTIONS", "/api/me", nil)
	req.Header.Set("Origin", "https://example.com")
	rr := httptest.NewRecorder()
	CORS(func(w http.ResponseWriter, r *http.Request) { t.Error("handler must not run for OPTIONS") })(rr, req)
	if rr.Code != 200 {
		t.Errorf("preflight code = %d, want 200", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "https://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q", rr.Header().Get("Access-Control-Allow-Origin"))
	}
	for _, h := range []string{"Access-Control-Allow-Methods", "Access-Control-Allow-Headers"} {
		if rr.Header().Get(h) == "" {
			t.Errorf("missing %s on preflight", h)
		}
	}
	req2 := httptest.NewRequest("GET", "/api/me", nil)
	rr2 := httptest.NewRecorder()
	called := false
	CORS(func(w http.ResponseWriter, r *http.Request) { called = true })(rr2, req2)
	if !called {
		t.Error("GET did not pass through CORS")
	}
}

func TestJSONErrShape(t *testing.T) {
	rr := httptest.NewRecorder()
	JSONErr(rr, 403, "无权操作")
	var body struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, rr.Body.String())
	}
	if body.Success || body.Error != "无权操作" || rr.Code != 403 {
		t.Errorf("got %+v code=%d, want success=false error=无权操作 code=403", body, rr.Code)
	}
}

func TestCodeMACAndRandomCode(t *testing.T) {
	a, b := codeMAC("code-1"), codeMAC("code-2")
	if a == "" || a == b || len(a) != 64 {
		t.Errorf("codeMAC outputs = %q %q", a, b)
	}
	m1, m2 := codeMAC("x"), codeMAC("x")
	if m1 != m2 {
		t.Error("codeMAC not deterministic")
	}
	c1, err := randomCode()
	if err != nil || len(c1) != 6 {
		t.Errorf("randomCode = %q, %v; want 6 chars", c1, err)
	}
	c2, _ := randomCode()
	if c1 == c2 {
		t.Error("randomCode collision on two draws (suspicious)")
	}
}

func TestHandleForgotRequestValidation(t *testing.T) {
	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"missing username", map[string]string{"email": "a@b.c"}, 400},
		{"missing email", map[string]string{"username": "u"}, 400},
		{"empty body", map[string]string{}, 400},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, _ := json.Marshal(c.body)
			r := httptest.NewRequest("POST", "/api/auth/forgot-password/request", bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			HandleForgotRequest(w, r)
			if w.Code != c.want {
				t.Errorf("code = %d, want %d (body %s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}
