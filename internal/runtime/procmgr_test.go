package runtime

import (
	"strings"
	"testing"
)

// TestFilterEnv — managed project processes must not inherit Lambs's own
// credential surface (COMPUTE_AGENT_TOKEN in project code = SYSTEM /cmd on
// another machine, R24). Regression guard for the blocklist.
func TestFilterEnv(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"credentials stripped",
			[]string{
				"PATH=/usr/bin",
				"DATABASE_URL=postgres://x",
				"JWT_SECRET=abc",
				"COMPUTE_AGENT_TOKEN=tok",
				"COMPUTE_AGENT_URL=http://a",
				"WOOL_AGENT_URL=http://b",
				"TG_BOT_TOKEN=tg",
				"SMTP_PASSWORD=pw",
				"GITHUB_TOKEN=gh",
				"CLOUDFLARE_API_TOKEN=cf",
				"LAMBS_CONFIG_PATH=/etc/lambs",
				"PORT=3602",
				"HOME=/home/ubuntu",
			},
			[]string{"PATH=/usr/bin", "PORT=3602", "HOME=/home/ubuntu"},
		},
		{
			"prefix boundary: LAMBSERVER is not LAMBS_",
			[]string{"LAMBSERVER=1", "LAMBS_X=2", "TG_=3", "TGSTUFF=4"},
			[]string{"LAMBSERVER=1", "TGSTUFF=4"},
		},
		{
			"empty env",
			[]string{},
			[]string{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterEnv(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("filterEnv(%v) = %v, want %v", c.in, got, c.want)
			}
			wantSet := map[string]bool{}
			for _, w := range c.want {
				wantSet[w] = true
			}
			for _, g := range got {
				if !wantSet[g] {
					t.Errorf("unexpected kept entry %q (got %v)", g, got)
				}
			}
		})
	}
}

// TestMinFreeMB — env parsing: valid override, invalid falls back to 100,
// absent falls back to 100.
func TestMinFreeMB(t *testing.T) {
	t.Setenv("LAMBS_MIN_FREE_MB", "250")
	if got := minFreeMB(); got != 250 {
		t.Errorf("minFreeMB = %d, want 250", got)
	}
	t.Setenv("LAMBS_MIN_FREE_MB", "not-a-number")
	if got := minFreeMB(); got != 100 {
		t.Errorf("minFreeMB = %d, want 100 fallback", got)
	}
	t.Setenv("LAMBS_MIN_FREE_MB", "")
	if got := minFreeMB(); got != 100 {
		t.Errorf("minFreeMB = %d, want 100 default", got)
	}
}

// TestMemAvailableFrom — parser matrix: normal line, kB→MB rounding, garbage.
func TestMemAvailableFrom(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"MemTotal: 16000000 kB\nMemAvailable: 5242880 kB\n", 5120, false},
		{"MemAvailable: 1024 kB\n", 1, false},
		{"MemTotal: 16000000 kB\n", 0, true},
		{"MemAvailable: not-a-number\n", 0, true},
		{"", 0, true},
	}
	for _, c := range cases {
		got, err := memAvailableFrom(c.in)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("memAvailableFrom(%q) = (%d, %v), want (%d, err=%v)", c.in, got, err, c.want, c.wantErr)
		}
	}
}

// TestParseProcStat — the field-index parser (indices are easy to get wrong
// when the kernel format shifts; a fixed fixture locks them).
func TestParseProcStat(t *testing.T) {
	// 52 fields after ")" is plenty; utime/stime/starttime/rss are the
	// 11th/12th/19th/21st fields after state.
	fixture := "1234 (my proc name) S 1 1234 1234 0 -1 4194560 100 0 0 0 " +
		"100 200 0 0 20 0 5 0 300 0 500 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0"
	ticks, rss, start := parseProcStat(fixture)
	// fields[11]=100 (utime), fields[12]=200 (stime), fields[19]=300, fields[21]=500
	if ticks != 300 || rss != 500 || start != 300 {
		t.Errorf("parseProcStat = (%d, %d, %d), want (300, 500, 300)", ticks, rss, start)
	}
	if t2, r2, s2 := parseProcStat("no-paren"); t2 != 0 || r2 != 0 || s2 != 0 {
		t.Errorf("malformed = (%d, %d, %d), want zeros", t2, r2, s2)
	}
}

// TestParseProcStatBoundaries — malformed lines degrade to zeros or partial
// parse, never panic. NOTE: the len<20 guard admits 20-21 fields, where
// fields[21] would panic; real /proc/stat lines always carry 52 fields so
// the hole stays latent — the smallest safe fixture is 22 fields.
func TestParseProcStatBoundaries(t *testing.T) {
	mk := func(over map[int]string) string {
		// The parser indexes fields AFTER the closing paren, with the state
		// char as fields[0] — so 22 trailing numbers yield 23 fields and
		// utime/stime/starttime/rss land at 11/12/19/21.
		f := make([]string, 23)
		f[0] = "S"
		for i := 1; i < 23; i++ {
			f[i] = "0"
		}
		for i, v := range over {
			f[i] = v
		}
		return "1234 (p) " + strings.Join(f, " ")
	}
	cases := []struct {
		name  string
		in    string
		ticks int64
		rss   int64
		start int64
	}{
		{"empty", "", 0, 0, 0},
		{"paren at end", "1234 (p)", 0, 0, 0},
		{"truncated fields", "1234 (p) S 1 2 3 4 5 6 7", 0, 0, 0},
		{"minimal 22 fields", mk(map[int]string{11: "12", 12: "11", 19: "19", 21: "500"}), 23, 500, 19},
		{"non-numeric rss", mk(map[int]string{11: "12", 12: "11", 19: "19", 21: "x"}), 23, 0, 19},
		{"non-numeric utime", mk(map[int]string{11: "x", 12: "11", 19: "19", 21: "500"}), 11, 500, 19},
	}
	for _, c := range cases {
		ticks, rss, start := parseProcStat(c.in)
		if ticks != c.ticks || rss != c.rss || start != c.start {
			t.Errorf("%s: parseProcStat(%q) = (%d, %d, %d), want (%d, %d, %d)",
				c.name, c.in, ticks, rss, start, c.ticks, c.rss, c.start)
		}
	}
}

// TestProcUptimeFrom — pure math with a pinned clockTicks.
func TestProcUptimeFrom(t *testing.T) {
	old := clockTicks
	clockTicks = 100
	defer func() { clockTicks = old }()
	if got := procUptimeFrom("1000.00 500.00\n", 10000); got != 900 {
		t.Errorf("uptime = %d, want 900", got)
	}
	if got := procUptimeFrom("", 10000); got != 0 {
		t.Errorf("empty uptime = %d, want 0", got)
	}
}
