package db

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestTestDSNUnconfigured — empty or dash dsn: honest 未配置数据源 envelope.
func TestTestDSNUnconfigured(t *testing.T) {
	for _, dsn := range []string{"", "—"} {
		got := TestDSN(dsn)
		if got["reachable"] != false || got["error"] != "未配置数据源" {
			t.Errorf("TestDSN(%q) = %v", dsn, got)
		}
	}
}

// TestTestDSNBlockedHost — the SSRF guard refuses internal targets before
// any dial happens (169.254.169.254 = cloud metadata).
func TestTestDSNBlockedHost(t *testing.T) {
	got := TestDSN("http://169.254.169.254/latest/meta-data")
	if got["reachable"] != false {
		t.Errorf("blocked host reachable = %v, want false", got)
	}
}

// TestTestDSNHTTP — the REST branch: 200 reachable, 500 unreachable,
// unreachable endpoint maps to a credential-free 连接失败.
func TestTestDSNHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	if got := TestDSN(ts.URL); got["reachable"] != false || got["db_type"] != "rest_api" {
		t.Errorf("500 dsn = %v, want unreachable rest_api", got)
	}

	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	if got := TestDSN(ok.URL); got["reachable"] != true || got["db_type"] != "rest_api" {
		t.Errorf("200 dsn = %v, want reachable rest_api", got)
	}

	if got := TestDSN("http://127.0.0.1:1/none"); got["reachable"] != false || got["error"] != "连接失败" {
		t.Errorf("unreachable http = %v", got)
	}
}

// TestTestDSNTCPDialBranches — mysql/mssql/mongodb health checks are pure
// TCP dials: a live local listener is reachable, a closed port is not.
func TestTestDSNTCPDialBranches(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	host := ln.Addr().String()

	cases := []struct{ name, dsn string }{
		{"mysql", "mysql://root:pw@" + host + "/db"},
		{"mssql", "mssql://sa:pw@" + host + "?database=x"},
		{"mongodb", "mongodb://" + host + "/db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := TestDSN(c.dsn); got["reachable"] != true {
				t.Errorf("live listener %s = %v, want reachable", c.name, got)
			}
		})
	}
	if got := TestDSN("mysql://root:pw@127.0.0.1:1/db"); got["reachable"] != false {
		t.Errorf("closed port = %v, want unreachable", got)
	}
}

// TestTestHealthPriority — health_url wins; without it the dsn is checked.
func TestTestHealthPriority(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer ok.Close()
	got := TestHealth("", ok.URL)
	if got["reachable"] != true {
		t.Errorf("health_url check = %v, want reachable", got)
	}
	got = TestHealth("—", "")
	if got["reachable"] != false || got["error"] != "未配置数据源" {
		t.Errorf("empty health fallback = %v", got)
	}
}

// TestInitAndTestDSNPostgres — env-gated: real postgres connect + ping
// through Init, and the postgres branch of testDSNInternal.
func TestInitAndTestDSNPostgres(t *testing.T) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := Init(dsn); err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(func() {
		if DB != nil {
			DB.Close()
		}
		DB = nil
	})
	if got := TestDSN(dsn); got["reachable"] != true || got["db_type"] != "postgresql" {
		t.Errorf("real pg = %v, want reachable postgresql", got)
	}
	if got := TestDSN("postgres://u:p@127.0.0.1:1/none?sslmode=disable"); got["reachable"] != false {
		t.Errorf("unreachable pg = %v, want false", got)
	}
	// asyncpg prefix normalization: Init accepts the asyncpg scheme.
	if err := Init("postgresql+asyncpg://" + dsn[len("postgres://"):]); err != nil {
		t.Errorf("Init asyncpg prefix: %v", err)
	}
}
