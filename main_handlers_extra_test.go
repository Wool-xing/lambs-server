package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestSystemHealthNodesAndCPU — the nodes array carries both snapshots and
// a second call exercises the /proc cpu delta branch (0 on first call,
// non-negative after).
func TestSystemHealthNodesAndCPU(t *testing.T) {
	rr := httptest.NewRecorder()
	handleSystemHealth(rr, httptest.NewRequest("GET", "/api/system/health", nil))
	var body struct {
		Data struct {
			Nodes []map[string]interface{} `json:"nodes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(body.Data.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2 (wool + agent)", len(body.Data.Nodes))
	}

	rr2 := httptest.NewRecorder()
	handleSystemHealth(rr2, httptest.NewRequest("GET", "/api/system/health", nil))
	var body2 struct {
		Data struct {
			CPU float64 `json:"cpu_percent"`
		} `json:"data"`
	}
	json.Unmarshal(rr2.Body.Bytes(), &body2)
	if body2.Data.CPU < 0 {
		t.Errorf("cpu_percent = %v, want non-negative", body2.Data.CPU)
	}
}

// TestAggregatedLogsSAProjectRows — env-gated: the admin branch also serves
// project status rows (level warn for non-online statuses).
func TestAggregatedLogsSAProjectRows(t *testing.T) {
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
	// Full schema (round-9 CI lesson).
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
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('agg-proj', '聚合项目', 'offline')`)

	rr := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/logs/aggregated?lines=20", nil)
	r.Header.Set("X-Role", "super_admin")
	r.Header.Set("X-User-ID", "admin")
	handleAggregatedLogs(rr, r)
	if rr.Code != 200 {
		t.Fatalf("aggregated = %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "聚合项目") || !strings.Contains(body, "已离线") {
		t.Errorf("project status row missing: %s", body[:400])
	}
}
