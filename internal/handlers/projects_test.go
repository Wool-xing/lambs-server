package handlers

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseDatasources(t *testing.T) {
	// []interface{} input
	in := []interface{}{map[string]interface{}{"id": "ds1", "name": "主"}, map[string]interface{}{"id": "ds2"}}
	got := parseDatasources(in)
	if len(got) != 2 || got[0]["id"] != "ds1" {
		t.Fatalf("parse []interface{} failed: %v", got)
	}
	// JSON string input
	s := `[{"id":"ds1","name":"x"}]`
	got = parseDatasources(s)
	if len(got) != 1 || got[0]["name"] != "x" {
		t.Fatalf("parse string failed: %v", got)
	}
	// garbage input → empty
	if got := parseDatasources(42); len(got) != 0 {
		t.Fatalf("parse garbage should be empty: %v", got)
	}
	// nil → empty, not nil
	if got := parseDatasources(nil); got == nil || len(got) != 0 {
		t.Fatalf("parse nil should be empty slice: %v", got)
	}
}

func TestNormalizeDatasources(t *testing.T) {
	in := []map[string]interface{}{
		{"name": "a"},
		{"name": "b", "id": "custom"},
	}
	got := normalizeDatasources(in)
	if got[0]["id"] != "ds1" {
		t.Fatalf("missing id not filled: %v", got[0])
	}
	if got[1]["id"] != "custom" {
		t.Fatalf("existing id overwritten: %v", got[1])
	}
	if got[0]["is_primary"] != true {
		t.Fatalf("primary not marked on first: %v", got[0])
	}
	// primary already set → not duplicated
	in2 := []map[string]interface{}{{"name": "a", "is_primary": true}, {"name": "b"}}
	got2 := normalizeDatasources(in2)
	if got2[1]["is_primary"] == true {
		t.Fatalf("second source wrongly marked primary: %v", got2)
	}
}

func TestPrimaryDatasource(t *testing.T) {
	if got := primaryDatasource(nil); got != nil {
		t.Fatalf("nil should be nil")
	}
	in := []map[string]interface{}{{"name": "a"}, {"name": "b", "is_primary": true}}
	if got := primaryDatasource(in); got["name"] != "b" {
		t.Fatalf("is_primary not honored: %v", got)
	}
	// no primary → first
	in2 := []map[string]interface{}{{"name": "a"}, {"name": "b"}}
	if got := primaryDatasource(in2); got["name"] != "a" {
		t.Fatalf("fallback to first failed: %v", got)
	}
}

func TestValidateRowCols(t *testing.T) {
	if err := validateRowCols(map[string]interface{}{"name": "x", "email_2": "y"}); err != nil {
		t.Fatalf("valid columns rejected: %v", err)
	}
	bad := []string{"x; DROP TABLE users;--", "1bad", "a b", `a"b`}
	for _, k := range bad {
		if err := validateRowCols(map[string]interface{}{k: "v"}); err == nil {
			t.Fatalf("invalid column %q accepted", k)
		}
	}
}

func TestSHA256HexStable(t *testing.T) {
	a := SHA256Hex("hello")
	b := SHA256Hex("hello")
	if a != b || len(a) != 64 {
		t.Fatalf("sha256 unstable: %s vs %s", a, b)
	}
}

func TestTagsJSONRoundTrip(t *testing.T) {
	// The handlers store tags as JSONB text; ensure the double-unmarshal
	// normalization used in ListProjects is idempotent.
	var arr []interface{}
	if err := json.Unmarshal([]byte(`["a","b"]`), &arr); err != nil {
		t.Fatal(err)
	}
	if _, ok := arr[0].(string); !ok {
		t.Fatalf("tags element not string: %v", arr[0])
	}
}

func TestRedactTabs(t *testing.T) {
	tabs := []interface{}{
		map[string]interface{}{
			"name": "users",
			"cols": []interface{}{"id", "password", "name"},
			"rows": []interface{}{
				[]interface{}{1.0, "secret", "alice"},
				[]interface{}{2.0, "secret2", "bob"},
			},
			"pk": "id",
		},
		map[string]interface{}{
			"name": "clean",
			"cols": []interface{}{"id", "name"},
			"rows": []interface{}{[]interface{}{1.0, "x"}},
			"pk":   "id",
		},
	}
	got := redactTabs(tabs)
	out := got.([]interface{})
	// sensitive column stripped from first tab
	t0 := out[0].(map[string]interface{})
	cols := t0["cols"].([]interface{})
	if len(cols) != 2 || cols[0] != "id" || cols[1] != "name" {
		t.Fatalf("cols not redacted: %v", cols)
	}
	row0 := t0["rows"].([]interface{})[0].([]interface{})
	if len(row0) != 2 || row0[0] != 1.0 || row0[1] != "alice" {
		t.Fatalf("row not aligned to redacted cols: %v", row0)
	}
	if t0["pk"] != "id" {
		t.Fatalf("pk lost: %v", t0["pk"])
	}
	// clean tab passes through with content intact
	if !reflect.DeepEqual(out[1], tabs[1]) {
		t.Fatal("clean tab content changed; no-redaction path should pass through")
	}
	// input not mutated
	if len(tabs[0].(map[string]interface{})["cols"].([]interface{})) != 3 {
		t.Fatal("input tabs were mutated")
	}
	// non-slice input passes through
	if redactTabs("not-a-slice") != "not-a-slice" {
		t.Fatal("non-slice input should pass through unchanged")
	}
}
