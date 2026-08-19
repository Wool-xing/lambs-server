package runtime

import "testing"

func TestParseAllowedSources(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]bool
	}{
		{"", map[string]bool{}},
		{"  , , ", map[string]bool{}},
		{"1.2.3.4", map[string]bool{"1.2.3.4": true}},
		{"1.2.3.4, 5.6.7.8,1.2.3.4", map[string]bool{"1.2.3.4": true, "5.6.7.8": true}},
	}
	for _, c := range cases {
		got := parseAllowedSources(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseAllowedSources(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for k := range c.want {
			if !got[k] {
				t.Errorf("parseAllowedSources(%q) missing %q: %v", c.in, k, got)
			}
		}
	}
}

// TestSourceAllowedFailClosed — with no allowlist configured, only loopback
// may connect; any public source must be rejected (QA round 1+2 HIGH: empty
// allowlist previously meant "allow everyone").
// TestSelfLoopBackend — the guard must catch every loopback spelling that
// resolves to the listener port, not just the literal 127.0.0.1:port string
// (QA round 2 calibration: localhost / ::1 variants bypassed the old check).
func TestSelfLoopBackend(t *testing.T) {
	cases := []struct {
		backend string
		port    string
		want    bool
	}{
		{"127.0.0.1:8080", "8080", true},
		{"localhost:8080", "8080", true},
		{"[::1]:8080", "8080", true},
		{"1.2.3.4:8080", "8080", false},
		{"127.0.0.1:8081", "8080", false},
		{"no-port-here", "8080", false},
	}
	for _, c := range cases {
		if got := selfLoopBackend(c.backend, c.port); got != c.want {
			t.Errorf("selfLoopBackend(%q, %q) = %v, want %v", c.backend, c.port, got, c.want)
		}
	}
}

func TestSourceAllowedFailClosed(t *testing.T) {
	cases := []struct {
		name   string
		cfg    map[string]bool
		remote string
		want   bool
	}{
		{"no cfg: ipv4 loopback allowed", nil, "127.0.0.1:8080", true},
		{"no cfg: ipv6 loopback allowed", nil, "[::1]:8080", true},
		{"no cfg: localhost allowed", nil, "localhost:8080", true},
		{"no cfg: public ip rejected", nil, "1.2.3.4:8080", false},
		{"no cfg: lan ip rejected", nil, "192.168.1.5:8080", false},
		{"allowlisted ip allowed", map[string]bool{"1.2.3.4": true}, "1.2.3.4:8080", true},
		{"non-allowlisted ip rejected", map[string]bool{"1.2.3.4": true}, "5.6.7.8:8080", false},
		{"allowlist keeps loopback", map[string]bool{"1.2.3.4": true}, "127.0.0.1:8080", true},
		{"malformed remote rejected", nil, "no-port", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			old := allowedSources
			allowedSources = c.cfg
			defer func() { allowedSources = old }()
			if got := sourceAllowed(c.remote); got != c.want {
				t.Errorf("sourceAllowed(%q) = %v, want %v", c.remote, got, c.want)
			}
		})
	}
}
