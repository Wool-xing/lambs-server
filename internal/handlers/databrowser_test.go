package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
)

// browseSetup seeds a project whose datasource is the test postgres itself,
// plus a browse_users table with sensitive columns. Real postgres, gated on
// LAMBS_TEST_PG_DSN.
func browseSetup(t *testing.T) {
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
	mustExec(`DELETE FROM projects WHERE id='browse-proj'`)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('browse-proj','浏览测试',$1)`, dsn)
	mustExec(`DROP TABLE IF EXISTS browse_users`)
	mustExec(`CREATE TABLE browse_users (id serial PRIMARY KEY, name text NOT NULL, password text NOT NULL)`)
	mustExec(`INSERT INTO browse_users (name, password) VALUES ('alice','secret-a'),('bob','secret-b'),('carol','secret-c')`)
	t.Cleanup(func() {
		mustExec(`DROP TABLE IF EXISTS browse_users`)
		mustExec(`DELETE FROM projects WHERE id='browse-proj'`)
	})
}

// doBrowse hits ProjectTables/ListTableNames directly with super_admin auth
// and returns the parsed envelope body.
func doBrowse(t *testing.T, id, query string) (int, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/projects/"+id+"/tables"+query, nil)
	req.Header.Set("X-Role", "super_admin")
	rec := httptest.NewRecorder()
	ProjectTables(rec, req, id)
	var env models.ApiResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal response: %v (%s)", err, rec.Body.String())
	}
	m, _ := env.Data.(map[string]interface{})
	return rec.Code, m
}

func TestListTableNames(t *testing.T) {
	browseSetup(t)
	req := httptest.NewRequest("GET", "/api/projects/browse-proj/tables/list", nil)
	req.Header.Set("X-Role", "super_admin")
	rec := httptest.NewRecorder()
	ListTableNames(rec, req, "browse-proj")
	var env models.ApiResponse
	json.Unmarshal(rec.Body.Bytes(), &env)
	if !env.Success {
		t.Fatalf("ListTableNames failed: %s", env.Error)
	}
	m, _ := env.Data.(map[string]interface{})
	tables, _ := m["tables"].([]interface{})
	found := false
	for _, tb := range tables {
		if tb == "browse_users" {
			found = true
		}
	}
	if !found {
		t.Errorf("tables = %v, want browse_users", tables)
	}
}

// TestProjectTablesRedact — password column must be dropped from both the
// column list and every row by the single-point redaction.
func TestProjectTablesRedact(t *testing.T) {
	browseSetup(t)
	code, m := doBrowse(t, "browse-proj", "?table=browse_users")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	cols, _ := m["columns"].([]interface{})
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c.(string)), "password") {
			t.Errorf("sensitive column leaked: %v", cols)
		}
	}
	rows, _ := m["rows"].([]interface{})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	first := rows[0].(map[string]interface{})
	if _, ok := first["password"]; ok {
		t.Error("password key leaked in row data")
	}
	if first["name"] != "alice" {
		t.Errorf("name = %v, want alice", first["name"])
	}
}

func TestProjectTablesPagination(t *testing.T) {
	browseSetup(t)
	code, m := doBrowse(t, "browse-proj", "?table=browse_users&page=2&page_size=2")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	rows, _ := m["rows"].([]interface{})
	if len(rows) != 1 {
		t.Errorf("page 2 rows = %d, want 1", len(rows))
	}
	if total := int(m["total"].(float64)); total != 3 {
		t.Errorf("total = %d, want 3", total)
	}
}

func TestProjectTablesSearch(t *testing.T) {
	browseSetup(t)
	code, m := doBrowse(t, "browse-proj", "?table=browse_users&search=carol")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	rows, _ := m["rows"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].(map[string]interface{})["name"] != "carol" {
		t.Errorf("search hit = %v, want carol", rows[0])
	}
}

func TestProjectTablesSortDesc(t *testing.T) {
	browseSetup(t)
	code, m := doBrowse(t, "browse-proj", "?table=browse_users&sort_col=name&sort_dir=desc")
	if code != 200 {
		t.Fatalf("code = %d", code)
	}
	rows, _ := m["rows"].([]interface{})
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if rows[0].(map[string]interface{})["name"] != "carol" {
		t.Errorf("first row = %v, want carol (desc)", rows[0].(map[string]interface{})["name"])
	}
}

func TestProjectTablesCSV(t *testing.T) {
	browseSetup(t)
	req := httptest.NewRequest("GET", "/api/projects/browse-proj/tables?table=browse_users&format=csv", nil)
	req.Header.Set("X-Role", "super_admin")
	rec := httptest.NewRecorder()
	ProjectTables(rec, req, "browse-proj")
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "name") || strings.Contains(strings.ToLower(body), "password") {
		t.Errorf("csv body missing name col or leaking password: %s", body)
	}
}

// TestProjectTablesAuth — without a role header the data browser must 403.
func TestProjectTablesAuth(t *testing.T) {
	browseSetup(t)
	req := httptest.NewRequest("GET", "/api/projects/browse-proj/tables?table=browse_users", nil)
	rec := httptest.NewRecorder()
	ProjectTables(rec, req, "browse-proj")
	if rec.Code != 403 {
		t.Errorf("code = %d, want 403", rec.Code)
	}
}
