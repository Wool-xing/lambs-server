package handlers

import (
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
