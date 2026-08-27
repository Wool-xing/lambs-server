package db

import (
	"strings"
	"testing"
)

// TestMySQLGoDSN pins the mysql:// → driver DSN conversion (the mysql
// adapter was the only source adapter whose goDSN had zero test coverage).
// mysql.Config.FormatDSN escapes special chars in credentials — raw
// fmt.Sprintf breaks on passwords containing @/: etc.
func TestMySQLGoDSN(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{
			name: "basic",
			dsn:  "mysql://user:pass@localhost:3306/dbname",
			want: "user:pass@tcp(localhost:3306)/dbname?timeout=5s&charset=utf8mb4",
		},
		{
			name: "passwordless user",
			dsn:  "mysql://user@localhost:3306/dbname",
			want: "user@tcp(localhost:3306)/dbname?timeout=5s&charset=utf8mb4",
		},
		{
			name: "percent-encoded special chars in password",
			dsn:  "mysql://user:p%40ss%3Aw@10.0.0.1:3307/my_db",
			want: "user:p@ss:w@tcp(10.0.0.1:3307)/my_db?timeout=5s&charset=utf8mb4",
		},
		{
			name: "empty database path",
			dsn:  "mysql://u:pw@h:3306/",
			want: "u:pw@tcp(h:3306)/?timeout=5s&charset=utf8mb4",
		},
		{
			name: "extra query params are dropped (only charset is applied)",
			dsn:  "mysql://u:pw@h:3306/db?tls=skip-verify",
			want: "u:pw@tcp(h:3306)/db?timeout=5s&charset=utf8mb4",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &MySQLSource{dsn: c.dsn}
			got, err := s.goDSN()
			if err != nil {
				t.Fatalf("goDSN(%q) error: %v", c.dsn, err)
			}
			if got != c.want {
				t.Errorf("goDSN(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}

// TestMySQLGoDSNInvalid — a malformed DSN must surface as an error, not a
// half-built connection string.
func TestMySQLGoDSNInvalid(t *testing.T) {
	s := &MySQLSource{dsn: "not a url::dsn"}
	if _, err := s.goDSN(); err == nil {
		t.Error("goDSN(malformed) = nil error, want error")
	}
}

// TestMySQLGoDSNUsesTCP — the driver must default to tcp (never the unix
// socket fallback) so host:port addresses work on every platform.
func TestMySQLGoDSNUsesTCP(t *testing.T) {
	s := &MySQLSource{dsn: "mysql://u:p@h:3306/db"}
	got, err := s.goDSN()
	if err != nil {
		t.Fatalf("goDSN: %v", err)
	}
	if !strings.Contains(got, "tcp(") {
		t.Errorf("goDSN = %q, want tcp network", got)
	}
}
