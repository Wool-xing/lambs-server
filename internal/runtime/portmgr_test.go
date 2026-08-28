package runtime

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestPortMgrAllocate — real postgres: allocation lands in-range and
// persists; a re-request is idempotent; a port actually LISTENED on is
// skipped; Free clears the assignment.
func TestPortMgrAllocate(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT, port TEXT)`)
	mustExec(`DELETE FROM projects WHERE id IN ('pm-a','pm-b','pm-c','pm-d')`)
	defer mustExec(`DELETE FROM projects WHERE id IN ('pm-a','pm-b','pm-c','pm-d')`)

	// Hold the range start with a real listener so the allocator must skip it.
	hold, err := net.Listen("tcp", fmt.Sprintf(":%d", PortMgr.StartPort))
	if err != nil {
		t.Fatalf("hold port: %v", err)
	}
	defer hold.Close()

	// Fresh project: gets StartPort+1 (StartPort is held).
	mustExec(`INSERT INTO projects (id, name, port) VALUES ('pm-a','A','')`)
	p, err := PortMgr.Allocate("pm-a")
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if p != PortMgr.StartPort+1 {
		t.Errorf("port = %d, want %d (held port skipped)", p, PortMgr.StartPort+1)
	}
	var stored string
	db.DB.QueryRow("SELECT port FROM projects WHERE id='pm-a'").Scan(&stored)
	if stored != fmt.Sprintf("%d", p) {
		t.Errorf("persisted port = %q, want %d", stored, p)
	}

	// Idempotent: second call returns the same port.
	if p2, _ := PortMgr.Allocate("pm-a"); p2 != p {
		t.Errorf("re-allocate = %d, want same %d", p2, p)
	}

	// Project with an in-range port keeps it.
	mustExec(`INSERT INTO projects (id, name, port) VALUES ('pm-b','B','3520')`)
	if p2, _ := PortMgr.Allocate("pm-b"); p2 != 3520 {
		t.Errorf("existing port = %d, want 3520", p2)
	}

	// Free clears the assignment.
	mustExec(`INSERT INTO projects (id, name, port) VALUES ('pm-c','C','3530')`)
	PortMgr.Free("pm-c")
	var cleared string
	db.DB.QueryRow("SELECT COALESCE(port,'') FROM projects WHERE id='pm-c'").Scan(&cleared)
	if cleared != "" {
		t.Errorf("port after Free = %q, want empty", cleared)
	}

	// Two fresh projects never collide.
	mustExec(`INSERT INTO projects (id, name, port) VALUES ('pm-d','D','')`)
	pd, err := PortMgr.Allocate("pm-d")
	if err != nil {
		t.Fatalf("allocate d: %v", err)
	}
	if pd == p {
		t.Errorf("pm-d got %d, collides with pm-a's %d", pd, p)
	}
}

// TestPortMgrAllocateExhaustion — every port in the range is LISTENED on:
// Allocate must answer the exact exhaustion error, never a wrong port. The
// lazy DB degrades every query to zero rows, so only the listener sweep
// decides the outcome.
func TestPortMgrAllocateExhaustion(t *testing.T) {
	lazyDB(t)
	pm := &PortManager{StartPort: 46000, EndPort: 46002}
	var holds []net.Listener
	for port := 46000; port <= 46002; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			t.Fatalf("hold %d: %v", port, err)
		}
		holds = append(holds, ln)
	}
	defer func() {
		for _, ln := range holds {
			ln.Close()
		}
	}()

	_, err := pm.Allocate("pm-full")
	if err == nil {
		t.Fatal("Allocate with full range succeeded, want exhaustion error")
	}
	if !strings.Contains(err.Error(), "no free ports in range 46000-46002") {
		t.Errorf("err = %v, want range exhaustion message", err)
	}
}
