package handlers

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"lambs-server-go/internal/db"
)

// TestMaskDSN — URL-form DSNs get *** for the password, local-file forms
// pass through untouched.
func TestMaskDSN(t *testing.T) {
	cases := map[string]string{
		// NOTE: dummy credentials only — never put real prod secrets in tests.
		"mysql://lambs:Dummy_Pw_NotReal@100.104.214.17:3306/db": "mysql://lambs:***@100.104.214.17:3306/db",
		"redis://:Dummy_Pw_NotReal@127.0.0.1:6379/0":            "redis://:***@127.0.0.1:6379/0",
		// password containing ":" — must not leak the prefix
		"mysql://lambs:ab:cd@h/d": "mysql://lambs:***@h/d",
		// password containing "@"
		"mysql://lambs:pa@ss@h/d": "mysql://lambs:***@h/d",
		// passwordless userinfo passes through unchanged
		"mysql://lambs@h/d":                        "mysql://lambs@h/d",
		"mongodb://100.104.214.17:27017/db":        "mongodb://100.104.214.17:27017/db",
		"sqlite:////home/ubuntu/apps/x.db?mode=ro": "sqlite:////home/ubuntu/apps/x.db?mode=ro",
		"—": "—",
	}
	for in, want := range cases {
		if got := maskDSN(in); got != want {
			t.Errorf("maskDSN(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDatasourcesMasked — the settings datasources endpoint must never
// return a DSN whose password is recoverable from the response body
// (browser DOM, screenshots, extensions all read this payload).
func TestDatasourcesMasked(t *testing.T) {
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
	mustExec(projectsDDL)
	defer mustExec(`DROP TABLE IF EXISTS projects CASCADE`)
	const secret = "Sup3rS3cretPw"
	mustExec(`INSERT INTO projects (id, name, db_type, dsn) VALUES ('mask-proj','脱敏项目','MySQL','mysql://lambs:` + secret + `@127.0.0.1:3306/lambs')`)

	r := httptest.NewRequest("GET", "/api/settings/datasources", nil)
	r.Header.Set("X-Role", "super_admin")
	r.Header.Set("X-User-ID", "admin")
	w := httptest.NewRecorder()
	Datasources(w, r)
	if w.Code != 200 {
		t.Fatalf("code = %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("raw password leaked in datasources payload")
	}
	if !strings.Contains(body, "***") {
		t.Fatalf("masked dsn missing; body = %s", body)
	}
	// still usable for display: scheme + host preserved
	if !strings.Contains(body, "mysql://lambs:***@127.0.0.1:3306") {
		t.Fatalf("mask shape wrong; body = %s", body)
	}
}
