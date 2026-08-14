package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestRequireAuthRejectsNonHS256(t *testing.T) {
	JWTKey = []byte("test-key")
	// Token signed with HS384 but the same key — must be rejected (alg confusion defense).
	claims := jwt.MapClaims{
		"user_id":  "u1",
		"username": "admin",
		"role":     "super_admin",
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS384, claims)
	s, err := tok.SignedString(JWTKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+s)
	rr := httptest.NewRecorder()
	RequireAuth(func(w http.ResponseWriter, r *http.Request) {})(rr, req)
	if rr.Code != 401 {
		t.Fatalf("expected 401 for HS384 token, got %d", rr.Code)
	}
}

func TestRateLimitBlocksAfterMax(t *testing.T) {
	loginLimiter.hits = make(map[string][]time.Time) // reset state
	limited := RateLimit(5, time.Minute)
	called := 0
	h := limited(func(w http.ResponseWriter, r *http.Request) { called++ })
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest("POST", "/api/auth/login", nil)
		req.RemoteAddr = "10.0.0.1:1234"
		h(httptest.NewRecorder(), req)
	}
	if called != 5 {
		t.Fatalf("expected 5 calls allowed, got %d", called)
	}
	req := httptest.NewRequest("POST", "/api/auth/login", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != 429 || called != 5 {
		t.Fatalf("expected 429 on 6th request, got %d called=%d", rr.Code, called)
	}
	// Different IP is not affected
	req2 := httptest.NewRequest("POST", "/api/auth/login", nil)
	req2.RemoteAddr = "10.0.0.2:1234"
	h(httptest.NewRecorder(), req2)
	if called != 6 {
		t.Fatalf("expected other IP to pass, called=%d", called)
	}
}

func TestRequireAuthAcceptsHS256(t *testing.T) {
	JWTKey = []byte("test-key")
	claims := jwt.MapClaims{
		"user_id":  "u1",
		"username": "admin",
		"role":     "super_admin",
		"exp":      time.Now().Add(time.Hour).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	s, err := tok.SignedString(JWTKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	req := httptest.NewRequest("GET", "/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+s)
	rr := httptest.NewRecorder()
	called := false
	RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})(rr, req)
	if rr.Code != 200 || !called {
		t.Fatalf("expected HS256 token to pass, got %d called=%v", rr.Code, called)
	}
}
