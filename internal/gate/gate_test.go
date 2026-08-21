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

// TestGatePathMatches — the block check must match exact path, slash-child,
// and query-suffixed paths, but NOT sibling prefixes (a base_path "/a" must
// not block "/ab": the error body leaks the existence of a blocked project).
func TestGatePathMatches(t *testing.T) {
	cases := []struct {
		path string
		bp   string
		want bool
	}{
		{"/a", "/a", true},
		{"/a/b", "/a", true},
		{"/a?x=1", "/a", true},
		{"/ab", "/a", false}, // sibling prefix must not match
		{"/a", "/ab", false},
		{"/a", "/b", false},
		{"", "/a", false},
	}
	for _, c := range cases {
		if got := gatePathMatches(c.path, c.bp); got != c.want {
			t.Errorf("gatePathMatches(%q, %q) = %v, want %v", c.path, c.bp, got, c.want)
		}
	}
}

// TestEscapeLike — LIKE wildcards in client-controlled paths must be
// neutralized so "%_" cannot match another project's row.
func TestEscapeLike(t *testing.T) {
	cases := []struct{ in, want string }{
		{"abc", "abc"},
		{"a_b", `a\_b`},
		{"100%", `100\%`},
		{`a\b`, `a\\b`},
		{"_%\\", `\_\%\\`},
	}
	for _, c := range cases {
		if got := escapeLike(c.in); got != c.want {
			t.Errorf("escapeLike(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestHandleCheckIntegration — real PostgreSQL verification, gated on
// LAMBS_TEST_PG_DSN. Offline/maintenance projects block their own paths;
// sibling prefixes and unrelated paths pass through.
func TestHandleCheckIntegration(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS projects;`)
	mustExec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT, base_path TEXT, status TEXT)`)
	mustExec(`INSERT INTO projects (id, name, base_path, status) VALUES
		('p1', 'offline proj', '/off', 'offline'),
		('p2', 'maint proj', '/maint', 'maintenance'),
		('p3', 'online proj', '/ok', 'online')`)

	cases := []struct {
		name     string
		path     string
		wantCode int
	}{
		{"offline own path blocked", "/off", 403},
		{"offline child path blocked", "/off/sub", 403},
		{"sibling prefix passes (no leak)", "/off2", 200},
		{"unrelated path passes", "/other", 200},
		{"maintenance blocked", "/maint", 403},
		{"online project passes", "/ok", 200},
		{"root passes", "/", 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/gate/check?path="+c.path, nil)
			w := httptest.NewRecorder()
			HandleCheck(w, r)
			if w.Code != c.wantCode {
				t.Errorf("code = %d, want %d (body %s)", w.Code, c.wantCode, w.Body.String())
			}
		})
	}
}

// TestHandleOfflinePageDefault — no matching project row: the page still
// renders branded defaults (never a 500), and hostile cookie values never
// reach the CSS (whitelist).
func TestHandleOfflinePageDefault(t *testing.T) {
	// Lazy connection that never pings — QueryRow fails and the handler
	// falls back to branded defaults (the contract under test).
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	db.DB = tdb
	defer func() { db.DB = nil }()

	// The hostile value is fully URL-encoded: raw ';' splits the Cookie
	// header and raw '"' gets stripped by cookie parsing — either way the
	// payload never reached the CSS whitelist and the assertion was passing
	// on the wrong defense (mutation-testing the whitelist exposed this).
	r := httptest.NewRequest("GET", "/api/gate/offline-page", nil)
	r.Header.Set("X-Original-URI", "/some-project/page")
	r.AddCookie(&http.Cookie{Name: "lambs_theme_accent", Value: url.QueryEscape(`{"Accent":"red;}</style><script>alert(1)</script>","AccentBg":"x","Border":"y"}`)})
	w := httptest.NewRecorder()
	HandleOfflinePage(w, r)
	if w.Code != 200 {
		t.Fatalf("offline page = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "项目") {
		t.Errorf("body missing branding: %s", body[:120])
	}
	if strings.Contains(body, "alert(1)") || strings.Contains(body, "};</style>") {
		t.Errorf("hostile cookie value leaked into page: %s", body[:300])
	}
}

// TestHandleProjectLogoMissingRow — no matching project: 404 via the lazy
// connection (route-matrix gap: this endpoint had zero route-level coverage).
func TestHandleProjectLogoMissingRow(t *testing.T) {
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	old := db.DB
	db.DB = tdb
	t.Cleanup(func() { db.DB = old })

	r := httptest.NewRequest("GET", "/api/gate/project-logo?path=/nope", nil)
	w := httptest.NewRecorder()
	HandleProjectLogo(w, r)
	if w.Code != 404 {
		t.Errorf("project-logo = %d, want 404", w.Code)
	}
}

// TestHandleCheckInternalNoAuth — nginx auth_request loopback: same
// contract as HandleCheck with no auth required. The empty-path fast path
// short-circuits before any DB access, so no test DB is needed.
func TestHandleCheckInternalNoAuth(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/gate/check-internal", nil)
	w := httptest.NewRecorder()
	HandleCheckInternal(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"allowed":true`) {
		t.Errorf("check-internal = %d %s", w.Code, w.Body.String())
	}
}
