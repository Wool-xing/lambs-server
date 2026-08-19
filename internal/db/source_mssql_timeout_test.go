package db

import (
	"net/url"
	"testing"
	"time"
)

// TestMSSQLGoDSNDefaultTimeout — a hung SQL Server host must not block a
// handler indefinitely: goDSN injects a connection timeout when the DSN
// omits it (QA round 2 test idea; audit MEDIUM "connection timeout").
func TestMSSQLGoDSNDefaultTimeout(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantHas bool
		wantVal string
	}{
		{"default injected", "mssql://sa:pw@127.0.0.1:1433/master", true, "10"},
		{"explicit preserved", "mssql://sa:pw@127.0.0.1:1433/master?dial+timeout=3", true, "3"},
		{"zero coerced to default", "mssql://sa:pw@127.0.0.1:1433/master?dial+timeout=0", true, "10"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &MSSQLSource{dsn: c.dsn}
			got, err := src.goDSN()
			if err != nil {
				t.Fatalf("goDSN: %v", err)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("parse result: %v", err)
			}
			v := u.Query().Get("dial timeout")
			if c.wantHas && v != c.wantVal {
				t.Errorf("dial timeout = %q, want %q (dsn %s)", v, c.wantVal, got)
			}
			if !c.wantHas && v != "" {
				t.Errorf("dial timeout = %q, want unset (dsn %s)", v, got)
			}
		})
	}
}

// TestMSSQLOpenTimeout — dialing an unreachable host (TEST-NET) must fail
// within the configured window, not hang on OS TCP retries. Uses an explicit
// 2s timeout so the CI run stays fast; the default-10s injection is covered
// by TestMSSQLGoDSNDefaultTimeout.
func TestMSSQLOpenTimeout(t *testing.T) {
	src := &MSSQLSource{dsn: "mssql://sa:pw@10.255.255.1:1433/master?dial+timeout=2"}
	start := time.Now()
	tdb, err := src.open()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer tdb.Close()
	_ = tdb.Ping() // expected to fail — what matters is the bound below
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("dial to unreachable host took %v, want < 8s (connection timeout not applied?)", elapsed)
	}
}
