package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/runtime"
)

const projectsDDL = `CREATE TABLE projects (
	id TEXT PRIMARY KEY, name TEXT NOT NULL DEFAULT '', repo TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '', icon_url TEXT NOT NULL DEFAULT '',
	icon_thumb TEXT NOT NULL DEFAULT '', stack TEXT NOT NULL DEFAULT '',
	port TEXT NOT NULL DEFAULT '', db_type TEXT NOT NULL DEFAULT '', dsn TEXT NOT NULL DEFAULT '',
	users_count INT NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'online', sort_order INT NOT NULL DEFAULT 0,
	is_pinned BOOLEAN NOT NULL DEFAULT false,
	icon_cls TEXT NOT NULL DEFAULT '', base_path TEXT NOT NULL DEFAULT '',
	backend_url TEXT NOT NULL DEFAULT '', service_name TEXT NOT NULL DEFAULT '',
	startup_command TEXT NOT NULL DEFAULT '', health_url TEXT NOT NULL DEFAULT '',
	tags JSONB DEFAULT '[]', offline_msg TEXT NOT NULL DEFAULT '',
	features JSONB DEFAULT '[]', tabs JSONB DEFAULT '[]',
	datasources JSONB DEFAULT '[]',
	services JSONB DEFAULT '[]', created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	backup_interval_hours INT NOT NULL DEFAULT 0, backup_retention_days INT NOT NULL DEFAULT 0)`

const usersDDL = `CREATE TABLE users (
	id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
	password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
	token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
	project_access JSONB NOT NULL DEFAULT '[]',
	avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
	last_login TIMESTAMPTZ DEFAULT now(),
	created_at TIMESTAMPTZ DEFAULT now())`

// puFixture rebuilds the projects + users + audit/notifications tables the
// project/user handlers read and write (real PostgreSQL, DSN-gated).
func puFixture(t *testing.T) func(string, ...interface{}) {
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
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS projects CASCADE`)
	mustExec(projectsDDL)
	mustExec(`DROP TABLE IF EXISTS users CASCADE`)
	mustExec(usersDDL)
	mustExec(`CREATE TABLE IF NOT EXISTS audit_logs (id SERIAL PRIMARY KEY, user_id TEXT, user_name TEXT, action TEXT, target TEXT, detail TEXT, created_at TIMESTAMPTZ DEFAULT now())`)
	mustExec(`CREATE TABLE IF NOT EXISTS notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	return mustExec
}

