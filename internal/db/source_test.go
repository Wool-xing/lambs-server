package db

import "testing"

func TestParseScheme(t *testing.T) {
	cases := map[string]string{
		"sqlite:////path/to.db":               "sqlite",
		"postgresql+asyncpg://u@h/db":         "postgresql+asyncpg",
		"mysql://u:p@h:3306/db":               "mysql",
		"redis://:pw@h:6380/0":                "redis",
		"mongodb://h:27017/db":                "mongodb",
		"http://h/api":                        "http",
		"https://h/api":                        "https",
		"no-scheme-here":                      "",
	}
	for dsn, want := range cases {
		if got := parseScheme(dsn); got != want {
			t.Errorf("parseScheme(%q) = %q, want %q", dsn, got, want)
		}
	}
}

func TestNewDataSourceRouting(t *testing.T) {
	// Placeholder hosts are loopback literals: NewDataSource now runs the SSRF
	// guard, so unresolvable placeholder hosts would be (correctly) rejected.
	cases := map[string]string{
		"sqlite:////tmp/x.db":                         "sqlite",
		"postgresql://u@127.0.0.1/db":                 "postgresql",
		"postgresql+asyncpg://u@127.0.0.1/db":         "postgresql",
		"mysql://u:p@127.0.0.1/db":                    "mysql",
		"mongodb://127.0.0.1/db":                      "mongodb",
		"mongo://127.0.0.1/db":                        "mongodb",
		"redis://:p@127.0.0.1/0":                      "redis",
		"http://127.0.0.1/api":                        "http",
		"https://127.0.0.1/api": "http", // both are RESTSource
		"qdrant://127.0.0.1:6333":                     "qdrant",
	}
	for dsn, wantType := range cases {
		src, err := NewDataSource(dsn)
		if err != nil {
			t.Errorf("NewDataSource(%q) error: %v", dsn, err)
			continue
		}
		got := ""
		switch src.(type) {
		case *SQLiteSource:
			got = "sqlite"
		case *PostgresSource:
			got = "postgresql"
		case *MySQLSource:
			got = "mysql"
		case *MongoSource:
			got = "mongodb"
		case *RedisSource:
			got = "redis"
		case *RESTSource:
			got = "http"
		case *VectorSource:
			got = "qdrant"
		}
		if got != wantType {
			t.Errorf("NewDataSource(%q) routed to %q, want %q", dsn, got, wantType)
		}
	}
	// unsupported
	if _, err := NewDataSource("oracle://h/db"); err == nil {
		t.Error("unsupported scheme should error")
	}
	if _, err := NewDataSource(""); err == nil {
		t.Error("empty dsn should error")
	}
}

func TestValidateKey(t *testing.T) {
	valid := []string{"a", "user:1:posts", "cache.page-1", "a.b.c", "中文key"}
	for _, k := range valid {
		if err := validateKey(k); err != nil {
			t.Errorf("validateKey(%q) rejected: %v", k, err)
		}
	}
	invalid := []string{"", "a b", `a"b`, "a'b", "a\\b", "a\nb"}
	for _, k := range invalid {
		if err := validateKey(k); err == nil {
			t.Errorf("validateKey(%q) accepted", k)
		}
	}
}

func TestValidateTable(t *testing.T) {
	if err := validateTable("users"); err != nil {
		t.Errorf("users rejected: %v", err)
	}
	bad := []string{"users; DROP TABLE x", "1users", "us ers", "users--"}
	for _, tb := range bad {
		if err := validateTable(tb); err == nil {
			t.Errorf("validateTable(%q) accepted", tb)
		}
	}
}
