package runtime

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// projectsFixture creates the full projects schema (round-9 CI lesson: a
// minimal recreate broke downstream packages at 42703) and returns the
// mustExec helper.
func projectsFixture(t *testing.T) func(string, ...interface{}) {
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

// TestReadProcStatsErrorPath — a pid that cannot exist anywhere yields
// zeros (no /proc on Windows; ENOENT on Linux), never a panic.
func TestReadProcStatsErrorPath(t *testing.T) {
	ticks, rss, start := readProcStats(1 << 30)
	if ticks != 0 || rss != 0 || start != 0 {
		t.Errorf("readProcStats = (%d,%d,%d), want zeros for missing pid", ticks, rss, start)
	}
}

// TestStartStatusStopTrackedProcess — env-gated PG: a project with a real
// startup_command gets a tracked process; Status reports running/pid/port
// (and the cpu-percent sampling branch on a second call), List shows the
// entry, Stop tears it down and Status flips back to running:false.
func TestStartStatusStopTrackedProcess(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not present")
	}
	mustExec := projectsFixture(t)
	mustExec(`INSERT INTO projects (id, name, port, startup_command) VALUES
		('proc-live', 'live', '3602', 'sleep 30')`)

	pm := newTestPM()
	t.Setenv("LAMBS_MIN_FREE_MB", "0") // CI boxes may sit under the 100MB floor
	t.Cleanup(func() { pm.Stop("proc-live") })

	if err := pm.Start("proc-live"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st := pm.Status("proc-live")
	if st["running"] != true || st["pid"].(int) <= 0 {
		t.Fatalf("Status after start = %v, want running with pid", st)
	}
	if st["port"].(int) != 3602 {
		t.Errorf("port = %v, want 3602", st["port"])
	}
	if st["starting"] != true {
		t.Errorf("starting = %v, want true right after launch", st["starting"])
	}
	// Second call: previous sample exists, cpu branch computes (both zero
	// ticks without /proc) and samples[projectID] is refreshed.
	st2 := pm.Status("proc-live")
	if st2["running"] != true {
		t.Fatalf("second Status = %v", st2)
	}
	if _, ok := st2["cpu_percent"]; !ok {
		t.Errorf("cpu_percent missing: %v", st2)
	}

	list := pm.List()
	if len(list) != 1 || list[0]["project_id"] != "proc-live" {
		t.Errorf("List = %v, want 1 live entry", list)
	}

	if err := pm.Stop("proc-live"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	st3 := pm.Status("proc-live")
	if st3["running"] != false {
		t.Errorf("Status after stop = %v, want running:false", st3)
	}
}

// TestStopUntrackedWithServiceName — no tracked process but a declared
// service_name: the systemctl fallback is attempted (no unit file on this
// box) and the stop degrades to a silent success.
func TestStopUntrackedWithServiceName(t *testing.T) {
	mustExec := projectsFixture(t)
	mustExec(`INSERT INTO projects (id, name, service_name) VALUES ('proc-svc', 'svc', 'lambs-web')`)

	pm := newTestPM()
	if err := pm.Stop("proc-svc"); err != nil {
		t.Errorf("Stop untracked-with-svc = %v, want nil", err)
	}
	// Status hits the systemd fallback branch; without systemd the pid
	// lookup degrades to 0 and the envelope says not-running.
	st := pm.Status("proc-svc")
	if st["running"] != false {
		t.Errorf("Status = %v, want running:false", st)
	}
}

// TestListSkipsNilCmd — a map entry whose process is gone (Wait goroutine
// has not cleaned up yet) must be skipped, never panicked on.
func TestListSkipsNilCmd(t *testing.T) {
	pm := newTestPM()
	pm.mu.Lock()
	pm.procs["ghost"] = &procState{}
	pm.mu.Unlock()
	if got := pm.List(); len(got) != 0 {
		t.Errorf("List = %v, want empty for nil-cmd entry", got)
	}
}

// TestStartSharedAlreadyStarting — the in-flight launch guard returns
// without a second start.
func TestStartSharedAlreadyStarting(t *testing.T) {
	pm := newTestPM()
	st := &svcState{name: "svc-x", startCmd: "true", starting: true}
	if err := pm.startShared(st); err != nil {
		t.Errorf("startShared on starting service = %v, want nil", err)
	}
	if !st.starting {
		t.Error("starting flag must stay true (launch in flight)")
	}
}

// TestServiceRunningBranches — the systemctl-style start command degrades
// to false when the unit is not active; a direct-run with a live process
// reports true.
func TestServiceRunningBranches(t *testing.T) {
	pm := newTestPM()
	if got := pm.serviceRunning(&svcState{startCmd: "systemctl start lambs-web"}); got {
		t.Error("systemctl branch = true, want false without active unit")
	}
	if got := pm.serviceRunning(&svcState{startCmd: "systemctl start"}); got {
		t.Error("systemctl without unit token = true, want false")
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep: %v", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	st := &svcState{startCmd: "sleep 30", cmd: cmd}
	if !pm.serviceRunning(st) {
		t.Error("direct-run with live process = false, want true")
	}
}

// TestDetachStopsLongRunningService — env-gated PG: a referenced shared
// service with a long-running start_cmd is killed on last detach (the
// stopShared kill path; the "true" roundtrip covers the exited-process
// branch).
func TestDetachStopsLongRunningService(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not present")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not present")
	}
	mustExec := projectsFixture(t)
	mustExec(`INSERT INTO projects (id, name, services) VALUES
		('svc-long-proj', 'long', '[{"name":"svc-long","start_cmd":"sleep 30","stop_cmd":""}]')`)

	pm := newTestPM()
	t.Cleanup(func() { pm.DetachServices("svc-long-proj") })

	pm.AttachServices("svc-long-proj") // starts bash -c 'sleep 30' (2s stagger)
	pm.mu.Lock()
	_, ok := pm.services["svc-long"]
	pm.mu.Unlock()
	if !ok {
		t.Fatal("svc-long not registered after attach")
	}

	pm.DetachServices("svc-long-proj") // refs 0 → stopShared kills the process
	pm.mu.Lock()
	_, gone := pm.services["svc-long"]
	pm.mu.Unlock()
	if gone {
		t.Error("svc-long still registered after last detach")
	}
	// Give the Wait goroutine a moment to reap; nothing further to assert
	// beyond no hang/panic (kill is synchronous via the done channel).
	time.Sleep(50 * time.Millisecond)
}
