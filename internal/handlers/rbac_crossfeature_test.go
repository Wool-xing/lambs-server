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
	"lambs-server-go/internal/runtime"
)

// backupDirForTest redirects backup storage to a temp dir for the test.
func backupDirForTest(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("LAMBS_BACKUP_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("LAMBS_BACKUP_DIR") })
	backupBaseDir = dir
	t.Cleanup(func() { backupBaseDir = "/home/ubuntu/lambs-backups" })
	return dir
}

func saHeader(r *http.Request) {
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
}

// TestDisableProjectKeepsTasksBackups — a disabled (offline) project keeps
// its scheduled tasks, backups stay listable AND creatable, and the status
// flip itself lands one notification (the notification semantics are the
// intended contract — a status change is always announced).
func TestDisableProjectKeepsTasksBackups(t *testing.T) {
	mustExec := puFixture(t)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not present")
	}
	mustExec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`DELETE FROM scheduled_tasks; DELETE FROM notifications`)

	src := filepath.ToSlash(filepath.Join(t.TempDir(), "dk.db"))
	if out, err := exec.Command("sqlite3", filepath.FromSlash(src), "CREATE TABLE t(v TEXT);").CombinedOutput(); err != nil {
		t.Fatalf("seed sqlite: %s %v", out, err)
	}
	mustExec(`INSERT INTO projects (id, name, dsn, status) VALUES ('dk-p','停用测试','sqlite:///`+src+`','online')`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host, enabled) VALUES ('dk-t','dk-p','保留任务','*/5 * * * *','echo hi','app1',true)`)
	backupDirForTest(t)

	patch := httptest.NewRequest("PATCH", "/api/projects/dk-p/status", strings.NewReader(`{"status":"offline"}`))
	patch.Header.Set("Content-Type", "application/json")
	saHeader(patch)
	pw := httptest.NewRecorder()
	PatchProjectStatus(pw, patch, "dk-p")
	if pw.Code != 200 {
		t.Fatalf("disable = %d (body %s)", pw.Code, pw.Body.String())
	}

	var status string
	db.DB.QueryRow("SELECT status FROM projects WHERE id='dk-p'").Scan(&status)
	if status != "offline" {
		t.Errorf("status = %q, want offline", status)
	}

	// Scheduled task survives the disable.
	var n, enabled int
	db.DB.QueryRow("SELECT COUNT(*), COALESCE(SUM(CASE WHEN enabled THEN 1 ELSE 0 END),0) FROM scheduled_tasks WHERE project_id='dk-p'").Scan(&n, &enabled)
	if n != 1 || enabled != 1 {
		t.Errorf("tasks after disable = %d (enabled %d), want 1/1", n, enabled)
	}

	// The status change landed exactly one notification (announcement).
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='dk-p' AND type='status'").Scan(&n)
	if n != 1 {
		t.Errorf("status notifications = %d, want 1", n)
	}

	// Backups still listable.
	lr := httptest.NewRequest("GET", "/api/backups/dk-p", nil)
	saHeader(lr)
	lw := httptest.NewRecorder()
	ListBackups(lw, lr, "dk-p")
	if lw.Code != 200 {
		t.Fatalf("list backups after disable = %d (body %s)", lw.Code, lw.Body.String())
	}

	// Backups still creatable while offline.
	cr := httptest.NewRequest("POST", "/api/backups/dk-p", nil)
	saHeader(cr)
	cw := httptest.NewRecorder()
	CreateBackup(cw, cr, "dk-p")
	if cw.Code != 200 {
		t.Fatalf("create backup after disable = %d (body %s)", cw.Code, cw.Body.String())
	}
	var bresp struct {
		Data struct {
			Filename string `json:"filename"`
			OK       bool   `json:"ok"`
		} `json:"data"`
	}
	json.Unmarshal(cw.Body.Bytes(), &bresp)
	if !bresp.Data.OK || bresp.Data.Filename == "" {
		t.Errorf("backup result = %+v, want ok+filename", bresp.Data)
	}
}

// TestDeleteProjectResidue — documents what survives a project delete:
// notifications and scheduled tasks keep their rows and the member
// relationship (users.project_access) is untouched. The delete handler only
// removes the project row and detaches runtime state — residue is the
// current implementation intent, pinned here so a future cascade cleanup
// would be a visible change.
func TestDeleteProjectResidue(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
		cron TEXT NOT NULL, command TEXT NOT NULL, host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true, created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ, last_status TEXT NOT NULL DEFAULT '', last_log TEXT NOT NULL DEFAULT '')`)
	mustExec(`DELETE FROM scheduled_tasks; DELETE FROM notifications; DELETE FROM audit_logs`)
	mustExec(`INSERT INTO projects (id, name, repo, dsn) VALUES ('dr-p','删除','dr-p','—')`)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000040','dr-pa','project_admin','["dr-p"]'),
		('10000000-0000-0000-0000-000000000041','dr-v','viewer','["dr-p"]')`)
	mustExec(`INSERT INTO notifications (id, project_id, type, title) VALUES
		('dr-n1','dr-p','alert','残留1'),('dr-n2','dr-p','status','残留2'),('dr-n3','','system','全局')`)
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command) VALUES ('dr-t','dr-p','残留任务','*/5 * * * *','echo')`)

	r := httptest.NewRequest("DELETE", "/api/projects/dr-p", nil)
	saHeader(r)
	w := httptest.NewRecorder()
	DeleteProject(w, r, "dr-p")
	if w.Code != 200 {
		t.Fatalf("delete = %d (body %s)", w.Code, w.Body.String())
	}

	// Project row gone.
	var cnt int
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE id='dr-p'").Scan(&cnt)
	if cnt != 0 {
		t.Errorf("project row remains: %d", cnt)
	}
	// Notifications residue: all 3 still present.
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='dr-p'").Scan(&cnt)
	if cnt != 2 {
		t.Errorf("project notifications after delete = %d, want 2 (residue)", cnt)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications").Scan(&cnt)
	if cnt != 3 {
		t.Errorf("total notifications after delete = %d, want 3 (residue)", cnt)
	}
	// Scheduled tasks residue.
	db.DB.QueryRow("SELECT COUNT(*) FROM scheduled_tasks WHERE project_id='dr-p'").Scan(&cnt)
	if cnt != 1 {
		t.Errorf("tasks after delete = %d, want 1 (residue)", cnt)
	}
	// Member relationship residue: project_access arrays untouched.
	var access string
	db.DB.QueryRow("SELECT project_access::text FROM users WHERE id='10000000-0000-0000-0000-000000000040'").Scan(&access)
	if access != `["dr-p"]` {
		t.Errorf("member access after delete = %s, want [\"dr-p\"] (residue)", access)
	}
	// Delete is audited.
	db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs WHERE action='删除项目' AND target='dr-p'").Scan(&cnt)
	if cnt != 1 {
		t.Errorf("delete audit rows = %d, want 1", cnt)
	}
}

