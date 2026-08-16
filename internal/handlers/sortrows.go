package handlers

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// sortRows sorts in place by sortCol. The whole column is prescanned: if
// every value parses as a float, the column compares numerically (R3-7:
// string compare ordered 10 before 2); otherwise it falls back to
// case-insensitive string compare. Per-column mode keeps the comparator
// transitive — mixing modes per value pair would make order undefined for
// columns holding numbers and text.
func sortRows(data []map[string]interface{}, sortCol, dir string) {
	if dir != "desc" {
		dir = "asc"
	}
	numeric := true
	for _, row := range data {
		if _, err := strconv.ParseFloat(fmt.Sprintf("%v", row[sortCol]), 64); err != nil {
			numeric = false
			break
		}
	}
	sort.SliceStable(data, func(i, j int) bool {
		a, b := data[i][sortCol], data[j][sortCol]
		if numeric {
			af, _ := strconv.ParseFloat(fmt.Sprintf("%v", a), 64)
			bf, _ := strconv.ParseFloat(fmt.Sprintf("%v", b), 64)
			if dir == "asc" {
				return af < bf
			}
			return af > bf
		}
		as := strings.ToLower(fmt.Sprintf("%v", a))
		bs := strings.ToLower(fmt.Sprintf("%v", b))
		if dir == "asc" {
			return as < bs
		}
		return as > bs
	})
}
