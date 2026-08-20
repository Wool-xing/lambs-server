package handlers

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// rowReq builds a request against the data/row routes with super_admin auth.
func rowReq(method, id, query string, body map[string]interface{}) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/api/projects/"+id+"/data/row"+query, nil)
	if body != nil {
		b, _ := json.Marshal(body)
		req = httptest.NewRequest(method, "/api/projects/"+id+"/data/row"+query, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Role", "super_admin")
	w := httptest.NewRecorder()
	switch method {
	case "PUT":
		UpdateTableRow(w, req, id)
	case "DELETE":
		DeleteTableRow(w, req, id)
	case "POST":
		InsertTableRow(w, req, id)
	}
	return w
}

// TestDataRowCRUD — the write chain against real PG: insert lands in the
// table, update changes it, delete removes it (browse_users fixture).
func TestDataRowCRUD(t *testing.T) {
	browseSetup(t)
	q := "?table=browse_users&pk=id&pkval=3"

	// Insert.
	w := rowReq("POST", "browse-proj", "?table=browse_users", map[string]interface{}{
		"name": "dana", "password": "secret-d",
	})
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("insert = %d (body %s)", w.Code, w.Body.String())
	}

	// Verify via the browser chain.
	code, m := doBrowse(t, "browse-proj", "?table=browse_users&search=dana")
	if code != 200 {
		t.Fatalf("browse = %d", code)
	}
	rows, _ := m["rows"].([]interface{})
	if len(rows) != 1 || rows[0].(map[string]interface{})["name"] != "dana" {
		t.Fatalf("inserted row not visible: %v", rows)
	}

	// Update.
	w = rowReq("PUT", "browse-proj", q, map[string]interface{}{"name": "dana2"})
	if w.Code != 200 {
		t.Fatalf("update = %d (body %s)", w.Code, w.Body.String())
	}
	_, m = doBrowse(t, "browse-proj", "?table=browse_users&search=dana2")
	rows, _ = m["rows"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("updated row not visible: %v", rows)
	}

	// Delete.
	w = rowReq("DELETE", "browse-proj", q, nil)
	if w.Code != 200 {
		t.Fatalf("delete = %d (body %s)", w.Code, w.Body.String())
	}
	_, m = doBrowse(t, "browse-proj", "?table=browse_users&search=dana2")
	rows, _ = m["rows"].([]interface{})
	if len(rows) != 0 {
		t.Errorf("row survived delete: %v", rows)
	}
}

// TestDataRowGates — 403 without admin, missing params 400, pk-column
// injection rejected by the charset gate, hostile row keys rejected.
func TestDataRowGates(t *testing.T) {
	browseSetup(t)

	// 403: no role header.
	req := httptest.NewRequest("PUT", "/api/projects/browse-proj/data/row?table=browse_users&pk=id&pkval=1", nil)
	w := httptest.NewRecorder()
	UpdateTableRow(w, req, "browse-proj")
	if w.Code != 403 {
		t.Errorf("no-role = %d, want 403", w.Code)
	}

	// Missing table/pk → 400.
	w = rowReq("PUT", "browse-proj", "", map[string]interface{}{"name": "x"})
	if w.Code != 400 {
		t.Errorf("missing params = %d, want 400", w.Code)
	}

	// pk column injection → 400 (charset gate before any SQL). The quote
	// and spaces travel URL-encoded so the request can even be built.
	w = rowReq("PUT", "browse-proj", `?table=browse_users&pk=id%22%20OR%201%3D1%20--&pkval=1`, map[string]interface{}{"name": "x"})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "非法主键列名") {
		t.Errorf("pk injection = %d (%s), want 400 charset rejection", w.Code, w.Body.String())
	}

	// Hostile row key in body → 400.
	w = rowReq("PUT", "browse-proj", "?table=browse_users&pk=id&pkval=1", map[string]interface{}{"id; DROP": "x"})
	if w.Code != 400 {
		t.Errorf("hostile row key = %d, want 400", w.Code)
	}
}