// TestResolveDatasource — the full branch matrix: legacy fallback, found id,
// id without dsn, missing id, missing project.
func TestResolveDatasource(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, datasources) VALUES ('rs-p', '解析', '[{"id":"ds1","name":"主","dsn":"sqlite:///x.db","type":"SQLite","is_primary":true},{"id":"ds2","name":"未配置","type":"SQLite"}]'::jsonb)`)

	if dsn, err := resolveDatasource("rs-p", "", "fallback-dsn"); err != nil || dsn != "fallback-dsn" {
		t.Errorf("empty dsID = %q err=%v, want fallback", dsn, err)
	}
	if dsn, err := resolveDatasource("rs-p", "ds1", ""); err != nil || dsn != "sqlite:///x.db" {
		t.Errorf("ds1 = %q err=%v, want sqlite dsn", dsn, err)
	}
	if _, err := resolveDatasource("rs-p", "ds2", ""); err == nil || !strings.Contains(err.Error(), "未配置连接串") {
		t.Errorf("ds2 err = %v, want 未配置连接串", err)
	}
	if _, err := resolveDatasource("rs-p", "ds9", ""); err == nil || !strings.Contains(err.Error(), "数据源不存在") {
		t.Errorf("ds9 err = %v, want 数据源不存在", err)
	}
	if _, err := resolveDatasource("rs-none", "ds1", ""); err == nil || !strings.Contains(err.Error(), "项目不存在") {
		t.Errorf("missing project err = %v, want 项目不存在", err)
	}
}

// TestCheckAccessHelpers — access matrix for CheckProjectAccess and
// CheckProjectView against seeded users.
func TestCheckAccessHelpers(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000001','ac-admin','super_admin','[]'),
		('10000000-0000-0000-0000-000000000002','ac-pa','project_admin','["proj-x"]'),
		('10000000-0000-0000-0000-000000000003','ac-pa2','project_admin','[]'),
		('10000000-0000-0000-0000-000000000004','ac-v','viewer','["proj-x"]')`)

	req := func(role, uid string) *http.Request {
		r := httptest.NewRequest("GET", "/api/x", nil)
		if role != "" {
			r.Header.Set("X-Role", role)
		}
		if uid != "" {
			r.Header.Set("X-User-ID", uid)
		}
		return r
	}

	cases := []struct {
		name string
		role string
		uid  string
		want bool
	}{
		{"super admin", "super_admin", "10000000-0000-0000-0000-000000000001", true},
		{"project admin with access", "project_admin", "10000000-0000-0000-0000-000000000002", true},
		{"project admin no access", "project_admin", "10000000-0000-0000-0000-000000000003", false},
		{"viewer", "viewer", "10000000-0000-0000-0000-000000000004", false},
		{"no role", "", "10000000-0000-0000-0000-000000000004", false},
		{"missing user row", "project_admin", "10000000-0000-0000-0000-0000000000ff", false},
	}
	for _, c := range cases {
		rr := req(c.role, c.uid)
		if got := CheckProjectAccess(rr, "proj-x"); got != c.want {
			t.Errorf("CheckProjectAccess(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	vCases := []struct {
		name string
		role string
		uid  string
		want bool
	}{
		{"super admin", "super_admin", "10000000-0000-0000-0000-000000000001", true},
		{"viewer with access", "viewer", "10000000-0000-0000-0000-000000000004", true},
		{"viewer no access", "viewer", "10000000-0000-0000-0000-000000000003", false},
		{"missing user row", "viewer", "ac-none", false},
	}
	for _, c := range vCases {
		rr := req(c.role, c.uid)
		if got := CheckProjectView(rr, "proj-x"); got != c.want {
			t.Errorf("CheckProjectView(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestListProjectsFilters — status/search filters, sort orders, icon URL
// rewrite, JSON-string jsonb decode, and non-admin scoping + masking.
func TestListProjectsFilters(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, repo, icon_url, users_count, status, sort_order, is_pinned, dsn, datasources, services, tags) VALUES
		('lp-p1','apples','a','http://x/i.png',2,'online',3,true,'postgres://x','[{"id":"ds1","name":"主","dsn":"postgres://x","type":"PostgreSQL","is_primary":true}]'::jsonb,'[{"name":"svc1"}]'::jsonb,'["t1"]'::jsonb),
		('lp-p2','bananas','b','',9,'offline',1,false,'—','"[]"'::jsonb,'"[]"'::jsonb,'"[\"t1\",\"t2\"]"'::jsonb),
		('lp-p3','carrots','c','',5,'maintenance',2,false,'','[]'::jsonb,'[]'::jsonb,'[]'::jsonb)`)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000005','lp-viewer','viewer','["lp-p1"]'),
		('10000000-0000-0000-0000-000000000006','lp-empty','viewer','[]')`)

	list := func(role, uid, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/projects"+query, nil)
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", uid)
		w := httptest.NewRecorder()
		ListProjects(w, r)
		return w
	}
	ids := func(w *httptest.ResponseRecorder) []string {
		var env struct {
			Data struct {
				Projects []map[string]interface{} `json:"projects"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &env)
		out := []string{}
		for _, p := range env.Data.Projects {
			out = append(out, p["id"].(string))
		}
		return out
	}

	// status filter
	w := list("super_admin", "admin", "?status=online")
	if got := ids(w); len(got) != 1 || got[0] != "lp-p1" {
		t.Errorf("status=online = %v, want [lp-p1]", got)
	}
	// search filter
	w = list("super_admin", "admin", "?search=ban")
	if got := ids(w); len(got) != 1 || got[0] != "lp-p2" {
		t.Errorf("search=ban = %v, want [lp-p2]", got)
	}
	// sort_by=users: pinned lp-p1 first, then lp-p2 (9 > 5)
	w = list("super_admin", "admin", "?sort_by=users")
	if got := ids(w); len(got) != 3 || got[1] != "lp-p2" {
		t.Errorf("sort_by=users = %v, want lp-p2 second", got)
	}
	// sort_by=name: apples pinned, then bananas
	w = list("super_admin", "admin", "?sort_by=name")
	if got := ids(w); len(got) != 3 || got[1] != "lp-p2" {
		t.Errorf("sort_by=name = %v, want lp-p2 second", got)
	}
	// default sort_order: lp-p2(1) then lp-p3(2)
	w = list("super_admin", "admin", "")
	if got := ids(w); len(got) != 3 || got[1] != "lp-p2" || got[2] != "lp-p3" {
		t.Errorf("default sort = %v, want [lp-p1 lp-p2 lp-p3]", got)
	}
	// icon URL rewritten for projects with one
	if !strings.Contains(w.Body.String(), "/api/projects/lp-p1/logo?v=") {
		t.Errorf("icon URL not rewritten: %s", w.Body.String())
	}
	// JSON-string jsonb decoded into arrays (lp-p2 tags)
	var env struct {
		Data struct {
			Projects []map[string]interface{} `json:"projects"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	for _, p := range env.Data.Projects {
		if p["id"] == "lp-p2" {
			tags, _ := p["tags"].([]interface{})
			if len(tags) != 2 || tags[0] != "t1" {
				t.Errorf("lp-p2 tags = %v, want [t1 t2]", p["tags"])
			}
		}
	}
	// non-admin scoped to access list, dsn/datasources/services masked
	w = list("viewer", "10000000-0000-0000-0000-000000000005", "")
	if got := ids(w); len(got) != 1 || got[0] != "lp-p1" {
		t.Errorf("viewer list = %v, want [lp-p1]", got)
	}
	var venv struct {
		Data struct {
			Projects []map[string]interface{} `json:"projects"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &venv)
	if len(venv.Data.Projects) == 0 {
		t.Fatal("viewer list empty")
	}
	p := venv.Data.Projects[0]
	if p["dsn"] != "—" {
		t.Errorf("viewer dsn = %v, want — (masked)", p["dsn"])
	}
	if ds, _ := p["datasources"].([]interface{}); len(ds) != 0 {
		t.Errorf("viewer datasources = %v, want []", p["datasources"])
	}
	if sv, _ := p["services"].([]interface{}); len(sv) != 0 {
		t.Errorf("viewer services = %v, want []", p["services"])
	}
	// non-admin with empty access → empty list
	w = list("viewer", "10000000-0000-0000-0000-000000000006", "")
	var eenv struct {
		Data struct {
			Projects []interface{} `json:"projects"`
			Total    int           `json:"total"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &eenv)
	if len(eenv.Data.Projects) != 0 || eenv.Data.Total != 0 {
		t.Errorf("empty-access viewer = %v/%v, want empty", eenv.Data.Projects, eenv.Data.Total)
	}
	// non-admin user row missing → same empty result
	w = list("viewer", "lp-none", "")
	json.Unmarshal(w.Body.Bytes(), &eenv)
	if len(eenv.Data.Projects) != 0 {
		t.Errorf("missing-user viewer = %v, want empty", eenv.Data.Projects)
	}
}

// TestGetProjectViewerAndStats — viewer with access reads a masked project
// (dsn/datasources/services hidden, stat cards still computed), JSON-string
// jsonb decoded, and the datasource stats path replaces features.
func TestGetProjectViewerAndStats(t *testing.T) {
	mustExec := puFixture(t)
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	mustExec(`INSERT INTO projects (id, name, repo, db_type, dsn, tags, features, datasources, services) VALUES
		('gp-v','取项目','gp','PostgreSQL',$1,'"[\"a\"]"'::jsonb,'[]'::jsonb,'[{"id":"ds1","name":"主","dsn":"postgres://x"}]'::jsonb,'[{"name":"svc"}]'::jsonb),
		('gp-nodsn','无源','gp2','SQLite','—','[]'::jsonb,'[]'::jsonb,'[]'::jsonb,'[]'::jsonb)`, dsn)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000007','gp-viewer','viewer','["gp-v"]')`)

	get := func(role, uid, id string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/projects/"+id, nil)
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", uid)
		w := httptest.NewRecorder()
		GetProject(w, r, id)
		return w
	}

	// viewer with access: 200, masked dsn/datasources/services, stat cards
	// still computed from the live datasource (computed before masking).
	w := get("viewer", "10000000-0000-0000-0000-000000000007", "gp-v")
	if w.Code != 200 {
		t.Fatalf("viewer get = %d (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	d := env.Data
	if d["dsn"] != "—" {
		t.Errorf("viewer dsn = %v, want —", d["dsn"])
	}
	if ds, _ := d["datasources"].([]interface{}); len(ds) != 0 {
		t.Errorf("viewer datasources = %v, want []", d["datasources"])
	}
	if tags, _ := d["tags"].([]interface{}); len(tags) != 1 || tags[0] != "a" {
		t.Errorf("gp-v tags = %v, want [a]", d["tags"])
	}
	feats, _ := d["features"].([]interface{})
	found := false
	for _, f := range feats {
		if m, ok := f.(map[string]interface{}); ok && m["label"] == "表数量" {
			found = true
		}
	}
	if !found {
		t.Errorf("stats cards not computed: features = %v", d["features"])
	}

	// super_admin sees the real dsn and datasources
	w = get("super_admin", "admin", "gp-v")
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Data["dsn"] != dsn {
		t.Errorf("super_admin dsn = %v, want %v", env.Data["dsn"], dsn)
	}
	if ds, _ := env.Data["datasources"].([]interface{}); len(ds) != 1 {
		t.Errorf("super_admin datasources = %v, want 1 entry", env.Data["datasources"])
	}

	// dsn '—' skips the stats path; stored features survive
	w = get("super_admin", "admin", "gp-nodsn")
	json.Unmarshal(w.Body.Bytes(), &env)
	if feats, _ := env.Data["features"].([]interface{}); len(feats) != 0 {
		t.Errorf("gp-nodsn features = %v, want [] (stats skipped)", env.Data["features"])
	}
}

// TestCreateProjectModes — invalid JSON, auto id from repo, datasource
// normalization from array and string forms, service dedupe, empty-port
// runtime allocation, auto-generated avatar thumbnail.
func TestCreateProjectModes(t *testing.T) {
	mustExec := puFixture(t)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/projects", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		CreateProject(w, r)
		return w
	}

	// invalid JSON → 400
	if w := post("{not-json"); w.Code != 400 {
		t.Errorf("bad json = %d, want 400", w.Code)
	}

	// empty port → runtime allocation; id derived from repo
	// auto-create allocates a PortMgr port — free it so 90-test port space
	// never exhausts (SQL deletes below bypass PortMgr.Free).
	t.Cleanup(func() { runtime.PortMgr.Free("auto-repo") })

	w := post(`{"repo":"auto-repo","name":"自动","db_type":"PostgreSQL","status":"offline"}`)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("auto create = %d (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Data["id"] != "auto-repo" {
		t.Errorf("auto id = %v, want auto-repo", env.Data["id"])
	}
	port, _ := env.Data["port"].(string)
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		t.Errorf("allocated port = %q, want 1-65535", port)
	}

	// datasources array form: normalized ids, primary mirrors dsn/db_type
	w = post(`{"id":"cp-arr","name":"数组","repo":"cp-arr","status":"offline","datasources":[{"name":"主","type":"MySQL","dsn":"mysql://n"}]}`)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("arr create = %d (body %s)", w.Code, w.Body.String())
	}
	var dsnStr, dbType string
	mustExecRow := func(q string, out ...interface{}) {
		if err := db.DB.QueryRow(q).Scan(out...); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	mustExecRow(`SELECT dsn, db_type FROM projects WHERE id='cp-arr'`, &dsnStr, &dbType)
	if dsnStr != "mysql://n" || dbType != "MySQL" {
		t.Errorf("primary mirror = %q/%q, want mysql://n/MySQL", dsnStr, dbType)
	}
	var dsText string
	mustExecRow(`SELECT datasources::text FROM projects WHERE id='cp-arr'`, &dsText)
	if !strings.Contains(dsText, `"id": "ds1"`) || !strings.Contains(dsText, `"is_primary": true`) {
		t.Errorf("normalized datasources = %s", dsText)
	}

	// datasources as JSON string form
	w = post(`{"id":"cp-str","name":"字符串","repo":"cp-str","status":"offline","datasources":"[{\"id\":\"ds9\",\"name\":\"旧\",\"dsn\":\"postgres://old\",\"type\":\"PostgreSQL\"}]"}`)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("str create = %d (body %s)", w.Code, w.Body.String())
	}
	mustExecRow(`SELECT dsn FROM projects WHERE id='cp-str'`, &dsnStr)
	if dsnStr != "postgres://old" {
		t.Errorf("string-form dsn = %q, want postgres://old", dsnStr)
	}

	// services dedupe by name
	w = post(`{"id":"cp-svc","name":"服务","repo":"cp-svc","status":"offline","services":[{"name":"s1"},{"name":"s1"},{"name":"s2"}]}`)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("svc create = %d (body %s)", w.Code, w.Body.String())
	}
	var svcText string
	mustExecRow(`SELECT services::text FROM projects WHERE id='cp-svc'`, &svcText)
	if strings.Count(svcText, `"s1"`) != 1 || !strings.Contains(svcText, `"s2"`) {
		t.Errorf("deduped services = %s", svcText)
	}

	// avatar data URL → thumbnail persisted
	av := pngDataURL(t, 600, 400, false)
	w = post(`{"id":"cp-av","name":"头像","repo":"cp-av","status":"offline","icon_url":"` + av + `"}`)
	if w.Code != 200 && w.Code != 201 {
		t.Fatalf("avatar create = %d (body %s)", w.Code, w.Body.String())
	}
	var thumb string
	mustExecRow(`SELECT COALESCE(icon_thumb,'') FROM projects WHERE id='cp-av'`, &thumb)
	if thumb == "" {
		t.Error("icon_thumb empty — thumbnail not generated")
	}

	mustExec(`DELETE FROM projects WHERE id IN ('auto-repo','cp-arr','cp-str','cp-svc','cp-av')`)
}

// TestUpdateProjectGuards — 403 gates, payload validation, port range,
// non-super_admin process-field stripping, and super_admin datasource/
// service replacement with interval/retention keep-current semantics.
func TestUpdateProjectGuards(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, repo, dsn, db_type, startup_command, service_name, backup_interval_hours, backup_retention_days, datasources, services) VALUES
		('up-p','原名','up','postgres://orig','PostgreSQL','orig-cmd','orig-svc',5,10,'[{"id":"ds1","name":"主","dsn":"postgres://orig","type":"PostgreSQL","is_primary":true}]'::jsonb,'[{"name":"old-svc"}]'::jsonb)`)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000020','up-pa','project_admin','["up-p"]'),
		('10000000-0000-0000-0000-000000000021','up-pa2','project_admin','[]')`)

	put := func(role, uid, id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/projects/"+id, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", uid)
		w := httptest.NewRecorder()
		UpdateProject(w, r, id)
		return w
	}

	// viewer → 403; project_admin without access → 403
	if w := put("viewer", "10000000-0000-0000-0000-000000000021", "up-p", `{"name":"x"}`); w.Code != 403 {
		t.Errorf("viewer = %d, want 403", w.Code)
	}
	if w := put("project_admin", "10000000-0000-0000-0000-000000000021", "up-p", `{"name":"x"}`); w.Code != 403 {
		t.Errorf("project_admin no access = %d, want 403", w.Code)
	}
	// invalid JSON → 400
	if w := put("super_admin", "admin", "up-p", "{bad"); w.Code != 400 {
		t.Errorf("bad json = %d, want 400", w.Code)
	}
	// port validation → 400
	if w := put("super_admin", "admin", "up-p", `{"port":"99999"}`); w.Code != 400 {
		t.Errorf("port 99999 = %d, want 400", w.Code)
	}
	if w := put("super_admin", "admin", "up-p", `{"port":"abc"}`); w.Code != 400 {
		t.Errorf("port abc = %d, want 400", w.Code)
	}
	// unknown project → 404
	if w := put("super_admin", "admin", "up-none", `{"name":"x"}`); w.Code != 404 {
		t.Errorf("unknown = %d, want 404", w.Code)
	}

	// project_admin: process fields stripped, current values kept
	w := put("project_admin", "10000000-0000-0000-0000-000000000020", "up-p", `{"name":"管理员改名","startup_command":"evil","service_name":"evil","port":"12345"}`)
	if w.Code != 200 {
		t.Fatalf("project_admin update = %d (body %s)", w.Code, w.Body.String())
	}
	var dsnStr, cmd, svc, port string
	mustExecRow := func(q string, out ...interface{}) {
		if err := db.DB.QueryRow(q).Scan(out...); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	mustExecRow(`SELECT dsn, startup_command, service_name, port FROM projects WHERE id='up-p'`, &dsnStr, &cmd, &svc, &port)
	if dsnStr != "postgres://orig" || cmd != "orig-cmd" || svc != "orig-svc" || port == "12345" {
		t.Errorf("stripped fields leaked: dsn=%q cmd=%q svc=%q port=%q", dsnStr, cmd, svc, port)
	}

	// super_admin: datasources/services replaced + deduped, interval updated,
	// retention kept (absent from payload)
	w = put("super_admin", "admin", "up-p", `{"name":"超管改","backup_interval_hours":9,"datasources":[{"name":"新","type":"MySQL","dsn":"mysql://n","is_primary":true},{"name":"新2","type":"MySQL","dsn":"mysql://n2"}],"services":[{"name":"s1"},{"name":"s1"},{"name":"s2"}]}`)
	if w.Code != 200 {
		t.Fatalf("super_admin update = %d (body %s)", w.Code, w.Body.String())
	}
	var dbType string
	var interval, retention int
	var dsText, svcText string
	mustExecRow(`SELECT dsn, db_type, backup_interval_hours, backup_retention_days, datasources::text, services::text FROM projects WHERE id='up-p'`, &dsnStr, &dbType, &interval, &retention, &dsText, &svcText)
	if dsnStr != "mysql://n" || dbType != "MySQL" {
		t.Errorf("primary mirror = %q/%q, want mysql://n/MySQL", dsnStr, dbType)
	}
	if interval != 9 || retention != 10 {
		t.Errorf("interval/retention = %d/%d, want 9/10 (retention kept)", interval, retention)
	}
	if !strings.Contains(dsText, `"is_primary": true`) || !strings.Contains(dsText, `"mysql://n2"`) {
		t.Errorf("replaced datasources = %s", dsText)
	}
	if strings.Count(svcText, `"s1"`) != 1 || !strings.Contains(svcText, `"s2"`) {
		t.Errorf("deduped services = %s", svcText)
	}
}

// TestDeleteProjectGuards — 403 gate and 404 when the row is gone.
func TestDeleteProjectGuards(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name) VALUES ('del-p','删除')`)

	r := httptest.NewRequest("DELETE", "/api/projects/del-p", nil)
	w := httptest.NewRecorder()
	DeleteProject(w, r, "del-p")
	if w.Code != 403 {
		t.Errorf("no role = %d, want 403", w.Code)
	}

	dr := httptest.NewRequest("DELETE", "/api/projects/del-miss", nil)
	dr.Header.Set("X-Role", "super_admin")
	dw := httptest.NewRecorder()
	DeleteProject(dw, dr, "del-miss")
	if dw.Code != 404 {
		t.Errorf("missing delete = %d, want 404", dw.Code)
	}
}

// TestPatchProjectStatusCycle — 404 unknown, 400 invalid status, and the
// full offline → maintenance → online → offline auto-advance cycle.
func TestPatchProjectStatusCycle(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, status) VALUES ('ps-c','循环','offline'),('ps-bad','非法','online')`)

	patch := func(id, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PATCH", "/api/projects/"+id+"/status", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "admin")
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		PatchProjectStatus(w, r, id)
		return w
	}

	if w := patch("ps-none", `{}`); w.Code != 404 {
		t.Errorf("unknown = %d, want 404", w.Code)
	}
	if w := patch("ps-bad", `{"status":"bogus"}`); w.Code != 400 {
		t.Errorf("bogus status = %d, want 400", w.Code)
	}

	want := []string{"maintenance", "online", "offline"}
	for i, next := range want {
		if w := patch("ps-c", `{}`); w.Code != 200 {
			t.Fatalf("cycle %d = %d (body %s)", i, w.Code, w.Body.String())
		}
		var status string
		db.DB.QueryRow("SELECT status FROM projects WHERE id='ps-c'").Scan(&status)
		if status != next {
			t.Errorf("cycle %d status = %q, want %q", i, status, next)
		}
	}
}

// TestTestConnectionGuards — 403, resolveDatasource failures via the ds
// param, and the health_url SSRF check.
func TestTestConnectionGuards(t *testing.T) {
	mustExec := puFixture(t)
	file := filepath.ToSlash(t.TempDir() + "/tc.db")
	os.WriteFile(filepath.FromSlash(file), []byte("x"), 0600)
	mustExec(`INSERT INTO projects (id, name, dsn, health_url, datasources) VALUES
		('tc-miss','缺失','','','[]'::jsonb),
		('tc-ds','多源','','','[{"id":"ds1","name":"主","dsn":"sqlite:///` + file + `","type":"SQLite","is_primary":true}]'::jsonb),
		('tc-h','健康','sqlite:///` + file + `','http://169.254.169.254/x','[]'::jsonb)`)

	tc := func(role, id, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/projects/"+id+"/test-connection"+query, nil)
		if role != "" {
			r.Header.Set("X-Role", role)
			r.Header.Set("X-User-ID", "admin")
		}
		w := httptest.NewRecorder()
		TestConnection(w, r, id)
		return w
	}

	if w := tc("", "tc-miss", ""); w.Code != 403 {
		t.Errorf("no role = %d, want 403", w.Code)
	}
	if w := tc("super_admin", "tc-miss", "?ds=ds1"); w.Code != 400 || !strings.Contains(w.Body.String(), "数据源不存在") {
		t.Errorf("absent datasource = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := tc("super_admin", "tc-ds", "?ds=ds9"); w.Code != 400 || !strings.Contains(w.Body.String(), "数据源不存在") {
		t.Errorf("missing ds = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := tc("super_admin", "tc-ds", "?ds=ds1"); w.Code != 200 {
		t.Errorf("valid ds = %d (body %s), want 200", w.Code, w.Body.String())
	}
	if w := tc("super_admin", "tc-h", ""); w.Code != 400 || !strings.Contains(w.Body.String(), "内网") {
		t.Errorf("health_url SSRF = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// TestProjectStatsScoped — non-admin stats are scoped to project_access;
// empty access and missing users rows return zeros.
func TestProjectStatsScoped(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, status, users_count) VALUES
		('p1','在线','online',5),('p2','离线','offline',3),('p3','维护','maintenance',2)`)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000008','st-u','project_admin','["p1","p2"]'),
		('10000000-0000-0000-0000-000000000009','st-v','viewer','[]')`)

	stats := func(role, uid string) map[string]int {
		r := httptest.NewRequest("GET", "/api/projects/stats", nil)
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", uid)
		w := httptest.NewRecorder()
		ProjectStats(w, r)
		var env struct {
			Data map[string]int `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &env)
		return env.Data
	}

	d := stats("super_admin", "admin")
	if d["total_projects"] != 3 || d["online"] != 1 || d["offline"] != 1 || d["maintenance"] != 1 || d["project_users"] != 10 || d["total_users"] != 12 {
		t.Errorf("super_admin stats = %v", d)
	}
	d = stats("project_admin", "10000000-0000-0000-0000-000000000008")
	if d["total_projects"] != 2 || d["online"] != 1 || d["offline"] != 1 || d["maintenance"] != 0 || d["project_users"] != 8 || d["total_users"] != 10 {
		t.Errorf("scoped stats = %v", d)
	}
	d = stats("viewer", "10000000-0000-0000-0000-000000000009")
	for _, v := range d {
		if v != 0 {
			t.Errorf("empty-access stats = %v, want zeros", d)
		}
	}
	d = stats("viewer", "st-none")
	if d["total_projects"] != 0 {
		t.Errorf("missing-user stats = %v, want zeros", d)
	}
}

// TestVectorSearchGates — 403, bad JSON, missing collection/vector, and the
// non-vector-source rejection before any search runs.
func TestVectorSearchGates(t *testing.T) {
	mustExec := puFixture(t)
	file := filepath.ToSlash(t.TempDir() + "/vs.db")
	os.WriteFile(filepath.FromSlash(file), []byte("x"), 0600)
	mustExec(`INSERT INTO projects (id, name, dsn) VALUES ('vs-proj','向量', 'sqlite:///` + file + `')`)
	mustExec(`INSERT INTO users (id, username, role, project_access) VALUES
		('10000000-0000-0000-0000-000000000010','vs-ok','viewer','["vs-proj"]'),
		('10000000-0000-0000-0000-000000000011','vs-x','viewer','[]')`)

	search := func(role, uid, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/projects/vs-proj/vector-search", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Role", role)
		r.Header.Set("X-User-ID", uid)
		w := httptest.NewRecorder()
		VectorSearch(w, r, "vs-proj")
		return w
	}

	if w := search("viewer", "10000000-0000-0000-0000-000000000011", `{"collection":"c","vector":[1]}`); w.Code != 403 {
		t.Errorf("no access = %d, want 403", w.Code)
	}
	if w := search("viewer", "10000000-0000-0000-0000-000000000010", `{bad`); w.Code != 400 {
		t.Errorf("bad json = %d, want 400", w.Code)
	}
	if w := search("viewer", "10000000-0000-0000-0000-000000000010", `{"collection":"","vector":[]}`); w.Code != 400 {
		t.Errorf("missing collection/vector = %d, want 400", w.Code)
	}
	if w := search("viewer", "10000000-0000-0000-0000-000000000010", `{"collection":"c","vector":[1,2],"top_k":5}`); w.Code != 400 || !strings.Contains(w.Body.String(), "不支持向量检索") {
		t.Errorf("non-vector source = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// TestListTableNamesGates — 403 and the datasource-resolution error path.
func TestListTableNamesGates(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, dsn, datasources) VALUES
		('ltn-p','表','','[{"id":"ds1","name":"主","dsn":"sqlite:///x.db"}]'::jsonb)`)

	r := httptest.NewRequest("GET", "/api/projects/ltn-p/tables/list", nil)
	w := httptest.NewRecorder()
	ListTableNames(w, r, "ltn-p")
	if w.Code != 403 {
		t.Errorf("no role = %d, want 403", w.Code)
	}

	dr := httptest.NewRequest("GET", "/api/projects/ltn-p/tables/list?ds=ds9", nil)
	dr.Header.Set("X-Role", "super_admin")
	dw := httptest.NewRecorder()
	ListTableNames(dw, dr, "ltn-p")
	if dw.Code != 400 || !strings.Contains(dw.Body.String(), "数据源不存在") {
		t.Errorf("bad ds = %d (%s), want 400", dw.Code, dw.Body.String())
	}
}

// TestRowWriteGates — 403 and parameter-validation branches for
// InsertTableRow / DeleteTableRow (happy paths live in TestDataRowCRUD).
func TestRowWriteGates(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, dsn, datasources) VALUES
		('rw-p','行','—','[{"id":"ds1","name":"主","dsn":"sqlite:///x.db"}]'::jsonb)`)

	insert := func(role, query, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/projects/rw-p/data/row"+query, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if role != "" {
			r.Header.Set("X-Role", role)
		}
		w := httptest.NewRecorder()
		InsertTableRow(w, r, "rw-p")
		return w
	}
	deleteRow := func(role, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("DELETE", "/api/projects/rw-p/data/row"+query, nil)
		if role != "" {
			r.Header.Set("X-Role", role)
		}
		w := httptest.NewRecorder()
		DeleteTableRow(w, r, "rw-p")
		return w
	}

	if w := insert("", "?table=t", `{"a":1}`); w.Code != 403 {
		t.Errorf("insert no role = %d, want 403", w.Code)
	}
	if w := insert("super_admin", "", `{"a":1}`); w.Code != 400 || !strings.Contains(w.Body.String(), "缺少table") {
		t.Errorf("insert no table = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := insert("super_admin", "?table=t", `{"a; DROP":1}`); w.Code != 400 || !strings.Contains(w.Body.String(), "非法列名") {
		t.Errorf("insert hostile key = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := insert("super_admin", "?table=t&ds=ds9", `{"a":1}`); w.Code != 400 || !strings.Contains(w.Body.String(), "数据源不存在") {
		t.Errorf("insert bad ds = %d (%s), want 400", w.Code, w.Body.String())
	}

	if w := deleteRow("", "?table=t&pk=id&pkval=1"); w.Code != 403 {
		t.Errorf("delete no role = %d, want 403", w.Code)
	}
	if w := deleteRow("super_admin", "?table=t&pk=id"); w.Code != 400 || !strings.Contains(w.Body.String(), "缺少table/pk/pkval") {
		t.Errorf("delete missing pkval = %d (%s), want 400", w.Code, w.Body.String())
	}
	if w := deleteRow("super_admin", "?table=t&pk=id%22%20OR%201%3D1%20--&pkval=1"); w.Code != 400 || !strings.Contains(w.Body.String(), "非法主键列名") {
		t.Errorf("delete pk injection = %d (%s), want 400", w.Code, w.Body.String())
	}
}

// TestCloneProjectGuards — 403, 404 unknown source, and the empty-dsn clone
// (datasources stays []).
func TestCloneProjectGuards(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, repo, dsn) VALUES ('cl-p','空源','cl-p','—')`)

	r := httptest.NewRequest("POST", "/api/projects/cl-p/clone", nil)
	w := httptest.NewRecorder()
	CloneProject(w, r, "cl-p")
	if w.Code != 403 {
		t.Errorf("no role = %d, want 403", w.Code)
	}

	cr := httptest.NewRequest("POST", "/api/projects/cl-miss/clone", nil)
	cr.Header.Set("X-Role", "super_admin")
	cw := httptest.NewRecorder()
	CloneProject(cw, cr, "cl-miss")
	if cw.Code != 404 {
		t.Errorf("missing source = %d, want 404", cw.Code)
	}

	ok := httptest.NewRequest("POST", "/api/projects/cl-p/clone", nil)
	ok.Header.Set("X-Role", "super_admin")
	ow := httptest.NewRecorder()
	CloneProject(ow, ok, "cl-p")
	if ow.Code != 200 && ow.Code != 201 {
		t.Fatalf("clone = %d (body %s)", ow.Code, ow.Body.String())
	}
	var dsText string
	if err := db.DB.QueryRow("SELECT datasources::text FROM projects WHERE id='cl-p-clone'").Scan(&dsText); err != nil {
		t.Fatalf("clone row missing: %v", err)
	}
	if dsText != "[]" {
		t.Errorf("clone datasources = %s, want []", dsText)
	}
	var status string
	db.DB.QueryRow("SELECT status FROM projects WHERE id='cl-p-clone'").Scan(&status)
	if status != "offline" {
		t.Errorf("clone status = %q, want offline", status)
	}
}

// TestListUsersFilters — search/role filters, avatar thumb preference and
// oversized-avatar drop, and the role-count rollups (NULL role rows count
// as viewer but are dropped from the list by the scan).
func TestListUsersFilters(t *testing.T) {
	mustExec := puFixture(t)
	hugeA := strings.Repeat("a", 70000)
	hugeB := strings.Repeat("b", 70000)
	mustExec(`INSERT INTO users (id, username, name, email, role, status, avatar_url, avatar_thumb) VALUES
		('11111111-aaaa-aaaa-aaaa-aaaaaaaaaaaa','boss','老板','b@t.c','super_admin','active',$1,'thumb-1'),
		('22222222-bbbb-bbbb-bbbb-bbbbbbbbbbbb','pa','管理员','pa@t.c','project_admin','active',$2,''),
		('33333333-cccc-cccc-cccc-cccccccccccc','vi','查看','vi@t.c','viewer','active','small.png',''),
		('44444444-dddd-dddd-dddd-dddddddddddd','listuser','列表','lu@t.c','viewer','active','',''),
		('55555555-eeee-eeee-eeee-eeeeeeeeeeee','nonerole','无角色','nr@t.c',NULL,'active','','')`, hugeA, hugeB)

	list := func(query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/api/users"+query, nil)
		r.Header.Set("X-Role", "super_admin")
		r.Header.Set("X-User-ID", "admin")
		w := httptest.NewRecorder()
		ListUsers(w, r)
		return w
	}
	parse := func(w *httptest.ResponseRecorder) ([]map[string]interface{}, map[string]int) {
		var env struct {
			Data struct {
				Users  []map[string]interface{} `json:"users"`
				Counts map[string]int           `json:"counts"`
			} `json:"data"`
		}
		json.Unmarshal(w.Body.Bytes(), &env)
		return env.Data.Users, env.Data.Counts
	}
	names := func(users []map[string]interface{}) []string {
		out := []string{}
		for _, u := range users {
			out = append(out, u["username"].(string))
		}
		return out
	}

	// search hits name
	w := list("?search=列表")
	users, _ := parse(w)
	if got := names(users); len(got) != 1 || got[0] != "listuser" {
		t.Errorf("search=列表 = %v, want [listuser]", got)
	}
	// role=viewer includes NULL-role rows in the query but the scan drops them
	w = list("?role=viewer")
	users, _ = parse(w)
	got := names(users)
	if len(got) != 2 || got[0] == got[1] || (got[0] != "vi" && got[0] != "listuser") {
		t.Errorf("role=viewer = %v, want [vi listuser] (any order)", got)
	}
	// role=project_admin
	w = list("?role=project_admin")
	users, _ = parse(w)
	if got := names(users); len(got) != 1 || got[0] != "pa" {
		t.Errorf("role=project_admin = %v, want [pa]", got)
	}
	// role=all → no filter
	w = list("?role=all")
	users, counts := parse(w)
	if got := names(users); len(got) != 4 {
		t.Errorf("role=all = %v, want 4 users (NULL-role scan-dropped)", got)
	}
	if counts["all"] != 5 || counts["super_admin"] != 1 || counts["project_admin"] != 1 || counts["viewer"] != 3 {
		t.Errorf("counts = %v, want all=5 super=1 pa=1 viewer=3", counts)
	}
	// avatar: thumb wins over huge url; huge url without thumb is dropped;
	// small url passes through
	for _, u := range users {
		switch u["username"] {
		case "boss":
			if u["avatar_url"] != "thumb-1" {
				t.Errorf("boss avatar = %v, want thumb-1", u["avatar_url"])
			}
		case "pa":
			if u["avatar_url"] != "" {
				t.Errorf("pa avatar = %v, want '' (huge dropped)", u["avatar_url"])
			}
		case "vi":
			if u["avatar_url"] != "small.png" {
				t.Errorf("vi avatar = %v, want small.png", u["avatar_url"])
			}
		}
	}
}

// TestCreateUserModes — auto-generated passwords (env and random paths), salt
// validation, duplicate rejection, and data-URL avatar thumbnails.
func TestCreateUserModes(t *testing.T) {
	puFixture(t)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/users", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", "admin-uid")
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		CreateUser(w, r)
		return w
	}
	mustExecRow := func(q string, out ...interface{}) {
		if err := db.DB.QueryRow(q).Scan(out...); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}

	// auto-generated from INITIAL_PASSWORD env; server picks the salt
	t.Setenv("INITIAL_PASSWORD", "init-pass")
	w := post(`{"username":"auto1","name":"自动","email":"auto1@t.c","role":"viewer"}`)
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("auto create = %d (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Data map[string]interface{} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	if env.Data["password"] != "init-pass" {
		t.Errorf("returned password = %v, want init-pass", env.Data["password"])
	}
	var salt, hash string
	mustExecRow(`SELECT pwd_salt, password_hash FROM users WHERE username='auto1'`, &salt, &hash)
	if !auth.IsSaltHex(salt) {
		t.Errorf("pwd_salt = %q, want 32-hex", salt)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(sha256Hex("init-pass"+salt))) != nil {
		t.Error("hash does not match sha256(pwd+salt) contract")
	}

	// auto-generated random path (INITIAL_PASSWORD empty)
	t.Setenv("INITIAL_PASSWORD", "")
	w = post(`{"username":"auto2","name":"随机","email":"auto2@t.c","role":"viewer"}`)
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("random create = %d (body %s)", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	pwd, _ := env.Data["password"].(string)
	if len(pwd) != 16 {
		t.Errorf("random password = %q, want 16 hex chars", pwd)
	}

	// bad salt shapes → 400 (explicit password keeps the auto path from
	// replacing the salt, matching production's validation order)
	if w := post(`{"username":"s1","name":"盐","email":"s1@t.c","role":"viewer","password":"pw1","salt":"short"}`); w.Code != 400 {
		t.Errorf("short salt = %d, want 400", w.Code)
	}
	if w := post(`{"username":"s2","name":"盐","email":"s2@t.c","role":"viewer","password":"pw2","salt":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`); w.Code != 400 {
		t.Errorf("non-hex salt = %d, want 400", w.Code)
	}

	// duplicate username → 400
	if w := post(`{"username":"dup1","name":"重复","email":"dup1@t.c","role":"viewer","password":"secret123"}`); w.Code != 201 && w.Code != 200 {
		t.Fatalf("first dup create = %d", w.Code)
	}
	if w := post(`{"username":"dup1","name":"重复2","email":"dup2@t.c","role":"viewer","password":"secret123"}`); w.Code != 400 {
		t.Errorf("duplicate = %d, want 400", w.Code)
	}

	// data-URL avatar → thumbnail persisted
	av := pngDataURL(t, 600, 400, false)
	w = post(`{"username":"av1","name":"头像","email":"av1@t.c","role":"viewer","avatar_url":"` + av + `"}`)
	if w.Code != 201 && w.Code != 200 {
		t.Fatalf("avatar create = %d (body %s)", w.Code, w.Body.String())
	}
	var thumb string
	mustExecRow(`SELECT COALESCE(avatar_thumb,'') FROM users WHERE username='av1'`, &thumb)
	if thumb == "" {
		t.Error("avatar_thumb empty — thumbnail not generated")
	}
}

// TestUpdateUserGuardsAndPassword — validation 400s, the R7 password-change
// flow (admin verification, wrong old password, admin missing, both legacy
// plaintext and salted-hex payload shapes), and token_version bumping.
func TestUpdateUserGuardsAndPassword(t *testing.T) {
	mustExec := puFixture(t)
	adminHash, _ := bcrypt.GenerateFromPassword([]byte(sha256Hex("oldpass"+"adminsalt")), bcrypt.DefaultCost)
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, pwd_salt, token_version) VALUES
		('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','adm','管理员','adm@t.c',$1,'super_admin','active','adminsalt',0),
		('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb','tgt','目标','tgt@t.c','oldhash','viewer','active','targetsalt',2)`, string(adminHash))

	put := func(uid, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/api/users/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-User-ID", uid)
		r.Header.Set("X-Role", "super_admin")
		w := httptest.NewRecorder()
		UpdateUser(w, r, "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
		return w
	}
	base := `{"username":"tgt","name":"目标","email":"tgt@t.c","role":"viewer","status":"active","project_access":"[]","avatar_url":""`

	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", `{"username":"","role":"viewer","status":"active"}`); w.Code != 400 {
		t.Errorf("empty username = %d, want 400", w.Code)
	}
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", `{"username":"tgt","role":"hacker","status":"active"}`); w.Code != 400 {
		t.Errorf("bad role = %d, want 400", w.Code)
	}
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", `{"username":"tgt","role":"viewer","status":"weird"}`); w.Code != 400 {
		t.Errorf("bad status = %d, want 400", w.Code)
	}
	// admin account missing → 400
	if w := put("cccccccc-cccc-cccc-cccc-cccccccccccc", `{"username":"tgt","role":"viewer","status":"active","password":"newpass","old_password":"whatever"}`); w.Code != 400 || !strings.Contains(w.Body.String(), "管理员账号不存在") {
		t.Errorf("admin missing = %d (%s), want 400", w.Code, w.Body.String())
	}
	// wrong old password → 400
	bad := `{"username":"tgt","name":"目标","email":"tgt@t.c","role":"viewer","status":"active","password":"newpass","old_password":"wrongpayload"}`
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", bad); w.Code != 400 || !strings.Contains(w.Body.String(), "原密码错误") {
		t.Errorf("wrong old = %d (%s), want 400", w.Code, w.Body.String())
	}
	// success with legacy plaintext new password + avatar thumb
	av := pngDataURL(t, 600, 400, false)
	body := `{"username":"tgt","name":"改名","email":"tgt@t.c","role":"viewer","status":"active","project_access":"[]","avatar_url":"` + av + `","password":"plain-new","old_password":"` + sha256Hex("oldpass"+"adminsalt") + `"}`
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", body); w.Code != 200 {
		t.Fatalf("password update = %d (body %s)", w.Code, w.Body.String())
	}
	var hash, thumb string
	var tv int
	mustExecRow := func(q string, out ...interface{}) {
		if err := db.DB.QueryRow(q).Scan(out...); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
	}
	mustExecRow(`SELECT password_hash, token_version, COALESCE(avatar_thumb,'') FROM users WHERE id='bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb'`, &hash, &tv, &thumb)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(sha256Hex("plain-new"+"targetsalt"))) != nil {
		t.Error("stored hash does not match legacy wrap contract")
	}
	if tv != 3 {
		t.Errorf("token_version = %d, want 3", tv)
	}
	if thumb == "" {
		t.Error("avatar_thumb empty after update")
	}
	// success with salted-hex payload shape (client hashed already)
	hexBody := `{"username":"tgt","name":"改名","email":"tgt@t.c","role":"viewer","status":"active","project_access":"[]","avatar_url":"","password":"` + sha256Hex("hexnew"+"targetsalt") + `","old_password":"` + sha256Hex("oldpass"+"adminsalt") + `"}`
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", hexBody); w.Code != 200 {
		t.Fatalf("hex password update = %d (body %s)", w.Code, w.Body.String())
	}
	mustExecRow(`SELECT password_hash FROM users WHERE id='bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb'`, &hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(sha256Hex("hexnew"+"targetsalt"))) != nil {
		t.Error("stored hash does not match client-hashed payload")
	}
	// no password in payload → plain field update still works (no admin check)
	if w := put("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", base+`,"name":"无密码改名"}`); w.Code != 200 {
		t.Errorf("plain update = %d, want 200", w.Code)
	}
}

// TestUpdateProjectIconEcho ensures a bare logo path echoed back by the
// frontend keeps the stored icon (UpdateProject "/"-prefix branch).
func TestUpdateProjectIconEcho(t *testing.T) {
	mustExec := puFixture(t)
	mustExec(`INSERT INTO projects (id, name, icon_url) VALUES ('up-icon','图标','http://x/icon.png')`)

	r := httptest.NewRequest("PUT", "/api/projects/up-icon", strings.NewReader(`{"name":"图标2","icon_url":"/api/projects/up-icon/logo?v=123"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Role", "super_admin")
	r.Header.Set("X-User-ID", "admin")
	w := httptest.NewRecorder()
	UpdateProject(w, r, "up-icon")
	if w.Code != 200 {
		t.Fatalf("update = %d (body %s)", w.Code, w.Body.String())
	}
	var icon string
	db.DB.QueryRow("SELECT COALESCE(icon_url,'') FROM projects WHERE id='up-icon'").Scan(&icon)
	if icon != "http://x/icon.png" {
		t.Errorf("icon_url = %q, want stored value kept", icon)
	}
}

// TestListProjectsNilTags ensures a project row with NULL tags still
// decodes (tags default '[]' in DDL but NULL arrives via legacy rows).
func TestListProjectsNilTags(t *testing.T) {
	mustExec := puFixture(t)
	// Create the row with NULL columns explicitly — legacy rows may hold NULL.
	mustExec(`INSERT INTO projects (id, name, tags, features, tabs, datasources, services) VALUES ('nil-p','空标签',NULL,NULL,NULL,NULL,NULL)`)

	r := httptest.NewRequest("GET", "/api/projects?status=all", nil)
	r.Header.Set("X-Role", "super_admin")
	r.Header.Set("X-User-ID", "admin")
	w := httptest.NewRecorder()
	ListProjects(w, r)
	if w.Code != 200 {
		t.Fatalf("list = %d (body %s)", w.Code, w.Body.String())
	}
	var env struct {
		Data struct {
			Projects []map[string]interface{} `json:"projects"`
		} `json:"data"`
	}
	json.Unmarshal(w.Body.Bytes(), &env)
	for _, p := range env.Data.Projects {
		if p["id"] == "nil-p" {
			if tags, _ := p["tags"].([]interface{}); len(tags) != 0 {
				t.Errorf("nil tags = %v, want []", p["tags"])
			}
			if feats, _ := p["features"].([]interface{}); len(feats) != 0 {
				t.Errorf("nil features = %v, want []", p["features"])
			}
		}
	}
}
