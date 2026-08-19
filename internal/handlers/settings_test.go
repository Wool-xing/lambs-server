package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"lambs-server-go/internal/models"
)

// TestGetConfigNeverLeaksSecrets — R17 regression guard: the sanitized copy
// must never carry jwt_secret or smtp_password to the frontend (QA round 3
// test idea 2).
func TestGetConfigNeverLeaksSecrets(t *testing.T) {
	cfg := &models.Config{
		JWTSecret:    "super-secret-jwt",
		SMTPPassword: "super-secret-smtp",
		AdminEmail:   "admin@lambs.local",
		Port:         3602,
		RefreshInt:   30,
	}
	r := httptest.NewRequest("GET", "/api/settings/config", nil)
	w := httptest.NewRecorder()
	GetConfig(w, r, cfg)
	if w.Code != 200 {
		t.Fatalf("code = %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Success bool `json:"success"`
		Data    struct {
			JWTSecret    string `json:"jwt_secret"`
			SMTPPassword string `json:"smtp_password"`
			AdminEmail   string `json:"admin_email"`
			Port         int    `json:"port"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v (body %s)", err, w.Body.String())
	}
	if body.Data.JWTSecret != "" {
		t.Errorf("jwt_secret leaked: %q", body.Data.JWTSecret)
	}
	if body.Data.SMTPPassword != "" {
		t.Errorf("smtp_password leaked: %q", body.Data.SMTPPassword)
	}
	if body.Data.AdminEmail != "admin@lambs.local" || body.Data.Port != 3602 {
		t.Errorf("safe fields mangled: %+v", body.Data)
	}
	// The caller's config must be untouched (sanitize on copy, not in place).
	if cfg.JWTSecret != "super-secret-jwt" || cfg.SMTPPassword != "super-secret-smtp" {
		t.Error("GetConfig mutated the caller's config")
	}
}

// TestUpdateConfigIgnoresAPISecrets — secrets sent by the client are dropped.
func TestUpdateConfigIgnoresAPISecrets(t *testing.T) {
	cfg := &models.Config{JWTSecret: "env-jwt", SMTPPassword: "env-smtp", AdminEmail: "old@x.y"}
	b, _ := json.Marshal(map[string]interface{}{
		"jwt_secret": "attacker-jwt", "smtp_password": "attacker-smtp",
		"admin_email": "new@x.y", "port": 3999,
	})
	r := httptest.NewRequest("PUT", "/api/settings/config", bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	UpdateConfig(w, r, cfg)
	// Persistence writes to LAMBS_CONFIG_PATH — leave unset: the write will
	// fail (default path likely unwritable) but secret handling happens
	// before persistence, so assert on the in-memory state regardless.
	if cfg.JWTSecret != "" && cfg.JWTSecret != "env-jwt" {
		t.Errorf("JWTSecret overwritten by API: %q", cfg.JWTSecret)
	}
	if cfg.AdminEmail != "new@x.y" {
		t.Errorf("AdminEmail = %q, want new@x.y", cfg.AdminEmail)
	}
}