// TestResetPassword — short/empty rejections, successful reset changes the
// hash and bumps token_version (all sessions die), audit row lands.
func TestResetPassword(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS users (
		id UUID PRIMARY KEY, username TEXT, name TEXT, email TEXT,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, token_version, pwd_salt)
		VALUES ('11111111-2222-3333-4444-555555555555','reset-user','重置','r@t.st','oldhash','viewer',3,'salt-1')
		ON CONFLICT (id) DO UPDATE SET password_hash='oldhash', token_version=3`)
	mustExec(`DELETE FROM audit_logs WHERE target='reset-user'`)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/users/11111111-2222-3333-4444-555555555555/reset-password", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Username", "admin")
		w := httptest.NewRecorder()
		ResetPassword(w, r, "11111111-2222-3333-4444-555555555555")
		return w
	}

	if w := post(`{"new_password":""}`); w.Code != 400 {
		t.Errorf("empty = %d, want 400", w.Code)
	}
	if w := post(`{"new_password":"12345"}`); w.Code != 400 {
		t.Errorf("short = %d, want 400", w.Code)
	}

	// Success: hash changes, token_version bumps, audit row lands.
	if w := post(`{"new_password":"newpass-123"}`); w.Code != 200 {
		t.Fatalf("reset = %d (body %s)", w.Code, w.Body.String())
	}
	var hash string
	var tv int
	db.DB.QueryRow("SELECT password_hash, token_version FROM users WHERE id='11111111-2222-3333-4444-555555555555'").Scan(&hash, &tv)
	if hash == "oldhash" {
		t.Error("password_hash unchanged")
	}
	if tv != 4 {
		t.Errorf("token_version = %d, want 4", tv)
	}
	var n int
	db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs WHERE target='reset-user' AND action='重置密码'").Scan(&n)
	if n != 1 {
		t.Errorf("audit rows = %d, want 1", n)
	}
	mustExec(`DELETE FROM users WHERE id='11111111-2222-3333-4444-555555555555'`)
}

// TestGetProject — super_admin reads (icon URL rewritten), unknown id 404,
// non-admin without access 403.
func TestGetProject(t *testing.T) {
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
	// Full-column table: other tests in this package may have replaced the
	// shared projects table with a stub schema (GetProject selects 30 cols).
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
	mustExec(`INSERT INTO projects (id, name, icon_url) VALUES ('gp-proj','取项目','http://x/icon.png')`)

	get := func(role, userID string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/projects/gp-proj", nil)
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", userID)
		w := httptest.NewRecorder()
		GetProject(w, r, "gp-proj")
		return w
	}

	// Bare-query control: the row is visible through the same handle.
	var ctlName string
	if err := db.DB.QueryRow("SELECT name FROM projects WHERE id='gp-proj'").Scan(&ctlName); err != nil || ctlName != "取项目" {
		t.Fatalf("control query: %q err=%v", ctlName, err)
	}

	// super_admin: 200, icon URL rewritten to the served path.
	w := get("super_admin", "admin")
	if w.Code != 200 {
		t.Fatalf("super_admin = %d (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/api/projects/gp-proj/logo") {
		t.Errorf("icon URL not rewritten: %s", w.Body.String())
	}

	// Non-admin without access → 403.
	mustExec(`CREATE TABLE IF NOT EXISTS users (id UUID PRIMARY KEY, username TEXT, name TEXT, email TEXT, password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active', token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '', project_access JSONB DEFAULT '[]', created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`INSERT INTO users (id, username, project_access) VALUES ('99999999-2222-3333-4444-555555555555','viewer-user','[]')
		ON CONFLICT (id) DO UPDATE SET project_access='[]'`)
	if w := get("viewer", "99999999-2222-3333-4444-555555555555"); w.Code != 403 {
		t.Errorf("viewer no-access = %d, want 403", w.Code)
	}

	// Unknown id → 404.
	r := httptest.NewRequest("GET", "/api/projects/nope-id", nil)
	r.Header.Set("X-Role", "super_admin")
	wn := httptest.NewRecorder()
	GetProject(wn, r, "nope-id")
	if wn.Code != 404 {
		t.Errorf("unknown = %d, want 404", wn.Code)
	}
	mustExec(`DELETE FROM projects WHERE id='gp-proj'`)
}

// TestPinProjectToggle — 403 without admin; pin toggles false→true→false
// in the DB and the response reports the new state.
func TestPinProjectToggle(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT, is_pinned BOOLEAN DEFAULT false)`)
	mustExec(`INSERT INTO projects (id, name, is_pinned) VALUES ('pin-proj','钉项目',false)
		ON CONFLICT (id) DO UPDATE SET is_pinned=false`)

	call := func(admin bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/pin-proj/pin", nil)
		if admin {
			r.Header.Set("X-Role", "super_admin")
		}
		w := httptest.NewRecorder()
		PinProject(w, r, "pin-proj")
		return w
	}

	if w := call(false); w.Code != 403 {
		t.Errorf("no-role = %d, want 403", w.Code)
	}
	// false → true.
	if w := call(true); w.Code != 200 || !strings.Contains(w.Body.String(), `"is_pinned":true`) {
		t.Fatalf("first toggle = %d (%s)", w.Code, w.Body.String())
	}
	var pinned bool
	db.DB.QueryRow("SELECT is_pinned FROM projects WHERE id='pin-proj'").Scan(&pinned)
	if !pinned {
		t.Error("is_pinned not persisted after toggle")
	}
	// true → false.
	call(true)
	db.DB.QueryRow("SELECT is_pinned FROM projects WHERE id='pin-proj'").Scan(&pinned)
	if pinned {
		t.Error("second toggle did not unpin")
	}
	mustExec(`DELETE FROM projects WHERE id='pin-proj'`)
}

