package db

import (
	"net"
	"testing"
)

func TestCheckDSNHost(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"sqlite ok", "sqlite:///data/app.db", false},
		{"tailscale ok", "postgres://lambs:pw@100.64.0.9:5432/lambs?sslmode=disable", false},
		{"tailscale mongo ok", "mongodb://100.64.0.10:27017/lambs", false},
		{"loopback ok (same-host datasources)", "postgres://u:p@127.0.0.1:5432/db", false},
		{"localhost ok", "http://localhost:8000/api", false},
		{"rfc1918 blocked", "mysql://u:p@192.168.1.5:3306/db", true},
		{"ten-blocked", "redis://10.0.0.5:6379", true},
		{"metadata blocked", "http://169.254.169.254/latest/meta-data", true},
		{"mysql tcp blocked", "u:p@tcp(172.16.0.2:3306)/db", true},
		// IP literal: net.LookupIP short-circuits without a resolver, so the
		// guard suite no longer needs online DNS (R3-P3: offline CI broke here).
		{"public ok", "http://93.184.216.34/api", false},
		{"qdrant loopback ok", "qdrant://127.0.0.1:6333", false},
		{"qdrant tailscale ok", "qdrant://100.64.0.9:6333", false},
		{"qdrant private blocked", "qdrant://169.254.169.254:6333", true},
		{"uppercase scheme bypass blocked", "Mongo://169.254.169.254:27017/db", true},
		{"uppercase qdrant bypass blocked", "QDRANT://169.254.169.254:6333", true},
		{"postgres keyword private blocked", "host=10.0.0.5 port=5432 user=u dbname=d", true},
		{"postgres keyword quoted private blocked", "host='192.168.1.9' port=5432 user=u", true},
		{"postgres keyword tailscale ok", "host=100.64.0.9 port=5432 user=u", false},
		{"postgres keyword loopback ok", "host=127.0.0.1 port=5432 user=u", false},
		{"postgres keyword password-with-scheme blocked", "host=192.168.1.5 port=5432 user=u password='x://y'", true},
		{"postgres keyword host-list public ok", "host=100.64.0.9,127.0.0.1 port=5432 user=u", false},
		{"postgres keyword host-list private blocked", "host=100.64.0.9,192.168.1.5 port=5432 user=u", true},
		{"postgres keyword dup host override blocked", "host=8.8.8.8 host=192.168.1.5 port=5432 user=u", true},
		{"postgres keyword hostaddr blocked", "host=8.8.8.8 hostaddr=169.254.169.254 port=5432 user=u", true},
		{"postgres url query hostaddr blocked", "postgres://8.8.8.8:5432/db?hostaddr=169.254.169.254", true},
		{"postgres keyword spaced-equals blocked", "host = 10.0.0.5 port=5432 user=u", true},
		{"postgres keyword dual loopback ok", "host=127.0.0.1 hostaddr=127.0.0.1 port=5432 user=u", false},
		{"mssql loopback ok", "mssql://sa:p@127.0.0.1:1433/db", false},
		{"mssql tailscale ok", "mssql://sa:p@100.64.0.9:1433/db", false},
		{"mssql private blocked", "mssql://sa:p@192.168.1.5:1433/db", true},
		{"mssql metadata blocked", "mssql://sa:p@169.254.169.254:1433/db", true},
		{"uppercase mssql bypass blocked", "MSSQL://sa:p@169.254.169.254:1433/db", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckDSNHost(c.dsn)
			if (err != nil) != c.wantErr {
				t.Errorf("dsn=%q err=%v wantErr=%v", c.dsn, err, c.wantErr)
			}
		})
	}
}

// TestPinHostToIP: URL-form DSNs (http/qdrant/redis) get their hostname pinned
// to a validated IP so the later dial cannot re-resolve (R3-3 DNS rebinding).
// postgres/mysql/mongo/https are driver/TLS-bound — pinning is skipped there.
func TestPinHostToIP(t *testing.T) {
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{"http localhost pinned", "http://localhost:8000/api", "http://127.0.0.1:8000/api"},
		{"qdrant localhost pinned", "qdrant://localhost:6333", "qdrant://127.0.0.1:6333"},
		{"already IP unchanged", "http://127.0.0.1:8901", "http://127.0.0.1:8901"},
		{"redis IP unchanged", "redis://:pw@127.0.0.1:6379/0", "redis://:pw@127.0.0.1:6379/0"},
		{"postgres untouched", "postgres://u:p@db.example.com:5432/db", "postgres://u:p@db.example.com:5432/db"},
		{"https untouched", "https://example.com/api", "https://example.com/api"},
		{"sqlite untouched", "sqlite:///data/app.db", "sqlite:///data/app.db"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := pinHostToIP(c.dsn)
			if err != nil {
				t.Fatalf("dsn=%q err=%v", c.dsn, err)
			}
			if got != c.want {
				t.Errorf("dsn=%q got=%q want=%q", c.dsn, got, c.want)
			}
		})
	}
}

// TestPickIPv4: IPv6-only resolution must NOT be pinned (bare IPv6 breaks URLs).
func TestPickIPv4(t *testing.T) {
	if got := pickIPv4([]net.IP{net.ParseIP("2001:db8::1")}); got != nil {
		t.Errorf("ipv6-only got %v want nil", got)
	}
	if got := pickIPv4([]net.IP{net.ParseIP("2001:db8::1"), net.ParseIP("192.0.2.1")}); got == nil || got.String() != "192.0.2.1" {
		t.Errorf("mixed got %v want 192.0.2.1", got)
	}
}

// TestNewDataSourcePinsHost: the pin must happen inside NewDataSource.
func TestNewDataSourcePinsHost(t *testing.T) {
	src, err := NewDataSource("http://localhost:8000")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	rest, ok := src.(*RESTSource)
	if !ok {
		t.Fatalf("unexpected type %T", src)
	}
	if rest.dsn != "http://127.0.0.1:8000" {
		t.Errorf("dsn=%q want pinned 127.0.0.1", rest.dsn)
	}
}

// TestNewDataSourceSSRFGuard: the guard must sit INSIDE NewDataSource so every
// dial path is covered (R3-2 — previously only TestDSN and 3 handlers guarded).
func TestNewDataSourceSSRFGuard(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"sqlite allowed", "sqlite:///qa.db", false},
		{"loopback allowed", "postgres://u:p@127.0.0.1:5432/db", false},
		{"tailscale allowed", "postgres://lambs:pw@100.64.0.9:5432/lambs", false},
		{"rfc1918 blocked", "mysql://u:p@192.168.1.5:3306/db", true},
		{"metadata blocked", "http://169.254.169.254/latest/meta-data", true},
		{"qdrant loopback allowed", "qdrant://127.0.0.1:6333", false},
		{"qdrant private blocked", "qdrant://169.254.169.254:6333", true},
		{"postgres keyword private blocked", "host=10.0.0.5 port=5432 user=u dbname=d", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewDataSource(c.dsn)
			if (err != nil) != c.wantErr {
				t.Errorf("dsn=%q err=%v wantErr=%v", c.dsn, err, c.wantErr)
			}
		})
	}
}
