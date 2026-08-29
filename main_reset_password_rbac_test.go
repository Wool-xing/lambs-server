package main

import (
	"net/http/httptest"
	"testing"

	"lambs-server-go/internal/db"
)

// TestResetPasswordRBAC — 2026-08-29 extension: super_admin resets anyone;
// project_admin only users sharing a project; viewers never; forged role
// headers are overwritten by RequireAuth.
func TestResetPasswordRBAC(t *testing.T) {
	mustExec := rbacFixture(t)
	// A member sharing proj-a with rbac_pa.
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role, status, token_version, project_access) VALUES
		('55555555-5555-5555-5555-555555555555','rbac_member','成员','member@t.c','x','viewer','active',1,'["proj-a"]'),
		('66666666-6666-6666-6666-666666666666','rbac_member_b','B成员','memberb@t.c','x','viewer','active',1,'["proj-b"]')`)

	ts := httptest.NewServer(newMux())
	defer ts.Close()

	body := `{"new_password":"reset123456"}`
	cases := []struct {
		name, token string
		target      string
		headers     map[string]string
		want        int
	}{
		{"超管重置任意", signRBACToken(t, "11111111-1111-1111-1111-111111111111", "super_admin", 1), "55555555-5555-5555-5555-555555555555", nil, 200},
		{"项目管理员重置共享成员", signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1), "55555555-5555-5555-5555-555555555555", nil, 200},
		{"项目管理员重置非共享用户", signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1), "33333333-3333-3333-3333-333333333333", nil, 403},
		{"项目管理员重置超管", signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1), "11111111-1111-1111-1111-111111111111", nil, 403},
		{"viewer 重置", signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1), "55555555-5555-5555-5555-555555555555", nil, 403},
		{"viewer 重置共享项目用户也被拒", signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1), "66666666-6666-6666-6666-666666666666", nil, 403},
		{"无令牌", "", "55555555-5555-5555-5555-555555555555", nil, 401},
		{"伪造 X-Role 超管头", signRBACToken(t, "33333333-3333-3333-3333-333333333333", "viewer", 1), "55555555-5555-5555-5555-555555555555", map[string]string{"X-Role": "super_admin"}, 403},
	}
	for _, c := range cases {
		code, resp := rbacDo(t, ts, "POST", "/api/users/"+c.target+"/reset-password", c.token, c.headers, body)
		if code != c.want {
			t.Errorf("%s: code = %d, want %d (resp %s)", c.name, code, c.want, resp)
		}
	}
}

// TestClearNotificationsScope — clear deletes only what the caller can see
// (QA feedback 2026-08-29: notification center clear button).
func TestClearNotificationsScope(t *testing.T) {
	mustExec := rbacFixture(t)
	mustExec(`DELETE FROM notifications`)
	mustExec(`INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES
		('n-global','','info','全局','全局通知',false,NOW()),
		('n-a','proj-a','info','A项目','A项目通知',false,NOW()),
		('n-b','proj-b','info','B项目','B项目通知',false,NOW())`)

	ts := httptest.NewServer(newMux())
	defer ts.Close()

	// project_admin (proj-a): clears global + proj-a only, proj-b survives.
	code, _ := rbacDo(t, ts, "DELETE", "/api/notifications", signRBACToken(t, "22222222-2222-2222-2222-222222222222", "project_admin", 1), nil, "")
	if code != 200 {
		t.Fatalf("clear: code = %d, want 200", code)
	}
	var remaining int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE id='n-b'").Scan(&remaining)
	if remaining != 1 {
		t.Errorf("proj-b notification = %d, want 1 (survives out-of-scope clear)", remaining)
	}
	var gone int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications WHERE id IN ('n-global','n-a')").Scan(&gone)
	if gone != 0 {
		t.Errorf("in-scope notifications = %d, want 0", gone)
	}

	// super_admin clears everything.
	if code, _ := rbacDo(t, ts, "DELETE", "/api/notifications", signRBACToken(t, "11111111-1111-1111-1111-111111111111", "super_admin", 1), nil, ""); code != 200 {
		t.Fatalf("sa clear: code = %d, want 200", code)
	}
	var total int
	db.DB.QueryRow("SELECT COUNT(*) FROM notifications").Scan(&total)
	if total != 0 {
		t.Errorf("notifications after sa clear = %d, want 0", total)
	}
}
