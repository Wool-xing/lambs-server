package handlers

import "lambs-server-go/internal/db"

// redactTabs strips sensitive columns from tab snapshots at serve time.
// Legacy tabs written before redaction may still hold password/token columns;
// write-time redaction alone does not clean them. Input is never mutated.
func redactTabs(tabs interface{}) interface{} {
	arr, ok := tabs.([]interface{})
	if !ok {
		return tabs
	}
	out := make([]interface{}, 0, len(arr))
	for _, item := range arr {
		tab, ok := item.(map[string]interface{})
		if !ok {
			out = append(out, item)
			continue
		}
		colsAny, _ := tab["cols"].([]interface{})
		var cols []interface{}
		var keep []int
		for i, c := range colsAny {
			if s, ok := c.(string); ok && db.IsSensitiveKey(s) {
				continue
			}
			keep = append(keep, i)
			cols = append(cols, c)
		}
		if len(keep) == len(colsAny) {
			out = append(out, tab)
			continue
		}
		nt := map[string]interface{}{"name": tab["name"], "cols": cols, "pk": tab["pk"]}
		rowsAny, _ := tab["rows"].([]interface{})
		rows := make([]interface{}, 0, len(rowsAny))
		for _, r := range rowsAny {
			arr, ok := r.([]interface{})
			if !ok {
				rows = append(rows, r)
				continue
			}
			nr := make([]interface{}, 0, len(keep))
			for _, i := range keep {
				if i < len(arr) {
					nr = append(nr, arr[i])
				}
			}
			rows = append(rows, nr)
		}
		nt["rows"] = rows
		out = append(out, nt)
	}
	return out
}
