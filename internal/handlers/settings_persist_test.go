package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
)

// TestUpdateConfigPersist — safe fields land in the config file (LAMBS_CONFIG_PATH
// → temp file) and secret values from the existing file survive the rewrite;
// the in-memory config is updated in place.
func TestUpdateConfigPersist(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "lambs_config.json")
	if err := os.WriteFile(cfgPath, []byte(`{"jwt_secret":"file-jwt","smtp_password":"file-smtp","admin_email":"old@x.y","port":3602}`), 0600); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	t.Setenv("LAMBS_CONFIG_PATH", cfgPath)
	cfg := &models.Config{JWTSecret: "file-jwt", SMTPPassword: "file-smtp", AdminEmail: "old@x.y", Port: 3602}

	b, _ := json.Marshal(map[string]interface{}{
		"jwt_secret": "attacker-jwt", "smtp_password": "attacker-smtp",
		"admin_email": "new@x.y", "port": 4100,
	})
	r := httptest.NewRequest("PUT", "/api/settings/config", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	UpdateConfig(w, r, cfg)
	if w.Code != 200 {
		t.Fatalf("update = %d (body %s)", w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Saved string `json:"saved"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Data.Saved != "ok" {
		t.Errorf("saved = %q, want ok", body.Data.Saved)
	}
	// In-memory config applied.
	if cfg.AdminEmail != "new@x.y" || cfg.Port != 4100 {
		t.Errorf("cfg after update = %+v", cfg)
	}
	// File rewritten: new safe fields + preserved secrets, no attacker secrets.
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	content := string(raw)
	for _, want := range []string{`"admin_email": "new@x.y"`, `"port": 4100`, `"jwt_secret": "file-jwt"`, `"smtp_password": "file-smtp"`} {
		if !strings.Contains(content, want) {
			t.Errorf("config file missing %s: %s", want, content)
		}
	}
	if strings.Contains(content, "attacker") {
		t.Errorf("attacker secrets leaked into config file: %s", content)
	}
}

// TestUpdateConfigPersistFailure — an unwritable config path yields a clean
// 500 JSON error, never a success.
func TestUpdateConfigPersistFailure(t *testing.T) {
	// The parent directory does not exist → os.WriteFile must fail.
	t.Setenv("LAMBS_CONFIG_PATH", filepath.Join(t.TempDir(), "no-such-dir", "lambs_config.json"))
	cfg := &models.Config{}

	b, _ := json.Marshal(map[string]interface{}{"admin_email": "x@y.z", "port": 4100})
	r := httptest.NewRequest("PUT", "/api/settings/config", strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	UpdateConfig(w, r, cfg)
	if w.Code != 500 {
		t.Fatalf("update = %d (body %s), want 500", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "配置保存失败") {
		t.Errorf("body missing error message: %s", w.Body.String())
	}
}

// brokenDB points db.DB at an unreachable postgres — Query/QueryRow fail at
// the row level, the degraded path every CSV exporter must survive.
func brokenDB(t *testing.T) {
	t.Helper()
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	old := db.DB
	db.DB = tdb
	t.Cleanup(func() { db.DB = old })
}

// TestExportCSVErrorPaths — DB down: ExportProjects/ExportUsers answer a
// clean JSON error (never a CSV body with BOM+header mixed with JSON, R3-P2);
// ExportProjectUsers degrades to an empty CSV envelope without JSON noise.
func TestExportCSVErrorPaths(t *testing.T) {
	brokenDB(t)

	for _, tc := range []struct {
		name string
		do   func(w http.ResponseWriter)
	}{
		{"projects", func(w http.ResponseWriter) { ExportProjects(w, httptest.NewRequest("GET", "/", nil)) }},
		{"users", func(w http.ResponseWriter) { ExportUsers(w, httptest.NewRequest("GET", "/", nil)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			tc.do(w)
			if w.Code != 500 {
				t.Errorf("code = %d, want 500 (body %s)", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); strings.Contains(ct, "csv") {
				t.Errorf("content-type = %q, want JSON error (not CSV)", ct)
			}
			if !strings.Contains(w.Body.String(), "导出失败") {
				t.Errorf("body missing JSON error: %s", w.Body.String())
			}
		})
	}

	// ExportProjectUsers: the project query fails → dsn stays empty → the
	// sqlite branch is skipped and SyncUserData("") yields nothing — the wire
	// stays a clean CSV envelope (BOM only), no JSON mixed in.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/", nil)
	ExportProjectUsers(w, r, "no-such-proj")
	if w.Code != 200 {
		t.Errorf("project-users code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); !strings.HasPrefix(body, "\uFEFF") || strings.Contains(body, `"success"`) {
		t.Errorf("project-users body = %q, want BOM-only CSV envelope", body)
	}
}
