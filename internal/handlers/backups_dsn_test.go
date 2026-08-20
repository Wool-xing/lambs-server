package handlers

import "testing"

// TestParsePGDSN — table-driven DSN parsing for the three postgres URL
// schemes. The postgres:// form previously parsed the password as "//xxx"
// and broke pg_dump auth (QA round 5 CI caught it).
func TestParsePGDSN(t *testing.T) {
	cases := []struct {
		dsn      string
		user     string
		password string
		host     string
		port     string
		dbname   string
	}{
		{
			"postgres://lambs_admin:pw@10.1.2.3:5433/mydb?sslmode=disable",
			"lambs_admin", "pw", "10.1.2.3", "5433", "mydb",
		},
		{
			"postgresql://u:pw@db.internal/app",
			"u", "pw", "db.internal", "5433", "app",
		},
		{
			"postgresql+asyncpg://u:pw@127.0.0.1:5432/lambs",
			"u", "pw", "127.0.0.1", "5432", "lambs",
		},
		{
			// No auth section: defaults kick in.
			"postgres://127.0.0.1/db",
			"lambs_admin", "", "127.0.0.1", "5433", "db",
		},
	}
	for _, c := range cases {
		user, password, host, port, dbname := parsePGDSN(c.dsn)
		if user != c.user || password != c.password || host != c.host || port != c.port || dbname != c.dbname {
			t.Errorf("parsePGDSN(%q) = (%q,%q,%q,%q,%q), want (%q,%q,%q,%q,%q)",
				c.dsn, user, password, host, port, dbname, c.user, c.password, c.host, c.port, c.dbname)
		}
	}
}
