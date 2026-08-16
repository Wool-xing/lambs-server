package handlers

import (
	"fmt"
	"testing"
)

func col(t *testing.T, data []map[string]interface{}, key string) []string {
	t.Helper()
	out := make([]string, 0, len(data))
	for _, row := range data {
		out = append(out, fmtSprint(row[key]))
	}
	return out
}

func fmtSprint(v interface{}) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return fmtFloat(x)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func fmtFloat(f float64) string {
	return fmt.Sprintf("%v", f)
}

// TestSortRowsNumeric: numeric-looking values must compare numerically, not
// as strings (R3-7: string compare ordered 10 before 2).
func TestSortRowsNumeric(t *testing.T) {
	rows := func() []map[string]interface{} {
		return []map[string]interface{}{
			{"n": "10"},
			{"n": "2"},
			{"n": "1"},
		}
	}

	t.Run("asc orders 1,2,10", func(t *testing.T) {
		data := rows()
		sortRows(data, "n", "asc")
		got := col(t, data, "n")
		if got[0] != "1" || got[1] != "2" || got[2] != "10" {
			t.Errorf("asc got %v want [1 2 10]", got)
		}
	})

	t.Run("desc orders 10,2,1", func(t *testing.T) {
		data := rows()
		sortRows(data, "n", "desc")
		got := col(t, data, "n")
		if got[0] != "10" || got[1] != "2" || got[2] != "1" {
			t.Errorf("desc got %v want [10 2 1]", got)
		}
	})

	t.Run("real JSON float64 values", func(t *testing.T) {
		data := []map[string]interface{}{
			{"n": float64(10)},
			{"n": float64(2)},
			{"n": float64(1)},
		}
		sortRows(data, "n", "asc")
		got := col(t, data, "n")
		if got[0] != "1" || got[1] != "2" || got[2] != "10" {
			t.Errorf("float64 asc got %v want [1 2 10]", got)
		}
	})
}

// TestSortRowsStringFallback: a column with any non-numeric value uses string
// compare for the WHOLE column — per-pair mode switching would break
// transitivity (review LOW: 10 < 1x < 2 < 10 cycle).
func TestSortRowsStringFallback(t *testing.T) {
	data := []map[string]interface{}{
		{"s": "banana"},
		{"s": "apple"},
	}
	sortRows(data, "s", "asc")
	if data[0]["s"] != "apple" {
		t.Errorf("string fallback got %v want apple first", data[0]["s"])
	}

	mixed := []map[string]interface{}{
		{"s": 10},
		{"s": "apple"},
	}
	sortRows(mixed, "s", "asc")
	got := col(t, mixed, "s")
	// Whole-column string mode: "10" < "apple".
	if got[0] != "10" || got[1] != "apple" {
		t.Errorf("mixed asc got %v want [10 apple]", got)
	}
	sortRows(mixed, "s", "desc")
	got = col(t, mixed, "s")
	if got[0] != "apple" || got[1] != "10" {
		t.Errorf("mixed desc got %v want [apple 10]", got)
	}
}

// TestSortRowsMissingCol: sorting by a column absent from some rows must not
// panic — "<nil>" fails the numeric prescan so the column sorts as strings.
func TestSortRowsMissingCol(t *testing.T) {
	data := []map[string]interface{}{
		{"x": "b"},
		{"y": "a"},
	}
	sortRows(data, "x", "asc")
	got := col(t, data, "x")
	if got[0] != "<nil>" || got[1] != "b" {
		t.Errorf("missing col asc got %v want [<nil> b]", got)
	}
}
