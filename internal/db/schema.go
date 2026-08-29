package db

// EnsureCoreSchema creates the four core tables idempotently. Production
// deployments historically relied on hand-created tables — a fresh database
// would fail on first login/register (2026-08-29 open-source audit).
// Idempotent: existing installs are untouched.
func EnsureCoreSchema() {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
			password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
			token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
			project_access JSONB NOT NULL DEFAULT '[]',
			avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
			last_login TIMESTAMPTZ DEFAULT now(),
			created_at TIMESTAMPTZ DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS projects (
			id TEXT PRIMARY KEY, name TEXT, repo TEXT, description TEXT, icon_url TEXT,
			icon_thumb TEXT, stack TEXT, port TEXT, db_type TEXT, dsn TEXT,
			users_count INT DEFAULT 0, status TEXT DEFAULT 'online', sort_order INT DEFAULT 0,
			is_pinned BOOLEAN DEFAULT false, icon_cls TEXT, base_path TEXT, backend_url TEXT,
			service_name TEXT, startup_command TEXT, health_url TEXT,
			tags JSONB DEFAULT '[]', offline_msg TEXT, features JSONB DEFAULT '[]',
			tabs JSONB DEFAULT '[]', datasources JSONB DEFAULT '[]', services JSONB DEFAULT '[]',
			created_at TIMESTAMPTZ DEFAULT now(), updated_at TIMESTAMPTZ DEFAULT now(),
			backup_interval_hours INT DEFAULT 0, backup_retention_days INT DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS notifications (
			id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT,
			content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS audit_logs (
			id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT,
			target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`,
	}
	for _, s := range stmts {
		if _, err := DB.Exec(s); err != nil {
			// Log only: an existing healthy install must keep booting even if
			// one statement hits a transient issue; the failing feature will
			// surface loudly on use.
			println("EnsureCoreSchema:", err.Error())
		}
	}
}
