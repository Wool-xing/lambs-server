package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestDatasourcesAndAuditLogs — real PostgreSQL: datasource listing honors
// the project rows; audit logs paginate with a real total. Gated on
// LAMBS_TEST_PG_DSN.
func TestDatasourcesAndAuditLogs(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`DELETE FROM projects WHERE id='ds-proj'; DELETE FROM audit_logs;`)
	mustExec(`INSERT INTO projects (id, name, repo, stack, db_type, dsn, status) VALUES ('ds-proj', '数据源项目', 'ds-repo', 'Go+PG', 'PostgreSQL', 'postgres://x', 'online')`)
	mustExec(`INSERT INTO audit_logs (user_id, action, target, detail) VALUES ('u1','登录','Lambs','ok'), ('u1','改配置','设置','port'), ('u2','导出','项目','csv')`)

	sa := func(r *http.Request) {
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
	}

	// Datasources: dsn flows through, missing dsn renders as —.
	dr := httptest.NewRequest("GET", "/api/settings/datasources", nil)
	sa(dr)
	dw := httptest.NewRecorder()
	Datasources(dw, dr)
	if dw.Code != 200 {
		t.Fatalf("datasources = %d", dw.Code)
	}
	var dbody struct {
		Data struct {
			Datasources []map[string]interface{} `json:"datasources"`
		} `json:"data"`
	}
	json.Unmarshal(dw.Body.Bytes(), &dbody)
	found := false
	for _, d := range dbody.Data.Datasources {
		if d["id"] == "ds-proj" && d["dsn"] == "postgres://x" && d["db_type"] == "PostgreSQL" {
			found = true
		}
	}
	if !found {
		t.Errorf("datasources missing ds-proj: %v", dbody.Data.Datasources)
	}

	// AuditLogs with page_size=2: one page of 2, total 3.
	ar := httptest.NewRequest("GET", "/api/settings/audit-logs?page=1&page_size=2", nil)
	sa(ar)
	aw := httptest.NewRecorder()
	AuditLogs(aw, ar)
	if aw.Code != 200 {
		t.Fatalf("audit-logs = %d", aw.Code)
	}
	var abody struct {
		Data struct {
			Logs  []map[string]interface{} `json:"logs"`
			Total int                      `json:"total"`
		} `json:"data"`
	}
	json.Unmarshal(aw.Body.Bytes(), &abody)
	if abody.Data.Total != 3 || len(abody.Data.Logs) != 2 {
		t.Errorf("audit logs total=%d len=%d, want 3/2 (body %s)", abody.Data.Total, len(abody.Data.Logs), aw.Body.String())
	}

	// ExportProjects CSV: header + the row, content-type text/csv.
	er := httptest.NewRequest("GET", "/api/settings/export/projects", nil)
	sa(er)
	ew := httptest.NewRecorder()
	ExportProjects(ew, er)
	if ew.Code != 200 {
		t.Fatalf("export = %d (body %s)", ew.Code, ew.Body.String())
	}
	if ct := ew.Header().Get("Content-Type"); !strings.Contains(ct, "text/csv") {
		t.Errorf("content-type = %q", ct)
	}
	if !strings.Contains(ew.Body.String(), "ds-proj") || !strings.Contains(ew.Body.String(), "项目") {
		t.Errorf("csv missing row: %s", ew.Body.String()[:200])
	}
}
