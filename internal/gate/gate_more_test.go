package gate

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// gateFixture creates the full projects schema (same DDL as the runtime
// package's fixtures — round-9 CI lesson) and returns mustExec.
func gateFixture(t *testing.T) func(string, ...interface{}) {
	t.Helper()
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
	mustExec(`DROP TABLE IF EXISTS projects CASCADE`)
	mustExec(`CREATE TABLE projects (
		id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT,
		icon_thumb TEXT, stack TEXT, port TEXT, db_type TEXT, dsn TEXT, users_count INT DEFAULT 0,
		status TEXT DEFAULT 'online', sort_order INT DEFAULT 0, is_pinned BOOLEAN DEFAULT false,
		icon_cls TEXT, base_path TEXT, backend_url TEXT, service_name TEXT,
		startup_command TEXT, health_url TEXT, tags JSONB DEFAULT '[]', offline_msg TEXT,
		features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]',
		services JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now(),
		updated_at TIMESTAMPTZ DEFAULT now(),
		backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`)
	return mustExec
}

// TestHandleProjectLogoIntegration — env-gated PG: the logo endpoint serves
// a resolvable data-URL icon with the right headers (CSP sandbox only for
// SVG), 404s on unresolvable or absent icons, and matches with or without
// the leading slash.
func TestHandleProjectLogoIntegration(t *testing.T) {
	mustExec := gateFixture(t)
	mustExec(`INSERT INTO projects (id, name, icon_url, base_path) VALUES
		('logo-svg', 'svg',  'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=', '/svg'),
		('logo-png', 'png',  'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==', '/png'),
		('logo-raw', 'raw',  'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg"/>', '/raw'),
		('logo-http', 'http', 'http://example.com/x.png', '/http'),
		('logo-plain', 'plain', 'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=', 'plain-path')`)

	cases := []struct {
		name      string
		path      string
		wantCode  int
		wantCT    string
		wantCSP   bool
		wantCache bool
	}{
		{"svg served with sandbox CSP", "/svg", 200, "image/svg+xml", true, true},
		{"slashed query matches unslashed base_path", "/plain-path", 200, "image/svg+xml", true, true},
		{"unslashed query misses slashed row", "svg", 404, "", false, false},
		{"png served without CSP", "/png", 200, "image/png", false, true},
		{"raw svg served", "/raw", 200, "image/svg+xml", true, true},
		{"non-data url 404s", "/http", 404, "", false, false},
		{"missing row 404s", "/nope", 404, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/gate/project-logo?path="+c.path, nil)
			w := httptest.NewRecorder()
			HandleProjectLogo(w, r)
			if w.Code != c.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, c.wantCode)
			}
			if w.Code != 200 {
				return
			}
			if ct := w.Header().Get("Content-Type"); ct != c.wantCT {
				t.Errorf("Content-Type = %q, want %q", ct, c.wantCT)
			}
			if _, has := w.Header()["Content-Security-Policy"]; has != c.wantCSP {
				t.Errorf("CSP header present = %v, want %v", has, c.wantCSP)
			}
			if cc := w.Header().Get("Cache-Control"); (cc != "") != c.wantCache {
				t.Errorf("Cache-Control = %q", cc)
			}
		})
	}
}

// TestHandleOfflinePageIntegration — env-gated PG: a matching project row
// renders name/default messages by status, the favicon tags when the icon
// is an inline data URL, and whitelisted theme cookies flow into the CSS.
// Wildcard paths fall back to the generic page.
func TestHandleOfflinePageIntegration(t *testing.T) {
	mustExec := gateFixture(t)
	mustExec(`INSERT INTO projects (id, name, icon_url, base_path, offline_msg, status) VALUES
		('off-maint', '维护项目', 'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=', '/maint', NULL, 'maintenance'),
		('off-down', '停用项目', 'data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=', '/down', '管理员自定义消息', 'offline'),
		('off-plain', '普通项目', NULL, '/plain', NULL, 'offline')`)

	// Maintenance row with valid theme cookies.
	r := httptest.NewRequest("GET", "/api/gate/offline-page", nil)
	r.Header.Set("X-Original-URI", "/maint/page")
	r.AddCookie(&http.Cookie{Name: "lambs_theme_accent", Value: url.QueryEscape(`{"Accent":"#FF0000","AccentBg":"rgba(0,255,0,.10)","Border":"#0000FF"}`)})
	r.AddCookie(&http.Cookie{Name: "lambs_theme_grad", Value: url.QueryEscape(`linear-gradient(red,blue)`)})
	r.AddCookie(&http.Cookie{Name: "lambs_theme_glass", Value: url.QueryEscape(`{"Alpha":0.9,"Blur":30}`)})
	w := httptest.NewRecorder()
	HandleOfflinePage(w, r)
	if w.Code != 200 {
		t.Fatalf("maint page = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"维护项目", "该项目正在维护中，请稍后再试。", "维护中",
		"/maint/favicon.svg", "color:#FF0000", "rgba(0,255,0,.10)",
		"border:1px solid #0000FF", "linear-gradient(red,blue)",
		"rgba(17,21,28,0.90)", "blur(30px)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("maint page missing %q", want)
		}
	}

	// Offline row with an explicit message (defaults skipped).
	r2 := httptest.NewRequest("GET", "/api/gate/offline-page", nil)
	r2.Header.Set("X-Original-URI", "/down/x")
	w2 := httptest.NewRecorder()
	HandleOfflinePage(w2, r2)
	body2 := w2.Body.String()
	for _, want := range []string{"停用项目", "管理员自定义消息", "已停用"} {
		if !strings.Contains(body2, want) {
			t.Errorf("down page missing %q", want)
		}
	}

	// No icon: no favicon references in the page at all.
	r3 := httptest.NewRequest("GET", "/api/gate/offline-page", nil)
	r3.Header.Set("X-Original-URI", "/plain/y")
	w3 := httptest.NewRecorder()
	HandleOfflinePage(w3, r3)
	if strings.Contains(w3.Body.String(), "favicon") {
		t.Errorf("plain page must not reference a favicon: %s", w3.Body.String()[:200])
	}

	// LIKE wildcards in the path match nothing → branded defaults.
	r4 := httptest.NewRequest("GET", "/api/gate/offline-page", nil)
	r4.Header.Set("X-Original-URI", "/%%/z")
	w4 := httptest.NewRecorder()
	HandleOfflinePage(w4, r4)
	body4 := w4.Body.String()
	if !strings.Contains(body4, "Project") || !strings.Contains(body4, "不可用") {
		t.Errorf("wildcard path = %s, want default page", body4[:200])
	}
}

// TestHandleCheckDBDown — the database-unavailable gate answers 503, never
// allows a path through on a lie.
func TestHandleCheckDBDown(t *testing.T) {
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	old := db.DB
	db.DB = tdb
	t.Cleanup(func() { db.DB = old })

	r := httptest.NewRequest("GET", "/api/gate/check?path=/x", nil)
	w := httptest.NewRecorder()
	HandleCheck(w, r)
	if w.Code != 503 || !strings.Contains(w.Body.String(), "database unavailable") {
		t.Errorf("check = %d %s, want 503 database unavailable", w.Code, w.Body.String())
	}
}
