package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"lambs-server-go/internal/db"
)

// TestListNotificationsUnreadCount — real PostgreSQL verification, gated on
// LAMBS_TEST_PG_DSN (e.g. postgres://postgres:postgres@127.0.0.1:5433/lambs_test?sslmode=disable).
// Skipped by default in CI; run manually against a postgres container.
//
// Covers the authz matrix (global / exact project / 'all' / unknown) for both
// the visible list and the unread_count — the count query had a $1 placeholder
// with no bound args, so unread_count was silently always 0 (QA round 2 HIGH).
func TestListNotificationsUnreadCount(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS notifications; DROP TABLE IF EXISTS users;`)
	mustExec(`CREATE TABLE users (id TEXT PRIMARY KEY, project_access JSONB NOT NULL DEFAULT '[]')`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`INSERT INTO users (id, project_access) VALUES ('u-app', '["app"]'), ('u-all', '["all"]'), ('u-app2', '["app2"]')`)
	mustExec(`INSERT INTO notifications (id, project_id, type, title, is_read) VALUES
		('n1', NULL, 'info', 'global', false),
		('n2', 'app', 'info', 'a1', false),
		('n3', 'app2', 'info', 'a2', false),
		('n4', 'app', 'info', 'a3', true)`)

	cases := []struct {
		name      string
		uid       string
		role      string
		want      int
		wantTotal int
	}{
		{"super_admin sees all", "u-x", "super_admin", 3, 4},
		{"all access sees all", "u-all", "user", 3, 4},
		{"app sees global+app", "u-app", "user", 2, 3},
		{"app2 sees global+app2", "u-app2", "user", 2, 2},
		{"unknown user sees global only", "u-none", "user", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// page_size=1 makes total distinguishable from len(page): total
			// must be the full matching count, not the page size (QA round 2).
			r := httptest.NewRequest("GET", "/api/notifications?page_size=1", nil)
			r.Header.Set("X-User-ID", c.uid)
			r.Header.Set("X-Role", c.role)
			w := httptest.NewRecorder()
			ListNotifications(w, r)
			var body struct {
				Success bool `json:"success"`
				Data    struct {
					UnreadCount int `json:"unread_count"`
					Total       int `json:"total"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("unmarshal: %v (body %s)", err, w.Body.String())
			}
			if body.Data.UnreadCount != c.want {
				t.Errorf("unread_count = %d, want %d (body %s)", body.Data.UnreadCount, c.want, w.Body.String())
			}
			if body.Data.Total != c.wantTotal {
				t.Errorf("total = %d, want %d (body %s)", body.Data.Total, c.wantTotal, w.Body.String())
			}
		})
	}
}

// TestNotificationTouchHandlers — read/delete single rows with the access
// gate; ReadAll covers the three branches (super_admin / hasAll / scoped).
func TestNotificationTouchHandlers(t *testing.T) {
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
	mustExec(`DROP TABLE IF EXISTS notifications; DROP TABLE IF EXISTS users;`)
	mustExec(`CREATE TABLE users (id TEXT PRIMARY KEY, project_access JSONB NOT NULL DEFAULT '[]')`)
	mustExec(`CREATE TABLE notifications (id TEXT PRIMARY KEY, project_id TEXT, type TEXT, title TEXT, content TEXT NOT NULL DEFAULT '', is_read BOOLEAN NOT NULL DEFAULT false, created_at TIMESTAMP NOT NULL DEFAULT now())`)
	mustExec(`INSERT INTO users (id, project_access) VALUES ('u-app', '["app"]'), ('u-all', '["all"]')`)
	mustExec(`INSERT INTO notifications (id, project_id, type, title, is_read) VALUES
		('t1', 'app', 'info', 'own', false),
		('t2', 'other', 'info', 'foreign', false),
		('t3', NULL, 'info', 'global', false)`)

	req := func(method, uid, role, path string) (*httptest.ResponseRecorder, *http.Request) {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("X-User-ID", uid)
		r.Header.Set("X-Role", role)
		w := httptest.NewRecorder()
		return w, r
	}

	// 403: u-app cannot read the foreign notification.
	{
		w, r := req("POST", "u-app", "viewer", "/api/notifications/t2/read")
		ReadNotification(w, r, "t2")
		if w.Code != 403 {
			t.Errorf("foreign read = %d, want 403", w.Code)
		}
	}

	// Own row: read flips is_read.
	{
		w, r := req("POST", "u-app", "viewer", "/api/notifications/t1/read")
		ReadNotification(w, r, "t1")
		if w.Code != 200 {
			t.Fatalf("own read = %d", w.Code)
		}
		var r1 bool
		db.DB.QueryRow("SELECT is_read FROM notifications WHERE id='t1'").Scan(&r1)
		if !r1 {
			t.Error("is_read not flipped for own row")
		}
	}

	// Delete own row.
	{
		w, r := req("DELETE", "u-app", "viewer", "/api/notifications/t1")
		DeleteNotification(w, r, "t1")
		if w.Code != 200 {
			t.Fatalf("own delete = %d", w.Code)
		}
		var n int
		db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE id='t1'").Scan(&n)
		if n != 0 {
			t.Error("row survived delete")
		}
	}

	// Scoped user reads all: only own + global rows flip, foreign stays.
	{
		w, r := req("POST", "u-app", "viewer", "/api/notifications/read-all")
		ReadAllNotifications(w, r)
		if w.Code != 200 {
			t.Fatalf("read-all = %d", w.Code)
		}
		var foreignRead bool
		db.DB.QueryRow("SELECT is_read FROM notifications WHERE id='t2'").Scan(&foreignRead)
		if foreignRead {
			t.Error("scoped read-all flipped a foreign row")
		}
		var globalRead bool
		db.DB.QueryRow("SELECT is_read FROM notifications WHERE id='t3'").Scan(&globalRead)
		if !globalRead {
			t.Error("scoped read-all did not flip the global row")
		}
	}

	// hasAll user flips everything, including foreign rows.
	{
		w, r := req("POST", "u-all", "viewer", "/api/notifications/read-all")
		ReadAllNotifications(w, r)
		if w.Code != 200 {
			t.Fatalf("read-all-all = %d", w.Code)
		}
		var foreignRead bool
		db.DB.QueryRow("SELECT is_read FROM notifications WHERE id='t2'").Scan(&foreignRead)
		if !foreignRead {
			t.Error("hasAll read-all did not flip the foreign row")
		}
	}

	// super_admin branch: flips everything too.
	{
		w, r := req("POST", "", "super_admin", "/api/notifications/read-all")
		ReadAllNotifications(w, r)
		if w.Code != 200 {
			t.Fatalf("read-all-sa = %d", w.Code)
		}
	}
}
