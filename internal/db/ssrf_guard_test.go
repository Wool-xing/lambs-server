package db

import "testing"

func TestCheckDSNHost(t *testing.T) {
	cases := []struct {
		name    string
		dsn     string
		wantErr bool
	}{
		{"empty ok", "", false},
		{"sqlite ok", "sqlite:///data/app.db", false},
		{"tailscale ok", "postgres://lambs:pw@100.92.91.11:5432/lambs?sslmode=disable", false},
		{"tailscale mongo ok", "mongodb://100.104.214.17:27017/lambs", false},
		{"loopback ok (same-host datasources)", "postgres://u:p@127.0.0.1:5432/db", false},
		{"localhost ok", "http://localhost:8000/api", false},
		{"rfc1918 blocked", "mysql://u:p@192.168.1.5:3306/db", true},
		{"ten-blocked", "redis://10.0.0.5:6379", true},
		{"metadata blocked", "http://169.254.169.254/latest/meta-data", true},
		{"mysql tcp blocked", "u:p@tcp(172.16.0.2:3306)/db", true},
		{"public ok", "http://example.com/api", false},
		{"qdrant loopback ok", "qdrant://127.0.0.1:6333", false},
		{"qdrant tailscale ok", "qdrant://100.92.91.11:6333", false},
		{"qdrant private blocked", "qdrant://169.254.169.254:6333", true},
		{"uppercase scheme bypass blocked", "Mongo://169.254.169.254:27017/db", true},
		{"uppercase qdrant bypass blocked", "QDRANT://169.254.169.254:6333", true},
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
		{"tailscale allowed", "postgres://lambs:pw@100.92.91.11:5432/lambs", false},
		{"rfc1918 blocked", "mysql://u:p@192.168.1.5:3306/db", true},
		{"metadata blocked", "http://169.254.169.254/latest/meta-data", true},
		{"qdrant loopback allowed", "qdrant://127.0.0.1:6333", false},
		{"qdrant private blocked", "qdrant://169.254.169.254:6333", true},
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
