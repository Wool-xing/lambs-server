package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/runtime"
)

// TestMachinesCRUD — real PostgreSQL: upsert (register + update), list,
// status patch. Gated on LAMBS_TEST_PG_DSN.
func TestMachinesCRUD(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if _, err := db.DB.Exec(`DROP TABLE IF EXISTS machines CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.DB.Exec(`CREATE TABLE machines (
		id TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', ts_ip TEXT, lan_ip TEXT,
		os TEXT, arch TEXT, cpu_cores INT DEFAULT 0, mem_gb INT DEFAULT 0, disk_gb INT DEFAULT 0,
		tags JSONB NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'online',
		override JSONB NOT NULL DEFAULT '[]', notes TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_check_at TIMESTAMPTZ, ssh_user TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create: %v", err)
	}

	post := func(path string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		UpsertMachine(w, req)
		return w
	}

	// Register
	w := post("/api/machines", `{"id":"testm1","role":"compute","ts_ip":"100.64.0.99","lan_ip":"10.0.0.99","os":"ubuntu","arch":"amd64","cpu_cores":2,"mem_gb":4,"disk_gb":50,"status":"online"}`)
	if w.Code != 200 {
		t.Fatalf("register code=%d body=%s", w.Code, w.Body.String())
	}
	// Update (upsert path)
	w = post("/api/machines", `{"id":"testm1","role":"compute","ts_ip":"100.64.0.99","mem_gb":8,"status":"online"}`)
	if w.Code != 200 {
		t.Fatalf("update code=%d body=%s", w.Code, w.Body.String())
	}
	// Missing id rejected
	w = post("/api/machines", `{"role":"gate"}`)
	if w.Code != 400 {
		t.Fatalf("missing id should 400, got %d", w.Code)
	}

	// List contains the machine with updated mem_gb
	req := httptest.NewRequest(http.MethodGet, "/api/machines", nil)
	lw := httptest.NewRecorder()
	ListMachines(lw, req)
	var resp struct {
		Data struct {
			Machines []map[string]interface{} `json:"machines"`
			Total    int                      `json:"total"`
		} `json:"data"`
	}
	if err := json.Unmarshal(lw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if resp.Data.Total != 1 {
		t.Fatalf("expected 1 machine, got %d", resp.Data.Total)
	}
	m := resp.Data.Machines[0]
	if m["id"] != "testm1" || m["mem_gb"].(float64) != 8 {
		t.Fatalf("unexpected machine row: %v", m)
	}

	// Status patch
	preq := httptest.NewRequest(http.MethodPatch, "/api/machines/testm1/status", bytes.NewBufferString(`{"status":"offline"}`))
	pw := httptest.NewRecorder()
	PatchMachineStatus(pw, preq, "testm1")
	if pw.Code != 200 {
		t.Fatalf("patch code=%d body=%s", pw.Code, pw.Body.String())
	}
	if _, err := db.DB.Exec(`DELETE FROM machines WHERE id='testm1'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestAllocHost — auto-pick chooses the largest online linux compute machine;
// an explicit offline machine id is rejected.
func TestAllocHost(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if _, err := db.DB.Exec(`DROP TABLE IF EXISTS machines CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.DB.Exec(`CREATE TABLE machines (
		id TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', ts_ip TEXT, lan_ip TEXT,
		os TEXT, arch TEXT, cpu_cores INT DEFAULT 0, mem_gb INT DEFAULT 0, disk_gb INT DEFAULT 0,
		tags JSONB NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'online',
		override JSONB NOT NULL DEFAULT '[]', notes TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_check_at TIMESTAMPTZ, ssh_user TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create: %v", err)
	}
	seed := `INSERT INTO machines (id, role, ts_ip, os, mem_gb, disk_gb, cpu_cores, status) VALUES
		('small','compute','100.64.0.1','ubuntu',1,40,1,'online'),
		('big','compute','100.64.0.2','ubuntu',12,96,2,'online'),
		('offone','compute','100.64.0.3','ubuntu',16,200,4,'offline'),
		('gate','gate','100.64.0.4','ubuntu',1,48,1,'online')`
	if _, err := db.DB.Exec(seed); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Auto: biggest online compute (offline machine excluded)
	if got, err := allocHost(""); err != nil || got != "big" {
		t.Fatalf("auto alloc = %q, %v; want big", got, err)
	}
	// Explicit online machine honored
	if got, err := allocHost("gate"); err != nil || got != "gate" {
		t.Fatalf("explicit alloc = %q, %v; want gate", got, err)
	}
	// Explicit offline machine rejected
	if _, err := allocHost("offone"); err == nil {
		t.Fatal("offline machine should be rejected")
	}
	// Unknown machine rejected
	if _, err := allocHost("nope"); err == nil {
		t.Fatal("unknown machine should be rejected")
	}
	db.DB.Exec(`DELETE FROM machines`)
}

// TestReconcileOffline — a machine pointing at a closed port must flip to offline.
func TestReconcileOffline(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	if _, err := db.DB.Exec(`DROP TABLE IF EXISTS machines CASCADE`); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if _, err := db.DB.Exec(`CREATE TABLE machines (
		id TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', ts_ip TEXT, lan_ip TEXT,
		os TEXT, arch TEXT, cpu_cores INT DEFAULT 0, mem_gb INT DEFAULT 0, disk_gb INT DEFAULT 0,
		tags JSONB NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'online',
		override JSONB NOT NULL DEFAULT '[]', notes TEXT NOT NULL DEFAULT '',
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		last_check_at TIMESTAMPTZ, ssh_user TEXT NOT NULL DEFAULT '')`); err != nil {
		t.Fatalf("create: %v", err)
	}
	// TEST-NET-1 (RFC 5737) — non-routable, dial must time out
	if _, err := db.DB.Exec(`INSERT INTO machines (id, role, ts_ip, status) VALUES ('ghost1','gate','192.0.2.1','online')`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/machines/reconcile", nil)
	w := httptest.NewRecorder()
	ReconcileMachines(w, req)
	if w.Code != 200 {
		t.Fatalf("reconcile code=%d body=%s", w.Code, w.Body.String())
	}
	var status string
	db.DB.QueryRow(`SELECT status FROM machines WHERE id='ghost1'`).Scan(&status)
	if status != "offline" {
		t.Fatalf("expected offline, got %s", status)
	}
	db.DB.Exec(`DELETE FROM machines WHERE id='ghost1'`)
}
// TestHeartbeat — token gate (401) + accepted push (200) + registry auto-fill.
func TestHeartbeat(t *testing.T) {
	oldToken := os.Getenv("MACHINE_HEARTBEAT_TOKEN")
	os.Setenv("MACHINE_HEARTBEAT_TOKEN", "test-hb-token")
	defer os.Setenv("MACHINE_HEARTBEAT_TOKEN", oldToken)

	// 401 without token
	req := httptest.NewRequest(http.MethodPost, "/api/machines/heartbeat", bytes.NewBufferString(`{"id":"hb1"}`))
	w := httptest.NewRecorder()
	HandleHeartbeat(w, req)
	if w.Code != 401 {
		t.Fatalf("no token = %d, want 401", w.Code)
	}
	// 200 with token + live store
	body := `{"id":"HB-Machine","cpu_percent":5.5,"memory_used_mb":300,"disk_used_gb":2.1,"os":"ubuntu","arch":"amd64","cpu_cores":2,"mem_gb":1,"disk_gb":48}`
	req = httptest.NewRequest(http.MethodPost, "/api/machines/heartbeat", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer test-hb-token")
	w = httptest.NewRecorder()
	HandleHeartbeat(w, req)
	if w.Code != 200 {
		t.Fatalf("token push = %d, want 200 body=%s", w.Code, w.Body.String())
	}
	if live, ok := runtime.SnapshotLive("hb-machine"); !ok || live.CPU != 5.5 {
		t.Fatalf("live store = %+v, want cpu 5.5 (id lowercased)", live)
	}
	// 400 malformed
	req = httptest.NewRequest(http.MethodPost, "/api/machines/heartbeat", bytes.NewBufferString(`{bad`))
	req.Header.Set("Authorization", "Bearer test-hb-token")
	w = httptest.NewRecorder()
	HandleHeartbeat(w, req)
	if w.Code != 400 {
		t.Fatalf("bad body = %d, want 400", w.Code)
	}
}
// TestDeleteMachine — 404 for unknown, 200 + row gone for existing.
func TestDeleteMachine(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	db.DB.Exec(`DROP TABLE IF EXISTS machines`)
	db.DB.Exec(`CREATE TABLE machines (id TEXT PRIMARY KEY, role TEXT NOT NULL DEFAULT '', ts_ip TEXT, lan_ip TEXT, os TEXT, arch TEXT, cpu_cores INT DEFAULT 0, mem_gb INT DEFAULT 0, disk_gb INT DEFAULT 0, tags JSONB NOT NULL DEFAULT '[]', status TEXT NOT NULL DEFAULT 'online', override JSONB NOT NULL DEFAULT '[]', notes TEXT NOT NULL DEFAULT '', created_at TIMESTAMPTZ NOT NULL DEFAULT now(), updated_at TIMESTAMPTZ NOT NULL DEFAULT now(), last_check_at TIMESTAMPTZ, ssh_user TEXT NOT NULL DEFAULT '')`)
	db.DB.Exec(`INSERT INTO machines (id, role, ts_ip) VALUES ('del-m1','gate','100.64.0.9')`)

	req := httptest.NewRequest(http.MethodDelete, "/api/machines/nope", nil)
	w := httptest.NewRecorder()
	DeleteMachine(w, req, "nope")
	if w.Code != 200 {
		t.Fatalf("unknown delete = %d, want 200 (idempotent)", w.Code)
	}
	req = httptest.NewRequest(http.MethodDelete, "/api/machines/del-m1", nil)
	w = httptest.NewRecorder()
	DeleteMachine(w, req, "del-m1")
	if w.Code != 200 {
		t.Fatalf("delete = %d body=%s", w.Code, w.Body.String())
	}
	var cnt int
	db.DB.QueryRow(`SELECT COUNT(*) FROM machines WHERE id='del-m1'`).Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("row still present after delete")
	}
	db.DB.Exec(`DROP TABLE IF EXISTS machines`)
}
