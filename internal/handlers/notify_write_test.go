package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/runtime"
)

// TestPatchProjectStatusNotifies — the write-side contract for status
// changes (QA round 3 idea 1): one notification row with the right
// project_id/type/title must land when a status flips. Read-side authz
// is covered elsewhere; the write side had zero guards.
func TestPatchProjectStatusNotifies(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`DELETE FROM projects WHERE id='proj-x'; DELETE FROM notifications WHERE project_id='proj-x';`)
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('proj-x', '项目X', 'online')`)

	body, _ := json.Marshal(map[string]string{"status": "offline"})
	r := httptest.NewRequest("PATCH", "/api/projects/proj-x/status", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-User-ID", "admin")
	r.Header.Set("X-Role", "super_admin")
	r.SetPathValue("id", "proj-x")
	w := httptest.NewRecorder()
	PatchProjectStatus(w, r, "proj-x")
	if w.Code != 200 {
		t.Fatalf("patch = %d (body %s)", w.Code, w.Body.String())
	}

	var status string
	db.DB.QueryRow("SELECT status FROM projects WHERE id='proj-x'").Scan(&status)
	if status != "offline" {
		t.Errorf("status = %q, want offline", status)
	}
	var nid, ntype, title, content string
	err := db.DB.QueryRow("SELECT id, type, title, content FROM notifications WHERE project_id='proj-x' ORDER BY created_at DESC LIMIT 1").
		Scan(&nid, &ntype, &title, &content)
	if err != nil {
		t.Fatalf("no notification row written: %v", err)
	}
	if nid == "" || ntype != "status" || title != "项目状态变更" {
		t.Errorf("notification contract broken: id=%q type=%q title=%q", nid, ntype, title)
	}
	// Verb-form wording per the status-semantics design: 上线/已停用 are gone.
	if !strings.Contains(content, "已停用") {
		t.Errorf("notification content = %q, want 已停用", content)
	}

	// Non-admin (viewer) is rejected before any write.
	r2 := httptest.NewRequest("PATCH", "/api/projects/proj-x/status", bytes.NewReader(body))
	r2.Header.Set("X-User-ID", "viewer")
	r2.Header.Set("X-Role", "viewer")
	r2.SetPathValue("id", "proj-x")
	w2 := httptest.NewRecorder()
	PatchProjectStatus(w2, r2, "proj-x")
	if w2.Code != 403 {
		t.Errorf("viewer patch = %d, want 403", w2.Code)
	}
}

// TestPatchProjectStatusSameStatusNoop — the homomorphic guard: a PATCH whose
// status equals the current one must write NO notification and must NOT touch
// the TCP proxy (no restart, no stop) — it only persists offline_msg when
// provided. The proxy is asserted observably: a live listener must still
// accept connections after the same-status patch, and a real transition must
// close it (negative control proving the dial detects a stopped proxy).
func TestPatchProjectStatusSameStatusNoop(t *testing.T) {
	mustExec := puFixture(t)
	// Pick a free port for the proxy listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	mustExec(`INSERT INTO projects (id, name, status, port, backend_url) VALUES ('ps-same','同态','online',$1,'127.0.0.1:1')`, port)
	mustExec(`DELETE FROM notifications WHERE project_id='ps-same'`) // table persists across runs
	t.Cleanup(func() { runtime.TCPProxyMgr.Stop("ps-same") })

	patch := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/ps-same/status", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		PatchProjectStatus(w, r, "ps-same")
		return w
	}
	dial := func() error {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if c != nil {
			c.Close()
		}
		return err
	}
	if err := runtime.TCPProxyMgr.Start("ps-same"); err != nil {
		t.Fatalf("start proxy: %v", err)
	}
	if err := dial(); err != nil {
		t.Fatalf("proxy not listening before patch: %v", err)
	}

	// Same-status PATCH → 200, offline_msg persisted, NO notification, proxy alive.
	if w := patch(`{"status":"online","offline_msg":"同态消息"}`); w.Code != 200 {
		t.Fatalf("same-status = %d (body %s)", w.Code, w.Body.String())
	}
	var status, msg string
	db.DB.QueryRow("SELECT status, COALESCE(offline_msg,'') FROM projects WHERE id='ps-same'").Scan(&status, &msg)
	if status != "online" || msg != "同态消息" {
		t.Errorf("after same-status = status %q offline_msg %q, want online/同态消息", status, msg)
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE project_id='ps-same'").Scan(&n)
	if n != 0 {
		t.Errorf("same-status notifications = %d, want 0", n)
	}
	if err := dial(); err != nil {
		t.Errorf("proxy stopped by same-status patch: %v", err)
	}

	// Real transition → verb-form notification, and the proxy must stop
	// (proves the dial check above can detect a stopped proxy).
	if w := patch(`{"status":"offline"}`); w.Code != 200 {
		t.Fatalf("offline = %d (body %s)", w.Code, w.Body.String())
	}
	if err := dial(); err == nil {
		t.Error("proxy still listening after real transition to offline — Stop did not run")
	}
	var content string
	db.DB.QueryRow("SELECT content FROM notifications WHERE project_id='ps-same' AND type='status' ORDER BY created_at DESC LIMIT 1").Scan(&content)
	if !strings.Contains(content, "已停用") {
		t.Errorf("notification content = %q, want 已停用", content)
	}
}