// TestDeleteUserResidue — deleting a user leaves their audit trail rows and
// any project notifications intact, and does not rewrite other users'
// project_access (no dangling-pointer cleanup). Pins the current behavior.
func TestDeleteUserResidue(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`DELETE FROM notifications; DELETE FROM audit_logs`)
	mustExec(`INSERT INTO users (id, username, name, email, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000050','dr-u','受害者','victim@t.c','viewer','["proj-a"]'),
		('10000000-0000-0000-0000-000000000051','dr-other','他人','other@t.c','project_admin','["proj-a"]')`)
	mustExec(`INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES
		('10000000-0000-0000-0000-000000000050','dr-u','登录','dr-u','登录成功'),
		('10000000-0000-0000-0000-000000000051','dr-other','登录','dr-other','登录成功')`)
	mustExec(`INSERT INTO notifications (id, project_id, type, title) VALUES ('du-n1','proj-a','alert','项目通知')`)

	r := httptest.NewRequest("DELETE", "/api/users/10000000-0000-0000-0000-000000000050", nil)
	saHeader(r)
	w := httptest.NewRecorder()
	DeleteUser(w, r, "10000000-0000-0000-0000-000000000050")
	if w.Code != 200 {
		t.Fatalf("delete user = %d (body %s)", w.Code, w.Body.String())
	}

	var cnt int
	db.DB.QueryRow("SELECT COUNT(*) FROM users WHERE id='10000000-0000-0000-0000-000000000050'").Scan(&cnt)
	if cnt != 0 {
		t.Errorf("user row remains: %d", cnt)
	}
	// Audit trail residue: the deleted user's rows stay (append-only log).
	db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs WHERE user_id='10000000-0000-0000-0000-000000000050'").Scan(&cnt)
	if cnt != 1 {
		t.Errorf("deleted user audit rows = %d, want 1 (residue)", cnt)
	}
	// Notification residue: untouched.
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications").Scan(&cnt)
	if cnt != 1 {
		t.Errorf("notifications after user delete = %d, want 1 (residue)", cnt)
	}
	// Other users' project_access unchanged.
	var access string
	db.DB.QueryRow("SELECT project_access::text FROM users WHERE id='10000000-0000-0000-0000-000000000051'").Scan(&access)
	if access != `["proj-a"]` {
		t.Errorf("other user access = %s, want unchanged", access)
	}
	// Delete is audited.
	db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs WHERE action='删除用户' AND target='dr-u'").Scan(&cnt)
	if cnt != 1 {
		t.Errorf("delete-user audit rows = %d, want 1", cnt)
	}
}

