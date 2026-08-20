package nginx

import (
	"os"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

// stubSSH swaps sshRun for a recorder; returns the recorder.
func stubSSH(t *testing.T, results ...error) *struct {
	calls int
	hosts []string
	cmds  []string
	stdin []string
} {
	t.Helper()
	old := sshRun
	rec := &struct {
		calls int
		hosts []string
		cmds  []string
		stdin []string
	}{}
	sshRun = func(host, cmd string, stdin []byte) ([]byte, error) {
		var err error
		if rec.calls < len(results) {
			err = results[rec.calls]
		}
		rec.calls++
		rec.hosts = append(rec.hosts, host)
		rec.cmds = append(rec.cmds, cmd)
		rec.stdin = append(rec.stdin, string(stdin))
		return []byte("ok"), err
	}
	t.Cleanup(func() { sshRun = old })
	return rec
}

func TestPushConfigUnconfigured(t *testing.T) {
	old := web1Host
	web1Host = ""
	defer func() { web1Host = old }()
	err := pushConfig("# conf")
	if err == nil || !strings.Contains(err.Error(), "WEB1_SSH_HOST") {
		t.Errorf("err = %v, want WEB1_SSH_HOST refusal", err)
	}
}

// TestPushConfigSSHFlow — the managed config must land on Web1 via two ssh
// commands: tee (config on stdin) then nginx -t && reload. A failed write
// must not trigger a reload.
func TestPushConfigSSHFlow(t *testing.T) {
	old := web1Host
	web1Host = "ubuntu@web1.internal"
	defer func() { web1Host = old }()

	rec := stubSSH(t)
	if err := pushConfig("# conf"); err != nil {
		t.Fatalf("pushConfig: %v", err)
	}
	if rec.calls != 2 {
		t.Fatalf("ssh calls = %d, want 2", rec.calls)
	}
	if rec.hosts[0] != "ubuntu@web1.internal" || rec.hosts[1] != "ubuntu@web1.internal" {
		t.Errorf("hosts = %v", rec.hosts)
	}
	if !strings.Contains(rec.cmds[0], "tee /etc/nginx/sites-available/lambs-managed.conf") {
		t.Errorf("first cmd = %q, want tee of managed config", rec.cmds[0])
	}
	if rec.stdin[0] != "# conf" {
		t.Errorf("stdin = %q, want config content", rec.stdin[0])
	}
	if !strings.Contains(rec.cmds[1], "nginx -t") || !strings.Contains(rec.cmds[1], "reload") {
		t.Errorf("second cmd = %q, want nginx -t && reload", rec.cmds[1])
	}

	// Failure path: write fails → error, reload never attempted.
	rec = stubSSH(t, os.ErrDeadlineExceeded)
	if err := pushConfig("# conf"); err == nil {
		t.Error("pushConfig with failing ssh should error")
	}
	if rec.calls != 1 {
		t.Errorf("ssh calls after write failure = %d, want 1 (no reload)", rec.calls)
	}
}

// TestSyncRetries — a transient Web1 outage must retry (3 attempts) and not
// silently leave the managed config stale. Real postgres, gated on
// LAMBS_TEST_PG_DSN.
func TestSyncRetries(t *testing.T) {
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
	mustExec(`DELETE FROM projects WHERE id='nginx-retry'`)
	mustExec(`INSERT INTO projects (id, name, base_path, port) VALUES ('nginx-retry','重试项目','/retry','3601')`)
	defer mustExec(`DELETE FROM projects WHERE id='nginx-retry'`)

	oldHost, oldDelay := web1Host, retryDelay
	web1Host = "ubuntu@web1.internal"
	retryDelay = 10 * time.Millisecond
	defer func() { web1Host, retryDelay = oldHost, oldDelay }()

	// First two attempts fail at the write, third succeeds fully: 2 failed
	// tee calls + 1 successful tee + 1 reload = 4 ssh invocations.
	rec := stubSSH(t, os.ErrDeadlineExceeded, os.ErrDeadlineExceeded, nil)
	Sync()
	if rec.calls != 4 {
		t.Errorf("ssh invocations = %d, want 4 (2 failed tee + tee + reload)", rec.calls)
	}
}

// TestRefreshOne — user count lands on users_count and the 用户数 feature is
// appended when absent / updated when present. dsn '—' exercises the
// zero-count path without a live datasource.
func TestRefreshOne(t *testing.T) {
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
	mustExec(`DELETE FROM projects WHERE id IN ('ngx-refresh-a','ngx-refresh-b')`)
	mustExec(`INSERT INTO projects (id, name, dsn, users_count, features) VALUES
		('ngx-refresh-a','A','—',7,'[{"label":"其他","value":1}]'),
		('ngx-refresh-b','B','—',9,'[{"label":"用户数","value":5},{"label":"其他","value":2}]')`)
	defer mustExec(`DELETE FROM projects WHERE id IN ('ngx-refresh-a','ngx-refresh-b')`)

	refreshOne("ngx-refresh-a", "—")
	refreshOne("ngx-refresh-b", "—")

	var countA, countB int
	db.DB.QueryRow("SELECT users_count FROM projects WHERE id='ngx-refresh-a'").Scan(&countA)
	db.DB.QueryRow("SELECT users_count FROM projects WHERE id='ngx-refresh-b'").Scan(&countB)
	if countA != 0 || countB != 0 {
		t.Errorf("users_count = %d/%d, want 0/0", countA, countB)
	}
	// jsonb assertions (spacing-insensitive): A gets 用户数 appended, B's
	// existing 用户数=5 is updated to 0 — no duplicate entry.
	for _, id := range []string{"ngx-refresh-a", "ngx-refresh-b"} {
		var ok bool
		db.DB.QueryRow(
			"SELECT features @> '[{\"label\":\"用户数\",\"value\":0}]'::jsonb FROM projects WHERE id=$1", id).Scan(&ok)
		if !ok {
			t.Errorf("%s: features missing 用户数=0", id)
		}
	}
	var fB string
	db.DB.QueryRow("SELECT features::text FROM projects WHERE id='ngx-refresh-b'").Scan(&fB)
	if strings.Count(fB, "用户数") != 1 {
		t.Errorf("feature B duplicated 用户数: %s", fB)
	}
}