// TestReorderProjects — both payload formats land sort_order in the DB.
func TestReorderProjects(t *testing.T) {
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
	mustExec(`CREATE TABLE IF NOT EXISTS projects (id TEXT PRIMARY KEY, name TEXT, sort_order INT DEFAULT 0)`)
	mustExec(`INSERT INTO projects (id, name) VALUES ('ro-a','A'),('ro-b','B'),('ro-c','C')
		ON CONFLICT (id) DO NOTHING`)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/reorder", strings.NewReader(body))
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		ReorderProjects(w, r)
		return w
	}

	// Format 1: ordered_ids.
	if w := post(`{"ordered_ids":["ro-c","ro-a","ro-b"]}`); w.Code != 200 {
		t.Fatalf("format1 = %d (%s)", w.Code, w.Body.String())
	}
	var o1, o2, o3 int
	db.DB.QueryRow("SELECT sort_order FROM projects WHERE id='ro-c'").Scan(&o1)
	db.DB.QueryRow("SELECT sort_order FROM projects WHERE id='ro-a'").Scan(&o2)
	db.DB.QueryRow("SELECT sort_order FROM projects WHERE id='ro-b'").Scan(&o3)
	if o1 != 1 || o2 != 2 || o3 != 3 {
		t.Errorf("format1 orders = %d/%d/%d, want 1/2/3", o1, o2, o3)
	}

	// Format 2: [{id, sort_order}].
	if w := post(`[{"id":"ro-a","sort_order":9},{"id":"ro-b","sort_order":8}]`); w.Code != 200 {
		t.Fatalf("format2 = %d (%s)", w.Code, w.Body.String())
	}
	db.DB.QueryRow("SELECT sort_order FROM projects WHERE id='ro-a'").Scan(&o2)
	db.DB.QueryRow("SELECT sort_order FROM projects WHERE id='ro-b'").Scan(&o3)
	if o2 != 9 || o3 != 8 {
		t.Errorf("format2 orders = %d/%d, want 9/8", o2, o3)
	}
	mustExec(`DELETE FROM projects WHERE id IN ('ro-a','ro-b','ro-c')`)
}
