package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestDeleteBackupHonest — deleting a file returns deleted and removes it;
// a failed remove (non-empty directory) must NOT report success (QA round 3
// calibration: os.Remove error was ignored — false success).
func TestDeleteBackupHonest(t *testing.T) {
	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()

	saReq := func() *http.Request {
		r := httptest.NewRequest("DELETE", "/api/backups/proj-a/download/x", nil)
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
		r.SetPathValue("id", "proj-a")
		return r
	}

	// Happy path: real file removed, honest deleted.
	f := filepath.Join(baseDir, "proj-a_test.db")
	os.WriteFile(f, []byte("data"), 0600)
	w := httptest.NewRecorder()
	DeleteBackup(w, saReq(), "proj-a", "proj-a_test.db")
	if w.Code != 200 {
		t.Fatalf("delete = %d (body %s)", w.Code, w.Body.String())
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file still exists after delete")
	}

	// Failure path: a non-empty DIRECTORY passes safeBackupPath (name has the
	// project prefix) but os.Remove fails — the API must not say deleted.
	dir := filepath.Join(baseDir, "proj-a_dir.db")
	os.MkdirAll(filepath.Join(dir, "inner"), 0755)
	defer os.RemoveAll(dir)
	w2 := httptest.NewRecorder()
	DeleteBackup(w2, saReq(), "proj-a", "proj-a_dir.db")
	if w2.Code == 200 {
		t.Fatalf("delete of non-empty dir = 200 (body %s), want error", w2.Body.String())
	}

	// Traversal rejected before any filesystem access.
	w3 := httptest.NewRecorder()
	DeleteBackup(w3, saReq(), "proj-a", "../etc/passwd")
	if w3.Code != 404 {
		t.Errorf("traversal delete = %d, want 404", w3.Code)
	}
}

// TestSafeBackupPathMatrix — containment + project isolation: "app" must
// not reach "app2_*" backups and traversal must never escape baseDir.
func TestSafeBackupPathMatrix(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		filename string
		wantErr  bool
	}{
		{"own backup", "app", "app_2026.db", false},
		{"sibling project blocked", "app", "app2_2026.db", true},
		{"traversal blocked", "app", "../app_2026.db", true},
		{"absolute escape blocked", "app", "/etc/passwd", true},
		{"clean name without prefix blocked", "app", "other.db", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := safeBackupPath(c.id, c.filename)
			if (err != nil) != c.wantErr {
				t.Errorf("safeBackupPath(%q, %q) err = %v, wantErr %v", c.id, c.filename, err, c.wantErr)
			}
		})
	}
}

// TestCreateBackupPostgres — real pg_dump round: a project pointing at the
// docker postgres gets a .sql backup on disk (I-dimension: the postgres
// branch had zero execution record). Env-gated on LAMBS_TEST_PG_DSN + pg_dump.
func TestCreateBackupPostgres(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		t.Skip("pg_dump not present")
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
	// The project DSN targets the same postgres the fixture lives in —
	// pg_dump dumps a real database.
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('pg-proj', 'PG备份', $1)`, dsn)

	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()

	r := httptest.NewRequest("POST", "/api/backups/pg-proj", nil)
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
	w := httptest.NewRecorder()
	CreateBackup(w, r, "pg-proj")
	if w.Code != 200 {
		t.Fatalf("pg backup = %d (body %s)", w.Code, w.Body.String())
	}
	var bresp struct {
		Data struct {
			Ok       bool   `json:"ok"`
			Filename string `json:"filename"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &bresp)
	if !bresp.Data.Ok || !strings.HasSuffix(bresp.Data.Filename, ".sql") {
		t.Fatalf("pg backup result = %+v", bresp.Data)
	}
	fi, err := os.Stat(filepath.Join(baseDir, bresp.Data.Filename))
	if err != nil {
		t.Fatalf("pg backup file missing: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("pg backup file is empty")
	}
}

