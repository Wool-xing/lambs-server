package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// TestEnsureCronSchemaIdempotent — creating twice is a no-op and the table
// accepts rows (this was the only zero-coverage function in cron_sched.go).
func TestEnsureCronSchemaIdempotent(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS scheduled_tasks`)
	EnsureCronSchema()
	EnsureCronSchema()
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command) VALUES ('sc-probe','p','probe','* * * * *','true')`)
}

// TestCronTickOnceFiresDueTasks — env-gated PG: a task whose cron matches
// the current minute fires exactly once even across two passes (same-minute
// guard); non-matching and invalid crons never fire.
func TestCronTickOnceFiresDueTasks(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not present")
	}
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
	mustExec(`DROP TABLE IF EXISTS scheduled_tasks`)
	EnsureCronSchema()
	mustExec(`DROP TABLE IF EXISTS notifications`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT, is_read BOOLEAN DEFAULT false, created_at TIMESTAMPTZ DEFAULT now())`)
	// minimal schema only for this test — drop so later tests rebuild their own
	defer mustExec(`DROP TABLE IF EXISTS notifications`)

	f, err := os.CreateTemp("", "cron_fire_*.txt")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	defer os.Remove(f.Name())
	firePath := strings.ReplaceAll(f.Name(), "\\", "/") // bash path

	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES
		('c-now',  'p1', '每分钟', '* * * * *', 'echo fired >> ` + firePath + `', 'app1'),
		('c-skip', 'p2', '不匹配', '30 2 29 2 *', 'echo never >> ` + firePath + `', 'app1'),
		('c-bad',  'p3', '坏cron', 'not-a-cron', 'echo bad >> ` + firePath + `', 'app1')`)

	lastFired := map[string]time.Time{}
	cronTickOnce(lastFired)
	// Pin the fired time so the second pass is ALWAYS within the same
	// minute — crossing a wall-clock minute boundary here would double-fire
	// and flake the assertion below. The guard stores the TRUNCATED minute.
	lastFired["c-now"] = time.Now().Truncate(time.Minute)
	cronTickOnce(lastFired) // same minute → the guard must suppress a second fire

	deadline := time.Now().Add(10 * time.Second)
	lines := 0
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(f.Name())
		lines = countLines(string(data))
		if lines >= 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lines < 1 {
		t.Fatalf("fired task left %d lines, want >= 1", lines)
	}
	// Async executions have landed by now; the guard kept the count at one.
	time.Sleep(1500 * time.Millisecond)
	data, _ := os.ReadFile(f.Name())
	if lines = countLines(string(data)); lines != 1 {
		t.Errorf("fired lines = %d, want 1 (same-minute guard failed)", lines)
	}

	for _, id := range []string{"c-skip", "c-bad"} {
		var ran interface{}
		db.DB.QueryRow("SELECT last_run_at FROM scheduled_tasks WHERE id=$1", id).Scan(&ran)
		if ran != nil {
			t.Errorf("%s ran, want never (last_run_at=%v)", id, ran)
		}
	}
	var status string
	db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='c-now'").Scan(&status)
	if status != "success" {
		t.Errorf("c-now last_status = %q, want success", status)
	}
}

// countLines counts non-empty log lines (an empty file is 0, not 1).
func countLines(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

// TestCronTickOnceDBDown — DB failure degrades to a silent skip.
func TestCronTickOnceDBDown(t *testing.T) {
	lazyDB(t)
	cronTickOnce(map[string]time.Time{})
}

// TestExecuteTaskWindowsBranch — a windows-host task runs through
// runWindowsCommand (httptest agent) and persists last_status=success.
func TestExecuteTaskWindowsBranch(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true,"code":0,"stdout":"win scan","stderr":""}`))
	}))
	defer srv.Close()
	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = srv.URL, "t"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`DROP TABLE IF EXISTS scheduled_tasks`)
	EnsureCronSchema()
	mustExec(`INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host) VALUES ('t-win','proj-x','win任务','* * * * *','win cmd','windows')`)

	executeTask("t-win", "proj-x", "win任务", "win cmd", "windows")

	var status string
	db.DB.QueryRow("SELECT last_status FROM scheduled_tasks WHERE id='t-win'").Scan(&status)
	if status != "success" {
		t.Errorf("last_status = %q, want success", status)
	}
}

// TestRunWindowsCommandRetriesTransportError — a first-request transport
// blip (connection dropped mid-flight) retries and succeeds; the run is not
// burned.
func TestRunWindowsCommandRetriesTransportError(t *testing.T) {
	cmdHits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cmd" {
			w.Write([]byte(`{"version":"1.0.0"}`))
			return
		}
		cmdHits++
		if cmdHits == 1 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "no hijack", 500)
				return
			}
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close() // drop the connection like a Tailscale blip
			}
			return
		}
		w.Write([]byte(`{"ok":true,"code":0,"stdout":"retried ok","stderr":""}`))
	}))
	defer srv.Close()

	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = srv.URL, "t"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	ok, out, status := runWindowsCommand("flaky cmd", 60*time.Second)
	if !ok || status != "success" || !strings.Contains(out, "retried ok") {
		t.Fatalf("ok=%v status=%s out=%q, want success after retry", ok, status, out)
	}
}

// TestNodePollOnceOnline — one monitor pass writes both snapshots from the
// configured endpoints.
func TestNodePollOnceOnline(t *testing.T) {
	wool := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"hostname":"wool","cpu_percent":2.5,"memory_used_mb":700,"memory_total_mb":3900,"disk_used_gb":11.1,"disk_total_gb":40.0,"uptime_seconds":999}`))
	}))
	defer wool.Close()
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"hostname":"WINBOX","cpu_percent":8.0,"memory_used_mb":900,"memory_total_mb":16000,"uptime_seconds":42}`))
	}))
	defer agent.Close()

	t.Setenv("WOOL_AGENT_URL", wool.URL)
	oldURL := agentURL
	agentURL = agent.URL
	t.Cleanup(func() {
		agentURL = oldURL
		woolMu.Lock()
		woolNode = NodeSnapshot{}
		woolMu.Unlock()
		agentMu.Lock()
		agentNode = NodeSnapshot{}
		agentMu.Unlock()
	})

	nodePollOnce()

	w := WoolSnapshot()
	if !w.Online || w.CPU != 2.5 || w.Uptime != 999 {
		t.Errorf("WoolSnapshot = %+v, want online wool metrics", w)
	}
	a := AgentSnapshot()
	if !a.Online || a.Name != "WINBOX" || a.MemTotalMB != 16000 {
		t.Errorf("AgentSnapshot = %+v, want online agent metrics", a)
	}
}

// TestNodePollOnceOffline — unreachable endpoints mark both nodes offline.
func TestNodePollOnceOffline(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := dead.URL
	dead.Close()

	t.Setenv("WOOL_AGENT_URL", url)
	oldURL := agentURL
	agentURL = url
	t.Cleanup(func() {
		agentURL = oldURL
		woolMu.Lock()
		woolNode = NodeSnapshot{}
		woolMu.Unlock()
		agentMu.Lock()
		agentNode = NodeSnapshot{}
		agentMu.Unlock()
	})

	nodePollOnce()

	if WoolSnapshot().Online || AgentSnapshot().Online {
		t.Error("nodes reported online for unreachable endpoints")
	}
}
