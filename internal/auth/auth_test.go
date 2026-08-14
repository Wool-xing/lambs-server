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