// TestRestoreBackupSQLiteRoundTrip — backup a sqlite project then restore
// the file into a second database and verify data lands (QA round 6 idea 5:
// the restore branch had zero execution).
func TestRestoreBackupSQLiteRoundTrip(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not present")
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

	// Source sqlite with real data.
	srcPath := filepath.Join(t.TempDir(), "src.db")
	out, err := exec.Command("sqlite3", srcPath, "CREATE TABLE t(v TEXT); INSERT INTO t VALUES('round-trip-data');").CombinedOutput()
	if err != nil {
		t.Fatalf("seed src: %s %v", out, err)
	}
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('rt-proj', '恢复测试', $1)`, "sqlite:///"+srcPath)

	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()

	sa := func(r *http.Request) {
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
	}

	// 1. Create the backup.
	cr := httptest.NewRequest("POST", "/api/backups/rt-proj", nil)
	sa(cr)
	cw := httptest.NewRecorder()
	CreateBackup(cw, cr, "rt-proj")
	var bresp struct {
		Data struct {
			Filename string `json:"filename"`
		} `json:"data"`
	}
	json.Unmarshal(cw.Body.Bytes(), &bresp)
	if bresp.Data.Filename == "" {
		t.Fatalf("backup failed: %s", cw.Body.String())
	}

	// 2. Restore into a SECOND sqlite file (dest db in the project row).
	destPath := filepath.Join(t.TempDir(), "dest.db")
	exec.Command("sqlite3", destPath, "CREATE TABLE t(v TEXT);").Run()
	mustExec(`UPDATE projects SET dsn=$1 WHERE id='rt-proj'`, "sqlite:///"+destPath)

	rr := httptest.NewRequest("POST", "/api/backups/rt-proj/restore/"+bresp.Data.Filename, nil)
	sa(rr)
	rr.SetPathValue("id", "rt-proj")
	rw := httptest.NewRecorder()
	RestoreBackup(rw, rr, "rt-proj", bresp.Data.Filename)
	if rw.Code != 200 {
		t.Fatalf("restore = %d (body %s)", rw.Code, rw.Body.String())
	}

	// 3. Data landed in dest.
	got, err := exec.Command("sqlite3", destPath, "SELECT v FROM t;").Output()
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if !strings.Contains(string(got), "round-trip-data") {
		t.Errorf("restored data missing: %q", got)
	}
}

// TestDownloadBackupContract — 403 without admin role, 404 for unknown
// backups, 200 + content when the file exists (safeBackupPath gate).
func TestDownloadBackupContract(t *testing.T) {
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
	mustExec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT, dsn TEXT, service_name TEXT)`)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('dl-proj','下载测试','—')`)

	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()

	// 403: no admin role.
	r := httptest.NewRequest("GET", "/api/backups/dl-proj/download/dl-proj_20260821.bak", nil)
	w := httptest.NewRecorder()
	DownloadBackup(w, r, "dl-proj", "dl-proj_20260821.bak")
	if w.Code != 403 {
		t.Errorf("no-role code = %d, want 403", w.Code)
	}

	// 404: backup file missing.
	r = httptest.NewRequest("GET", "/api/backups/dl-proj/download/nope.bak", nil)
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
	w = httptest.NewRecorder()
	DownloadBackup(w, r, "dl-proj", "nope.bak")
	if w.Code != 404 {
		t.Errorf("missing file code = %d, want 404", w.Code)
	}

	// 200: file exists and is served (backups live flat under baseDir,
	// the id is only a filename-prefix gate).
	if err := os.WriteFile(filepath.Join(baseDir, "dl-proj_20260821.bak"), []byte("backup-bytes"), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}
	r = httptest.NewRequest("GET", "/api/backups/dl-proj/download/dl-proj_20260821.bak", nil)
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
	w = httptest.NewRecorder()
	DownloadBackup(w, r, "dl-proj", "dl-proj_20260821.bak")
	if w.Code != 200 {
		t.Fatalf("download code = %d, want 200", w.Code)
	}
	if w.Body.String() != "backup-bytes" {
		t.Errorf("body = %q, want backup-bytes", w.Body.String())
	}
}

// TestRestoreBackupContract — 403 without admin, 400 for projects without
// an independent sqlite DB, 400 for non-sqlite DSNs. (The sqlite success
// path is covered by the round-trip test above.)
func TestRestoreBackupContract(t *testing.T) {
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
	mustExec(`CREATE TABLE projects (id TEXT PRIMARY KEY, name TEXT, dsn TEXT, service_name TEXT)`)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES
		('rs-proj-a','无独立库','—'), ('rs-proj-b','PG库',$1)`, dsn)
	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()

	post := func(id, file string, admin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/backups/"+id+"/restore/"+file, nil)
		if admin {
			r.Header.Set("X-User-ID", "admin")
			r.Header.Set("X-Role", "super_admin")
		}
		w := httptest.NewRecorder()
		RestoreBackup(w, r, id, file)
		return w
	}

	if w := post("rs-proj-a", "x.bak", false); w.Code != 403 {
		t.Errorf("no-role restore = %d, want 403", w.Code)
	}
	if w := post("rs-proj-a", "x.bak", true); w.Code != 400 {
		t.Errorf("dash-dsn restore = %d (body %s), want 400", w.Code, w.Body.String())
	}
	if w := post("rs-proj-b", "x.bak", true); w.Code != 400 {
		t.Errorf("pg-dsn restore = %d (body %s), want 400", w.Code, w.Body.String())
	}
}

