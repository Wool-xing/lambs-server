package execpath

import "testing"

// TestPathOverride — LAMBS_<BIN>_PATH env overrides the bare binary name so
// open-source deployments can point at non-standard install locations.
func TestPathOverride(t *testing.T) {
	t.Setenv("LAMBS_SQLITE3_PATH", "/opt/tools/sqlite3")
	if got := Path("sqlite3"); got != "/opt/tools/sqlite3" {
		t.Errorf("Path(sqlite3) = %q, want /opt/tools/sqlite3", got)
	}
}

// TestPathDefault — no env set falls back to the bare name.
func TestPathDefault(t *testing.T) {
	for _, name := range []string{"sqlite3", "pg_dump", "journalctl", "ssh"} {
		if got := Path(name); got != name {
			t.Errorf("Path(%s) = %q, want %q", name, got, name)
		}
	}
}

// TestPathKeyShape — each external binary reads its own env var; the key is
// derived from the name (underscores preserved).
func TestPathKeyShape(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{"sqlite3", "LAMBS_SQLITE3_PATH"},
		{"pg_dump", "LAMBS_PG_DUMP_PATH"},
		{"journalctl", "LAMBS_JOURNALCTL_PATH"},
		{"ssh", "LAMBS_SSH_PATH"},
	}
	for _, c := range cases {
		t.Setenv(c.key, "/fake/"+c.name)
		if got := Path(c.name); got != "/fake/"+c.name {
			t.Errorf("Path(%s) = %q, want /fake/%s", c.name, got, c.name)
		}
		t.Setenv(c.key, "")
	}
}
