package runtime

import (
	"testing"
	"time"
)

func TestParseCronValid(t *testing.T) {
	cases := []string{
		"* * * * *",
		"*/5 * * * *",
		"0 2 * * *",
		"0 0 1 1 *",
		"*/15 9-17 * * 1-5",
		"0 12 * * 0",
		"1,2,3 4,5 * * *",
		"0 0 * * 1-5/2",
	}
	for _, expr := range cases {
		if _, err := parseCron(expr); err != nil {
			t.Errorf("parseCron(%q) = err %v, want ok", expr, err)
		}
	}
}

func TestParseCronInvalid(t *testing.T) {
	cases := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 7",
		"abc * * * *",
		"*/0 * * * *",
		"1- * * * *",
	}
	for _, expr := range cases {
		if _, err := parseCron(expr); err == nil {
			t.Errorf("parseCron(%q) = ok, want error", expr)
		}
	}
}

func mustParse(t *testing.T, expr string) *cronSpec {
	t.Helper()
	s, err := parseCron(expr)
	if err != nil {
		t.Fatalf("parseCron(%q): %v", expr, err)
	}
	return s
}

// at builds a time in local zone for nextAfter comparisons.
func at(y int, mon time.Month, d, h, m int) time.Time {
	return time.Date(y, mon, d, h, m, 0, 0, time.Local)
}

func TestNextAfterEvery5Min(t *testing.T) {
	s := mustParse(t, "*/5 * * * *")
	got := s.nextAfter(at(2026, 8, 18, 12, 3))
	want := at(2026, 8, 18, 12, 5)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
	// on the boundary: strictly after the current minute
	got = s.nextAfter(at(2026, 8, 18, 12, 5))
	want = at(2026, 8, 18, 12, 10)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
}

func TestNextAfterDaily(t *testing.T) {
	s := mustParse(t, "0 2 * * *")
	got := s.nextAfter(at(2026, 8, 18, 1, 30))
	want := at(2026, 8, 18, 2, 0)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
	// past 02:00 today → tomorrow
	got = s.nextAfter(at(2026, 8, 18, 3, 0))
	want = at(2026, 8, 19, 2, 0)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
}

func TestNextAfterMonthBoundary(t *testing.T) {
	s := mustParse(t, "0 0 1 1 *")
	got := s.nextAfter(at(2026, 12, 15, 10, 0))
	want := at(2027, 1, 1, 0, 0)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
}

func TestNextAfterDowSunday(t *testing.T) {
	// 2026-08-18 is a Tuesday. Next Sunday (dow=0) is 08-23.
	s := mustParse(t, "0 12 * * 0")
	got := s.nextAfter(at(2026, 8, 18, 10, 0))
	want := at(2026, 8, 23, 12, 0)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
}

func TestNextAfterNeverFires(t *testing.T) {
	// dom=31 AND dow=0 both restricted — impossible date range in months
	// without a Sunday-31st... Feb 31 never exists, but a Sunday on the 31st
	// exists elsewhere, so this CAN fire. Use a truly impossible combo:
	// Feb 31 (dom=31 month=2) — no such date.
	s := mustParse(t, "0 0 31 2 *")
	got := s.nextAfter(at(2026, 8, 18, 10, 0))
	if !got.IsZero() {
		t.Errorf("nextAfter = %v, want zero (never fires)", got)
	}
}

func TestNextAfterStepDow(t *testing.T) {
	// "1-5/2" = Mon,Wed,Fri. 2026-08-18 Tue → next Wed 08-19.
	s := mustParse(t, "0 0 * * 1-5/2")
	got := s.nextAfter(at(2026, 8, 18, 10, 0))
	want := at(2026, 8, 19, 0, 0)
	if !got.Equal(want) {
		t.Errorf("nextAfter = %v, want %v", got, want)
	}
}

// TestCronMatchesTable — matches() semantics, pinned to real 2026-08 dates
// (08-18 is Tuesday, 08-15 Saturday): standard cron OR rule when both dom and
// dow are restricted; single restriction wins; unrestricted stars match all.
func TestCronMatchesTable(t *testing.T) {
	cases := []struct {
		name string
		expr string
		at   time.Time
		want bool
	}{
		{"all stars", "* * * * *", at(2026, 8, 18, 12, 30), true},
		{"minute only", "30 12 * * *", at(2026, 8, 18, 12, 30), true},
		{"minute mismatch", "30 12 * * *", at(2026, 8, 18, 12, 31), false},
		{"hour mismatch", "30 12 * * *", at(2026, 8, 18, 11, 30), false},
		{"dom only hits", "0 0 15 * *", at(2026, 8, 15, 0, 0), true},
		{"dom only misses", "0 0 15 * *", at(2026, 8, 18, 0, 0), false},
		{"dow only hits", "0 0 * * 2", at(2026, 8, 18, 0, 0), true},  // Tue
		{"dow only misses", "0 0 * * 2", at(2026, 8, 15, 0, 0), false}, // Sat
		{"both restricted, dom side", "0 0 15 * 2", at(2026, 8, 15, 0, 0), true},
		{"both restricted, dow side", "0 0 15 * 2", at(2026, 8, 18, 0, 0), true},
		{"both restricted, neither", "0 0 15 * 2", at(2026, 8, 19, 0, 0), false}, // Wed
		{"both restricted, both match", "0 0 18 * 2", at(2026, 8, 18, 0, 0), true},
		{"month restrict hits", "0 0 1 8 *", at(2026, 8, 1, 0, 0), true},
		{"month restrict misses", "0 0 1 8 *", at(2026, 9, 1, 0, 0), false},
	}
	for _, c := range cases {
		s := mustParse(t, c.expr)
		if got := s.matches(c.at); got != c.want {
			t.Errorf("%s: matches(%q @ %v) = %v, want %v", c.name, c.expr, c.at, got, c.want)
		}
	}
}

// TestParseCronFieldBoundaries — the single-field parser: unrestricted flag
// for stars, values for single/list/range/step, and the full invalid matrix
// (bad step, empty bounds, reversed range, out-of-range).
func TestParseCronFieldBoundaries(t *testing.T) {
	valid := []struct {
		spec       string
		min, max   int
		restricted bool
		probe      int // a value that must be set after parse
	}{
		{"*", 0, 59, false, 30},
		{"5", 0, 59, true, 5},
		{"1,3,5", 0, 59, true, 3},
		{"10-20", 0, 59, true, 15},
		{"*/10", 0, 59, false, 40},
		{"1-5/2", 0, 6, true, 3},
		{"0,30", 0, 59, true, 30},
	}
	for _, c := range valid {
		f, restricted, err := parseCronField(c.spec, c.min, c.max)
		if err != nil {
			t.Errorf("parseCronField(%q) err = %v, want ok", c.spec, err)
			continue
		}
		if restricted != c.restricted {
			t.Errorf("parseCronField(%q) restricted = %v, want %v", c.spec, restricted, c.restricted)
		}
		if !f.values[c.probe-c.min] {
			t.Errorf("parseCronField(%q) missing value %d", c.spec, c.probe)
		}
	}
	invalid := []struct{ spec string; min, max int }{
		{"*/0", 0, 59},
		{"1-", 0, 59},
		{"-5", 0, 59},
		{"0-60", 0, 59},
		{"5-3", 0, 59},
		{"abc", 0, 59},
		{"61", 0, 59},
		{"a-b", 0, 59},
		{"7/2", 0, 6},
	}
	for _, c := range invalid {
		if _, _, err := parseCronField(c.spec, c.min, c.max); err == nil {
			t.Errorf("parseCronField(%q) = ok, want error", c.spec)
		}
	}
}
