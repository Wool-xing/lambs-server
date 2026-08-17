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