// TestClonePortConflict — a clone never copies the source port (the INSERT
// writes ''), so enabling both projects cannot collide; PortMgr hands the
// clone a fresh port distinct from the source's occupied one. Cloning an
// already-cloned repo id fails with 400.
func TestClonePortConflict(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, repo, dsn, status) VALUES ('cp-src','源项目','cp-src','—','online')`)

	// Source occupies a runtime port (simulating a live process); the
	// allocation persists into the projects row.
	p1, err := runtime.PortMgr.Allocate("cp-src")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	t.Cleanup(func() { runtime.PortMgr.Free("cp-src") })

	r := httptest.NewRequest("POST", "/api/projects/cp-src/clone", nil)
	saHeader(r)
	w := httptest.NewRecorder()
	CloneProject(w, r, "cp-src")
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("clone = %d (body %s)", w.Code, w.Body.String())
	}

	var port, status string
	db.DB.QueryRow("SELECT COALESCE(port,'') FROM projects WHERE id='cp-src-clone'").Scan(&port)
	if port != "" {
		t.Errorf("clone port = %q, want '' (source port not copied)", port)
	}
	db.DB.QueryRow("SELECT status FROM projects WHERE id='cp-src-clone'").Scan(&status)
	if status != "offline" {
		t.Errorf("clone status = %q, want offline", status)
	}

	// Enabling the clone later gets a port distinct from the source's.
	p2, err := runtime.PortMgr.Allocate("cp-src-clone")
	if err != nil {
		t.Fatalf("allocate clone: %v", err)
	}
	t.Cleanup(func() { runtime.PortMgr.Free("cp-src-clone") })
	if p2 == p1 {
		t.Errorf("clone port %d reuses occupied source port %d", p2, p1)
	}

	// Duplicate clone of the same repo id → 400 (unique id conflict).
	r2 := httptest.NewRequest("POST", "/api/projects/cp-src/clone", nil)
	saHeader(r2)
	w2 := httptest.NewRecorder()
	CloneProject(w2, r2, "cp-src")
	if w2.Code != 400 {
		t.Errorf("second clone = %d, want 400", w2.Code)
	}
}

// TestStatusMachineExhaustive — every legal transition across the three
// states (explicit status and auto-advance), plus the whitelist rejections.
// Same-state set (online→online) is idempotent by design: it passes the
// whitelist and returns 200 (no-op + announcement), only unknown status
// strings are illegal.
func TestStatusMachineExhaustive(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`DELETE FROM notifications`)
	mustExec(`INSERT INTO projects (id, name, status) VALUES
		('sm-on','在线','online'),('sm-off','停用','offline'),('sm-maint','维护','maintenance')`)

	patch := func(uid, id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/"+id+"/status", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", uid)
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		PatchProjectStatus(w, r, id)
		return w
	}
	statusOf := func(id string) string {
		var s string
		db.DB.QueryRow("SELECT status FROM projects WHERE id=$1", id).Scan(&s)
		return s
	}

	// Full legal cycle via explicit statuses: offline→maintenance→online→offline.
	for _, want := range []struct{ id, body, next string }{
		{"sm-off", `{"status":"maintenance"}`, "maintenance"},
		{"sm-off", `{"status":"online"}`, "online"},
		{"sm-off", `{"status":"offline"}`, "offline"},
	} {
		if w := patch("admin", want.id, want.body); w.Code != 200 {
			t.Fatalf("explicit %s→%s = %d (body %s)", want.id, want.next, w.Code, w.Body.String())
		}
		if got := statusOf(want.id); got != want.next {
			t.Errorf("explicit %s = %q, want %q", want.id, got, want.next)
		}
	}
	// Legal direct jump: online→maintenance.
	if w := patch("admin", "sm-on", `{"status":"maintenance"}`); w.Code != 200 {
		t.Errorf("online→maintenance = %d, want 200", w.Code)
	}
	// Auto-advance from each state (sm-on was moved to maintenance above,
	// sm-off was cycled back to offline, sm-maint untouched).
	for id, next := range map[string]string{"sm-off": "maintenance", "sm-on": "online", "sm-maint": "online"} {
		if w := patch("admin", id, `{}`); w.Code != 200 {
			t.Errorf("auto %s = %d, want 200", id, w.Code)
		}
		if got := statusOf(id); got != next {
			t.Errorf("auto %s = %q, want %q", id, got, next)
		}
	}
	// Illegal status strings rejected on every state.
	for _, id := range []string{"sm-on", "sm-off", "sm-maint"} {
		if w := patch("admin", id, `{"status":"bogus"}`); w.Code != 400 {
			t.Errorf("bogus on %s = %d, want 400", id, w.Code)
		}
		if w := patch("admin", id, `{"status":" ONLINE "}`); w.Code != 400 {
			t.Errorf("whitespace status on %s = %d, want 400", id, w.Code)
		}
	}
	// Same-state set is idempotent (200): online→online.
	before := statusOf("sm-off")
	if w := patch("admin", "sm-off", `{"status":"`+before+`"}`); w.Code != 200 {
		t.Errorf("same-state %s→%s = %d, want 200 (idempotent)", before, before, w.Code)
	}
	if got := statusOf("sm-off"); got != before {
		t.Errorf("same-state changed status to %q", got)
	}
}

// TestBackupRestoreInPlaceRoundtrip — write data → backup → change data →
// restore → the live database is back at the backup point (real sqlite
// roundtrip through the same project, unlike the two-db variant already
// covered by TestRestoreBackupSQLiteRoundTrip).
func TestBackupRestoreInPlaceRoundtrip(t *testing.T) {
	mustExec := puFixture(t)
	if _, err := exec.LookPath("sqlite3"); err != nil {
		t.Skip("sqlite3 CLI not present")
	}
	src := filepath.ToSlash(filepath.Join(t.TempDir(), "rt.db"))
	if out, err := exec.Command("sqlite3", filepath.FromSlash(src), "CREATE TABLE t(v TEXT); INSERT INTO t VALUES('original');").CombinedOutput(); err != nil {
		t.Fatalf("seed sqlite: %s %v", out, err)
	}
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('rt-p','往返','sqlite:///`+src+`')`)
	backupDirForTest(t)

	// 1. Backup the original data.
	cr := httptest.NewRequest("POST", "/api/backups/rt-p", nil)
	saHeader(cr)
	cw := httptest.NewRecorder()
	CreateBackup(cw, cr, "rt-p")
	var bresp struct {
		Data struct {
			Filename string `json:"filename"`
			OK       bool   `json:"ok"`
		} `json:"data"`
	}
	json.Unmarshal(cw.Body.Bytes(), &bresp)
	if !bresp.Data.OK || bresp.Data.Filename == "" {
		t.Fatalf("backup = %s", cw.Body.String())
	}

	// 2. Change the data after the backup.
	if out, err := exec.Command("sqlite3", filepath.FromSlash(src), "INSERT INTO t VALUES('changed');").CombinedOutput(); err != nil {
		t.Fatalf("change sqlite: %s %v", out, err)
	}
	got, _ := exec.Command("sqlite3", filepath.FromSlash(src), "SELECT COUNT(*) FROM t;").Output()
	if !strings.Contains(string(got), "2") {
		t.Fatalf("pre-restore rows = %q, want 2", got)
	}

	// 3. Restore → data returns to the backup point.
	rr := httptest.NewRequest("POST", "/api/backups/rt-p/restore/"+bresp.Data.Filename, nil)
	saHeader(rr)
	rw := httptest.NewRecorder()
	RestoreBackup(rw, rr, "rt-p", bresp.Data.Filename)
	if rw.Code != 200 {
		t.Fatalf("restore = %d (body %s)", rw.Code, rw.Body.String())
	}
	got, _ = exec.Command("sqlite3", filepath.FromSlash(src), "SELECT v FROM t ORDER BY v;").Output()
	if strings.Contains(string(got), "changed") || !strings.Contains(string(got), "original") {
		t.Errorf("post-restore rows = %q, want only [original] (backup point)", got)
	}
}

