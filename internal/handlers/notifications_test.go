package handlers

import (
	"encoding/json"
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
		name string
		uid  string
		role string
		want int
	}{
		{"super_admin sees all", "u-x", "super_admin", 3},
		{"all access sees all", "u-all", "user", 3},
		{"app sees global+app", "u-app", "user", 2},
		{"app2 sees global+app2", "u-app2", "user", 2},
		{"unknown user sees global only", "u-none", "user", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/notifications", nil)
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
		})
	}
}
