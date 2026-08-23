package runtime

import (
	"io"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lambs-server-go/internal/db"
)

func newTestTCPProxy() *TCPProxy {
	return &TCPProxy{
		listeners: make(map[string]net.Listener),
		actives:   make(map[string]*int64),
	}
}

// TestTCPProxyStartInvalidPort — no usable port column: explicit refusal
// (tcpproxy.go was the lowest-covered file in the repo, 53.3%).
func TestTCPProxyStartInvalidPort(t *testing.T) {
	lazyDB(t)
	tp := newTestTCPProxy()
	err := tp.Start("no-port-proj")
	if err == nil || !strings.Contains(err.Error(), "invalid port") {
		t.Errorf("Start err = %v, want invalid port refusal", err)
	}
}

// TestTCPProxyServeRoundTrip — a real proxy session: client dials the proxy
// listener, bytes round-trip through to a local echo backend. The
// in-connection ProcMgr.Start call degrades silently on the lazy DB.
func TestTCPProxyServeRoundTrip(t *testing.T) {
	lazyDB(t)
	// Echo backend.
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, err := echoLn.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()

	tp := newTestTCPProxy()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	var counter int64
	go tp.serve("p", ln, echoLn.Addr().String(), &counter)

	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	if _, err := client.Write([]byte("ping")); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Errorf("echo = %q, want ping", buf)
	}
	// Closing the client ends the client→backend copy, which releases the
	// relay's <-done and lets the connection counter return to 0.
	client.Close()
	// Counter peaked at 1 during the connection; the relay goroutines wind
	// down after close — just confirm it returned to 0 within a beat.
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&counter) != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt64(&counter) != 0 {
		t.Errorf("active counter = %d, want 0 after close", counter)
	}
}

// TestTCPProxyStop — Stop removes the registry entry and closes the
// listener; stopping an unknown project is a silent no-op.
func TestTCPProxyStop(t *testing.T) {
	tp := newTestTCPProxy()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	tp.mu.Lock()
	tp.listeners["p"] = ln
	tp.mu.Unlock()

	tp.Stop("p")
	tp.mu.RLock()
	_, tracked := tp.listeners["p"]
	tp.mu.RUnlock()
	if tracked {
		t.Error("listener still tracked after Stop")
	}
	if _, err := ln.Accept(); err == nil {
		t.Error("listener not closed by Stop")
	}
	tp.Stop("ghost") // no-op
}

// TestTCPProxyStartDuplicate — second Start for a tracked project is a
// silent success, no second listener.
func TestTCPProxyStartDuplicate(t *testing.T) {
	lazyDB(t)
	tp := newTestTCPProxy()
	// Direct registry insert avoids a real DB row: Start's early-return
	// branch fires before any DB access.
	tp.mu.Lock()
	tp.listeners["dup"] = &net.TCPListener{}
	tp.mu.Unlock()
	if err := tp.Start("dup"); err != nil {
		t.Errorf("duplicate Start err = %v, want nil", err)
	}
}

// TestTCPProxyStartBranches — env-gated PG: the no-backend skip, the
// self-loop guard, a real listener registration, and the listen-conflict
// refusal all run against real project rows.
func TestTCPProxyStartBranches(t *testing.T) {
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

	tp := newTestTCPProxy()
	t.Cleanup(func() { tp.Stop("px-skip"); tp.Stop("px-loop"); tp.Stop("px-live") })

	// 1. No backend + no service config → skip (nil), nothing registered.
	mustExec(`INSERT INTO projects (id, name, port) VALUES ('px-skip', 'skip', '35551')`)
	if err := tp.Start("px-skip"); err != nil {
		t.Errorf("no-backend skip err = %v, want nil", err)
	}

	// 2. Backend == listener port on loopback → self-loop skip (nil).
	mustExec(`INSERT INTO projects (id, name, port, backend_url) VALUES ('px-loop', 'loop', '35552', 'http://127.0.0.1:35552')`)
	if err := tp.Start("px-loop"); err != nil {
		t.Errorf("self-loop skip err = %v, want nil", err)
	}

	// 3. Real registration: backend differs → listener on :35553.
	mustExec(`INSERT INTO projects (id, name, port, backend_url) VALUES ('px-live', 'live', '35553', 'http://127.0.0.1:35554')`)
	if err := tp.Start("px-live"); err != nil {
		t.Fatalf("live Start err = %v", err)
	}
	tp.mu.RLock()
	_, tracked := tp.listeners["px-live"]
	tp.mu.RUnlock()
	if !tracked {
		t.Error("live listener not registered")
	}

	// 4. Port conflict: another project wants :35553 → listen refusal.
	mustExec(`INSERT INTO projects (id, name, port, backend_url) VALUES ('px-conflict', 'c', '35553', 'http://127.0.0.1:35554')`)
	if err := tp.Start("px-conflict"); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Errorf("conflict Start err = %v, want listen refusal", err)
	}
}

// TestIdleOnceDegradedPaths — the extracted idle pass survives both a down
// DB and a busy connection (the deeper stop branch needs a genuinely
// running ProcManager project, which no test can fake without spawning).
func TestIdleOnceDegradedPaths(t *testing.T) {
	lazyDB(t)
	tp := newTestTCPProxy()
	tp.idleOnce() // empty registry: no-op

	var active int64 = 1
	tp.mu.Lock()
	tp.actives["busy"] = &active
	tp.mu.Unlock()
	tp.idleOnce() // busy counter: skip, no panic

	var idle int64
	tp.mu.Lock()
	tp.actives["idle"] = &idle
	tp.mu.Unlock()
	tp.idleOnce() // idle but Status reports not-running on the lazy DB: skip
}
