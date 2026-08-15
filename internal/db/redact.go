package db

import "strings"

// IsSensitiveKey reports whether a column/key name carries password or token
// data. Matches the redaction semantics of the SQL sources (case-insensitive
// substring).
func IsSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	return strings.Contains(lk, "password") || strings.Contains(lk, "token")
}

// RedactSensitive returns a copy of rows with password/token keys removed.
// The input slice and maps are never mutated.
func RedactSensitive(rows []map[string]interface{}) []map[string]interface{} {
	out := make([]map[string]interface{}, len(rows))
	for i, row := range rows {
		if row == nil {
			out[i] = nil
			continue
		}
		clean := make(map[string]interface{}, len(row))
		for k, v := range row {
			if IsSensitiveKey(k) {
				continue
			}
			clean[k] = v
		}
		out[i] = clean
	}
	return out
}

// RedactSensitiveCols returns a copy of cols without password/token columns.
// The input slice is never mutated.
func RedactSensitiveCols(cols []string) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if !IsSensitiveKey(c) {
			out = append(out, c)
		}
	}
	return out
}
