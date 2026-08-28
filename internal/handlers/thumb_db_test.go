package handlers

import (
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestEnsureThumbsAndBackfill — env-gated on LAMBS_TEST_PG_DSN: the startup
// schema pass is idempotent, and the backfill fills icon_thumb from a legacy
// base64 icon while leaving existing thumbs untouched (guarded UPDATE).
func TestEnsureThumbsAndBackfill(t *testing.T) {
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

	EnsureThumbs() // idempotent schema pass — running twice must not fail
	EnsureThumbs()

	// Full schema, not a minimal fixture — downstream packages (nginx,
	// runtime) INSERT into the same shared table and expect every column
	// (QA round 8 CI lesson: a 2-column recreate broke them at 42703).
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
	icon := pngDataURL(t, 200, 100, false)
	mustExec(`INSERT INTO projects (id, name, icon_url, icon_thumb) VALUES
		('thumb-legacy', 'legacy', $1, NULL),
		('thumb-set', 'set', $1, 'data:image/png;base64,existing')`, icon)

	EnsureThumbsBackfill()

	var legacyThumb, setThumb string
	if err := db.DB.QueryRow("SELECT icon_thumb FROM projects WHERE id='thumb-legacy'").Scan(&legacyThumb); err != nil {
		t.Fatalf("legacy scan: %v", err)
	}
	if err := db.DB.QueryRow("SELECT icon_thumb FROM projects WHERE id='thumb-set'").Scan(&setThumb); err != nil {
		t.Fatalf("set scan: %v", err)
	}
	if legacyThumb == "" || len(legacyThumb) < 40 {
		t.Errorf("legacy icon_thumb not backfilled: %q", legacyThumb)
	}
	if setThumb != "data:image/png;base64,existing" {
		t.Errorf("existing thumb overwritten: %q", setThumb)
	}
}

// TestEnsureThumbsBackfillUsers — the users branch of EnsureThumbsBackfill:
// legacy avatar data URLs get avatar_thumb filled, existing thumbs stay
// untouched (guarded UPDATE), plain URL avatars are never touched, and the
// literal 'null' avatar_url is normalized to '' by EnsureThumbs.
func TestEnsureThumbsBackfillUsers(t *testing.T) {
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
	// Full users schema, matching the smoke fixture — EnsureThumbs ALTERs the
	// table, so it must exist with every column downstream code expects.
	mustExec(`DROP TABLE IF EXISTS users CASCADE`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT now())`)

	av := pngDataURL(t, 200, 100, false)
	mustExec(`INSERT INTO users (id, username, email, avatar_url, avatar_thumb) VALUES
		('11111111-1111-1111-1111-111111111111', 'u-legacy', 'u1@x.y', $1, NULL),
		('22222222-2222-2222-2222-222222222222', 'u-set', 'u2@x.y', $1, 'data:image/png;base64,existing'),
		('33333333-3333-3333-3333-333333333333', 'u-plain', 'u3@x.y', 'https://cdn.x.y/a.png', NULL),
		('44444444-4444-4444-4444-444444444444', 'u-null', 'u4@x.y', 'null', NULL)`, av)

	EnsureThumbs() // adds avatar_thumb if missing + normalizes 'null' avatars
	EnsureThumbsBackfill()

	var thumb, avURL string
	if err := db.DB.QueryRow("SELECT avatar_thumb FROM users WHERE id='11111111-1111-1111-1111-111111111111'").Scan(&thumb); err != nil {
		t.Fatalf("legacy scan: %v", err)
	}
	if thumb == "" || len(thumb) < 40 {
		t.Errorf("legacy avatar_thumb not backfilled: %q", thumb)
	}
	if err := db.DB.QueryRow("SELECT avatar_thumb FROM users WHERE id='22222222-2222-2222-2222-222222222222'").Scan(&thumb); err != nil {
		t.Fatalf("set scan: %v", err)
	}
	if thumb != "data:image/png;base64,existing" {
		t.Errorf("existing avatar_thumb overwritten: %q", thumb)
	}
	if err := db.DB.QueryRow("SELECT COALESCE(avatar_thumb,'') FROM users WHERE id='33333333-3333-3333-3333-333333333333'").Scan(&thumb); err != nil {
		t.Fatalf("plain scan: %v", err)
	}
	if thumb != "" {
		t.Errorf("plain URL avatar got a thumb: %q", thumb)
	}
	if err := db.DB.QueryRow("SELECT avatar_url FROM users WHERE id='44444444-4444-4444-4444-444444444444'").Scan(&avURL); err != nil {
		t.Fatalf("null scan: %v", err)
	}
	if avURL != "" {
		t.Errorf("literal 'null' avatar_url not normalized: %q", avURL)
	}
}

// TestProjectLogoHappyPaths — the red-flag branches from the full-round
// function map (QA 全量轮红标 #1): thumb served, full=1 original served
// with SVG sandbox CSP, no-thumb fallback to icon, and corrupt data 404.
func TestProjectLogoHappyPaths(t *testing.T) {
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
		services JSONB DEFAULT '[]')`)

	// 1x1 red PNG and a minimal SVG, base64.
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	svg := "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciLz4="

	cases := []struct {
		name, id, icon, thumb, query, wantCT, wantCSP string
		wantCode                                       int
	}{
		{"thumb served", "logo-a", png, png, "", "image/png", "", 200},
		{"full original svg sandbox", "logo-b", svg, png, "?full=1", "image/svg+xml", "sandbox", 200},
		{"no thumb fallback to icon", "logo-c", png, "", "", "image/png", "", 200},
		{"corrupt data 404", "logo-d", "data:image/png;base64,!!!", "", "", "", "", 404},
	}
	for _, c := range cases {
		mustExec(`INSERT INTO projects (id, name, icon_url, icon_thumb) VALUES ($1,$2,$3,$4)`,
			c.id, c.name, c.icon, c.thumb)
		req := httptest.NewRequest("GET", "/api/projects/"+c.id+"/logo"+c.query, nil)
		w := httptest.NewRecorder()
		ProjectLogo(w, req, c.id)
		if w.Code != c.wantCode {
			t.Errorf("%s: code = %d, want %d", c.name, w.Code, c.wantCode)
			continue
		}
		if c.wantCode == 200 {
			if got := w.Header().Get("Content-Type"); got != c.wantCT {
				t.Errorf("%s: Content-Type = %q, want %q", c.name, got, c.wantCT)
			}
			if got := w.Header().Get("Content-Security-Policy"); got != c.wantCSP {
				t.Errorf("%s: CSP = %q, want %q", c.name, got, c.wantCSP)
			}
			if w.Body.Len() == 0 {
				t.Errorf("%s: empty body", c.name)
			}
		}
		mustExec(`DELETE FROM projects WHERE id=$1`, c.id)
	}
}
