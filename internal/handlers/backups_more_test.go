package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// sqlite3Bin resolves the sqlite3 CLI (env override first, then PATH) and
// skips when unavailable.
func sqlite3Bin(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("LAMBS_SQLITE3_PATH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("sqlite3"); err == nil {
		return p
	}
	t.Skip("sqlite3 CLI not found — set LAMBS_SQLITE3_PATH to run")
	return ""
}

func swapBackupDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := backupBaseDir
	backupBaseDir = dir
	t.Cleanup(func() { backupBaseDir = old })
	return dir
}

func backupsFixture(t *testing.T) func(string, ...interface{}) {
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

func saRequest(t *testing.T, method, target string, body string) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("X-Role", "super_admin")
	r.Header.Set("X-User-ID", "admin")
	return httptest.NewRecorder(), r
}

// TestDoBackupSQLite — real sqlite3 CLI backup of a temp source db:
// ok:true + file on disk + size_mb shape.
func TestDoBackupSQLite(t *testing.T) {
	bin := sqlite3Bin(t)
	dir := swapBackupDir(t)
	src := filepath.Join(t.TempDir(), "src.db")
	cmd := exec.Command(bin, src, "CREATE TABLE t(id INTEGER PRIMARY KEY, name TEXT); INSERT INTO t(name) VALUES ('x');")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v (%s)", err, out)
	}
	result := doBackup("proj-sql", "sqlite:///"+filepath.ToSlash(src)+"?mode=rw")
	if result["ok"] != true {
		t.Fatalf("sqlite backup = %v", result)
	}
	fname, _ := result["filename"].(string)
	if _, err := os.Stat(filepath.Join(dir, fname)); err != nil {
		t.Fatalf("backup file missing: %v", err)
	}
	if _, ok := result["size_mb"].(float64); !ok {
		t.Fatalf("size_mb shape = %T", result["size_mb"])
	}
}

// TestDoBackupUnsupported — non-sqlite/postgres DSNs fail honestly.
func TestDoBackupUnsupported(t *testing.T) {
	swapBackupDir(t)
	result := doBackup("proj-redis", "redis://127.0.0.1:6380/0")
	if result["ok"] != false || result["error"] != "unsupported db type" {
		t.Fatalf("unsupported = %v", result)
	}
}

// TestDoBackupPostgresNoDump — with pg_dump absent the postgres branch
// reports failure honestly; with pg_dump present it must succeed.
func TestDoBackupPostgresNoDump(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	dir := swapBackupDir(t)
	_, dumpErr := exec.LookPath("pg_dump")
	result := doBackup("proj-pg", dsn)
	if dumpErr != nil {
		if result["ok"] != false {
			t.Fatalf("no pg_dump: backup = %v, want failure", result)
		}
	} else {
		if result["ok"] != true {
			t.Fatalf("pg_dump present: backup = %v, want success", result)
		}
		fname, _ := result["filename"].(string)
		if _, err := os.Stat(filepath.Join(dir, fname)); err != nil {
			t.Fatalf("backup file missing: %v", err)
		}
	}
}

// TestRunScheduledBackups — due sqlite project backs up, retention removes
// expired files, TG upload degrades to a log line when unconfigured.
func TestRunScheduledBackups(t *testing.T) {
	bin := sqlite3Bin(t)
	mustExec := backupsFixture(t)
	dir := swapBackupDir(t)

	src := filepath.Join(t.TempDir(), "sched.db")
	cmd := exec.Command(bin, src, "CREATE TABLE t(id INTEGER PRIMARY KEY);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v (%s)", err, out)
	}
	// expired backup that retention must remove
	stale := filepath.Join(dir, "proj-sched_20000101-000000.db")
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatalf("stale file: %v", err)
	}
	old := time.Now().AddDate(0, 0, -3)
	os.Chtimes(stale, old, old)

	mustExec(`INSERT INTO projects (id, name, dsn, backup_interval_hours, backup_retention_days) VALUES ('proj-sched','调度备份项目',$1,1,1)`, "sqlite:///"+filepath.ToSlash(src))
	defer mustExec(`DELETE FROM projects WHERE id='proj-sched'`)

	RunScheduledBackups()

	// a fresh backup file must exist
	entries, _ := os.ReadDir(dir)
	fresh := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "proj-sched_") && e.Name() != filepath.Base(stale) {
			fresh = true
		}
	}
	if !fresh {
		t.Fatalf("no fresh backup produced; dir = %v", entries)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("expired backup not removed by retention")
	}
	// second run: fresh backup is within interval → no new backup (no error)
	RunScheduledBackups()
}

// TestRestoreBackupGuards — non-sqlite 400, missing file 404, corrupt file 500.
func TestRestoreBackupGuards(t *testing.T) {
	bin := sqlite3Bin(t)
	mustExec := backupsFixture(t)
	dir := swapBackupDir(t)

	// project with a postgres dsn → 400 仅支持恢复 SQLite
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('proj-pg2','PG项目','postgres://u:p@127.0.0.1:5433/db')`)
	defer mustExec(`DELETE FROM projects WHERE id='proj-pg2'`)
	w, r := saRequest(t, "POST", "/api/backups/proj-pg2/restore/x.db", "")
	RestoreBackup(w, r, "proj-pg2", "x.db")
	if w.Code != 400 {
		t.Fatalf("non-sqlite restore = %d, want 400", w.Code)
	}

	// sqlite project, missing backup file → 404
	src := filepath.Join(t.TempDir(), "r.db")
	cmd := exec.Command(bin, src, "CREATE TABLE t(id INTEGER PRIMARY KEY);")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v (%s)", err, out)
	}
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('proj-sq','SQLite项目','sqlite:///` + filepath.ToSlash(src) + `')`)
	defer mustExec(`DELETE FROM projects WHERE id='proj-sq'`)
	w, r = saRequest(t, "POST", "/api/backups/proj-sq/restore/missing.db", "")
	RestoreBackup(w, r, "proj-sq", "missing.db")
	if w.Code != 404 {
		t.Fatalf("missing file restore = %d, want 404", w.Code)
	}

	// corrupt backup file → sqlite .backup fails → 500
	corrupt := filepath.Join(dir, "proj-sq_corrupt.db")
	os.WriteFile(corrupt, []byte("not a sqlite db"), 0600)
	w, r = saRequest(t, "POST", "/api/backups/proj-sq/restore/proj-sq_corrupt.db", "")
	RestoreBackup(w, r, "proj-sq", "proj-sq_corrupt.db")
	if w.Code != 500 {
		t.Fatalf("corrupt restore = %d, want 500", w.Code)
	}
}

// TestUploadBackupToTGNotConfigured — TG secrets empty → 500 (real upload
// needs the Telegram channel config, covered by tgbackup package tests).
func TestUploadBackupToTGNotConfigured(t *testing.T) {
	backupsFixture(t)
	dir := swapBackupDir(t)
	f := filepath.Join(dir, "proj-tg_backup.db")
	if err := os.WriteFile(f, []byte("x"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	w, r := saRequest(t, "POST", "/api/backups/proj-tg/upload/proj-tg_backup.db", "")
	UploadBackupToTG(w, r, "proj-tg", "proj-tg_backup.db")
	if w.Code != 500 {
		t.Fatalf("tg upload = %d, want 500", w.Code)
	}
}
