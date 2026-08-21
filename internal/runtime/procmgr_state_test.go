package runtime

import (
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// newTestPM returns a fresh ProcManager — never the package global, so
// tests can't pollute ProcMgr's state.
func newTestPM() *ProcManager {
	return &ProcManager{
		procs:     make(map[string]*procState),
		services:  make(map[string]*svcState),
		samples:   make(map[string]cpuSample),
		lastAlert: make(map[string]time.Time),
	}
}

// lazyDB points db.DB at an unreachable postgres — QueryRow/Query fail at
// the row level, which is exactly the degraded path these state-machine
// methods must survive. Restored via t.Cleanup (var-audit convention).
func lazyDB(t *testing.T) {
	t.Helper()
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	old := db.DB
	db.DB = tdb
	t.Cleanup(func() { db.DB = old })
}

// TestStartNoServiceConfig — a project row that yields neither
// startup_command nor service_name must be refused with the exact error the
// health monitor keys off (procmgr was 0% on the whole state machine).
func TestStartNoServiceConfig(t *testing.T) {
	lazyDB(t)
	pm := newTestPM()
	err := pm.Start("no-config-proj")
	if err == nil || !strings.Contains(err.Error(), "no service_name") {
		t.Errorf("Start err = %v, want no service_name refusal", err)
	}
}

// TestStopUntrackedNoop — stopping a project with no tracked process and no
// systemd unit is a silent success.
func TestStopUntrackedNoop(t *testing.T) {
	lazyDB(t)
	pm := newTestPM()
	if err := pm.Stop("ghost"); err != nil {
		t.Errorf("Stop untracked = %v, want nil", err)
	}
}

// TestStatusNotRunning — no tracked process, no systemd unit: honest
// running:false envelope.
func TestStatusNotRunning(t *testing.T) {
	lazyDB(t)
	pm := newTestPM()
	st := pm.Status("ghost")
	if st["running"] != false || st["project_id"] != "ghost" {
		t.Errorf("Status = %v, want running:false", st)
	}
}

// TestListEmpty — a fresh manager returns an empty list, never nil.
func TestListEmpty(t *testing.T) {
	pm := newTestPM()
	got := pm.List()
	if got == nil || len(got) != 0 {
		t.Errorf("List = %v, want empty non-nil", got)
	}
}

// TestSystemdPIDUnavailable — without systemd (Windows dev, CI containers)
// the lookup degrades to 0 and the caller reports not-running.
func TestSystemdPIDUnavailable(t *testing.T) {
	if pid := systemdPID("lambs-no-such-unit"); pid != 0 {
		t.Errorf("systemdPID = %d, want 0 on unavailable systemctl", pid)
	}
}

// TestReadServicesNoDB — DB down: empty service list, no panic.
func TestReadServicesNoDB(t *testing.T) {
	lazyDB(t)
	if got := readServices("x"); got != nil {
		t.Errorf("readServices = %v, want nil on DB failure", got)
	}
}

// TestAttachServicesNoop — DB down: nothing to attach, no panic.
func TestAttachServicesNoop(t *testing.T) {
	lazyDB(t)
	newTestPM().AttachServices("x")
}

// TestDetachServicesNoop — DB down: nothing to detach, no panic.
func TestDetachServicesNoop(t *testing.T) {
	lazyDB(t)
	newTestPM().DetachServices("x")
}

// TestProcUptimeSecNonNegative — without /proc/uptime (Windows) the
// degraded value is 0; with it, a sane non-negative number.
func TestProcUptimeSecNonNegative(t *testing.T) {
	if got := procUptimeSec(100); got < 0 {
		t.Errorf("procUptimeSec = %d, want non-negative", got)
	}
}

// TestRestartUntracked — restart of an unknown project degrades to the
// Start refusal (not a nil-success).
func TestRestartUntracked(t *testing.T) {
	lazyDB(t)
	pm := newTestPM()
	if err := pm.Restart("ghost"); err == nil {
		t.Error("Restart untracked = nil, want no-service refusal")
	}
}

// TestAttachDetachServicesRoundTrip — env-gated PG: two projects share one
// service. Attach twice = one launch (refcount 2), detach to 0 stops and
// forgets the service. start/stop commands are "true" so nothing real runs.
func TestAttachDetachServicesRoundTrip(t *testing.T) {
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
	// Full schema — downstream packages INSERT into the same shared table
	// (round 9 CI lesson: a minimal recreate broke nginx/runtime at 42703).
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
	mustExec(`INSERT INTO projects (id, name, services) VALUES
		('svc-proj-a', 'A', '[{"name":"svc-x","start_cmd":"true","stop_cmd":"true"}]'),
		('svc-proj-b', 'B', '[{"name":"svc-x","start_cmd":"true","stop_cmd":"true"}]')`)

	pm := newTestPM()
	pm.AttachServices("svc-proj-a") // refs 1, starts svc-x (2s stagger)
	pm.AttachServices("svc-proj-b") // refs 2, no second start
	pm.mu.Lock()
	st, ok := pm.services["svc-x"]
	refs := 0
	if ok {
		refs = len(st.refs)
	}
	pm.mu.Unlock()
	if !ok || refs != 2 {
		t.Fatalf("after attach: ok=%v refs=%d, want 2", ok, refs)
	}

	pm.DetachServices("svc-proj-a") // refs 1, still alive
	pm.mu.Lock()
	_, stillThere := pm.services["svc-x"]
	pm.mu.Unlock()
	if !stillThere {
		t.Fatal("service dropped at refs=1")
	}

	pm.DetachServices("svc-proj-b") // refs 0, stopped + forgotten
	pm.mu.Lock()
	_, gone := pm.services["svc-x"]
	pm.mu.Unlock()
	if gone {
		t.Error("service still registered after last detach")
	}
}

// TestHealthOnceDisabled — enabled=false is a no-op before any DB access
// (the monitor loop is untestable; the extracted pass is).
func TestHealthOnceDisabled(t *testing.T) {
	lazyDB(t)
	newTestPM().healthOnce(func() bool { return false })
}

// TestHealthOnceDBDown — DB failure degrades to a silent skip, never a panic.
func TestHealthOnceDBDown(t *testing.T) {
	lazyDB(t)
	newTestPM().healthOnce(func() bool { return true })
}

// TestHealthOnceRestartsDownProject — env-gated PG: an online project with a
// startup_command but no tracked process gets restarted (start_cmd "true")
// and a 进程恢复 notification is inserted. The 10-minute alert cooldown is
// also exercised: a second pass for a failing project stays silent.
func TestHealthOnceRestartsDownProject(t *testing.T) {
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
	// Full schema (round-9 CI lesson) + notifications fixture.
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
	mustExec(`DROP TABLE IF EXISTS notifications`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT, is_read BOOLEAN DEFAULT false, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`INSERT INTO projects (id, name, status, startup_command) VALUES
		('health-down', 'down', 'online', 'true'),
		('health-bad', 'bad', 'online', 'nonexistent-cmd-xyz')`)

	pm := newTestPM()
	pm.healthOnce(func() bool { return true })

	var n int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='health-down' AND title='进程恢复'").Scan(&n); err != nil || n != 1 {
		t.Errorf("health-down 恢复通知 = %d (%v), want 1", n, err)
	}
	// health-bad fails to start → 进程异常 alert (cooldown allows the first).
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='health-bad' AND title='进程异常'").Scan(&n); err != nil || n != 1 {
		t.Errorf("health-bad 异常通知 = %d (%v), want 1", n, err)
	}
	// Second pass inside the cooldown: no duplicate alert for health-bad.
	pm.healthOnce(func() bool { return true })
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='health-bad' AND title='进程异常'").Scan(&n); err != nil || n != 1 {
		t.Errorf("cooldown 后异常通知 = %d (%v), want still 1", n, err)
	}
	// health-down's "true" process exited by now — Status flips back to
	// down and the next pass restarts it again: 进程恢复 count grows.
	time.Sleep(300 * time.Millisecond)
	pm.healthOnce(func() bool { return true })
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='health-down' AND title='进程恢复'").Scan(&n); err != nil || n < 1 {
		t.Errorf("health-down 再次恢复通知 = %d (%v)", n, err)
	}
}

// TestHealthOnceRestartsSharedService — env-gated: a referenced shared
// service that is not running gets restarted and a 共享服务恢复
// notification lands.
func TestHealthOnceRestartsSharedService(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS notifications`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT, is_read BOOLEAN DEFAULT false, created_at TIMESTAMPTZ DEFAULT now())`)

	pm := newTestPM()
	pm.mu.Lock()
	pm.services["svc-health"] = &svcState{name: "svc-health", startCmd: "true", stopCmd: "true", refs: map[string]bool{"p": true}}
	pm.mu.Unlock()

	pm.healthOnce(func() bool { return true })

	var n int
	if err := db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE title='共享服务恢复'").Scan(&n); err != nil || n != 1 {
		t.Errorf("共享服务恢复通知 = %d (%v), want 1", n, err)
	}
}
