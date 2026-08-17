package db

import "strings"

// sensitiveKeySubstrings are the lowercase fragments that mark a column/key
// as credential material in the data browser (R12 security: passwd/pwd/
// api_key/secret were leaking).
var sensitiveKeySubstrings = []string{
	"password", "passwd", "pwd", "token", "secret", "api_key", "apikey",
	"access_key", "private_key", "credential",
}

// IsSensitiveKey reports whether a column/key name carries credential data
// (case-insensitive substring).
func IsSensitiveKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range sensitiveKeySubstrings {
		if strings.Contains(lk, s) {
			return true
		}
	}
	return false
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
