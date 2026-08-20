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