// TestListBackups — prefix gate ("app" must not list "app2_*"), shape check,
// 403 without admin.
func TestListBackups(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if _, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT, dsn TEXT)`); err != nil {
		t.Fatalf("create: %v", err)
	}
	baseDir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", baseDir)
	defer os.Unsetenv("LAMBS_BACKUP_DIR")
	backupBaseDir = baseDir
	defer func() { backupBaseDir = "/home/ubuntu/lambs-backups" }()
	for _, f := range []string{"lb.bak", "lb_20260821.bak", "lb2_evil.bak", "other.txt"} {
		if err := os.WriteFile(filepath.Join(baseDir, f), []byte("x"), 0600); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	r := httptest.NewRequest("GET", "/api/backups/lb", nil)
	w := httptest.NewRecorder()
	ListBackups(w, r, "lb")
	if w.Code != 403 {
		t.Errorf("no-role = %d, want 403", w.Code)
	}

	r = httptest.NewRequest("GET", "/api/backups/lb", nil)
	r.Header.Set("X-Role", "super_admin")
	w = httptest.NewRecorder()
	ListBackups(w, r, "lb")
	if w.Code != 200 {
		t.Fatalf("list = %d", w.Code)
	}
	var env struct {
		Data struct {
			Backups []map[string]interface{} `json:"backups"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]bool{}
	for _, b := range env.Data.Backups {
		names[b["filename"].(string)] = true
	}
	// Backup filenames follow the id_<timestamp> convention; a bare name
	// matching exactly nothing and other-project prefixes must be excluded.
	if !names["lb_20260821.bak"] {
		t.Errorf("missing own backup: %v", names)
	}
	if names["lb.bak"] {
		t.Error("listed a non-conforming filename (lb.bak)")
	}
	if names["lb2_evil.bak"] {
		t.Error("listed another project's backup (lb2_evil)")
	}
}

// TestUploadBackupToTGNoAccess — no role header: 403 before any DB or TG
// work (route-matrix gap: upload-tg wrapper had zero coverage).
func TestUploadBackupToTGNoAccess(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/backups/p/upload-tg/p_20260821.bak", nil)
	w := httptest.NewRecorder()
	UploadBackupToTG(w, r, "p", "p_20260821.bak")
	if w.Code != 403 {
		t.Errorf("upload-tg = %d, want 403", w.Code)
	}
}

// TestUploadBackupToTGForeignFile — super_admin with a filename not owned by
// the project: safeBackupPath rejects it with 404 before any TG call.
func TestUploadBackupToTGForeignFile(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/backups/p/upload-tg/q_20260821.bak", nil)
	r.Header.Set("X-Role", "super_admin")
	w := httptest.NewRecorder()
	UploadBackupToTG(w, r, "p", "q_20260821.bak")
	if w.Code != 404 {
		t.Errorf("upload-tg = %d, want 404", w.Code)
	}
}