// TestDisableEnableProcessRecovery — offline detaches the process (status
// reports running=false) and online re-attaches; each flip lands exactly one
// status notification, and the project row reflects the transition.
func TestDisableEnableProcessRecovery(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`DELETE FROM notifications`)
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('pe-p','恢复','online')`)

	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/pe-p/status", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		saHeader(r)
		w := httptest.NewRecorder()
		PatchProjectStatus(w, r, "pe-p")
		return w
	}
	var status string
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='pe-p'").Scan(&n)
	if n != 0 {
		t.Fatalf("pre-test notifications = %d, want 0", n)
	}

	// Disable: synchronous Stop/Detach → running=false.
	if w := patch(`{"status":"offline"}`); w.Code != 200 {
		t.Fatalf("offline = %d (body %s)", w.Code, w.Body.String())
	}
	db.DB.QueryRow("SELECT status FROM projects WHERE id='pe-p'").Scan(&status)
	if status != "offline" {
		t.Errorf("status = %q, want offline", status)
	}
	if st := runtime.ProcMgr.Status("pe-p"); st["running"] != false {
		t.Errorf("proc after offline = %v, want running=false", st)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='pe-p' AND type='status'").Scan(&n)
	if n != 1 {
		t.Errorf("notifications after offline = %d, want 1", n)
	}

	// Re-enable: row flips online, process lifecycle re-attached (async
	// Start — the status map must answer for the project again).
	if w := patch(`{"status":"online"}`); w.Code != 200 {
		t.Fatalf("online = %d (body %s)", w.Code, w.Body.String())
	}
	db.DB.QueryRow("SELECT status FROM projects WHERE id='pe-p'").Scan(&status)
	if status != "online" {
		t.Errorf("status = %q, want online", status)
	}
	if st := runtime.ProcMgr.Status("pe-p"); st["project_id"] != "pe-p" {
		t.Errorf("proc after online = %v, want project_id=pe-p", st)
	}
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='pe-p' AND type='status'").Scan(&n)
	if n != 2 {
		t.Errorf("notifications after re-enable = %d, want 2", n)
	}
}
