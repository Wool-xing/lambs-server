package db

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRESTSourceCRUD — a local httptest server plays the REST datasource:
// read (with paging + password/token column redaction), insert, update,
// delete. No container needed — the contract is the HTTP layer.
func TestRESTSourceCRUD(t *testing.T) {
	state := []map[string]interface{}{
		{"id": "1", "name": "alpha", "password": "sekrit", "api_token": "tok"},
		{"id": "2", "name": "beta"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(state)
		case "POST":
			var d map[string]interface{}
			json.NewDecoder(r.Body).Decode(&d)
			state = append(state, d)
			w.WriteHeader(201)
		case "PUT":
			var d map[string]interface{}
			json.NewDecoder(r.Body).Decode(&d)
			for i, row := range state {
				if row["id"] == d["id"] {
					state[i] = d
				}
			}
		case "DELETE":
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			out := state[:0]
			for _, row := range state {
				if row["id"] != id {
					out = append(out, row)
				}
			}
			state = out
		}
	}))
	defer srv.Close()

	s := &RESTSource{dsn: srv.URL}

	rows, cols, pk, err := s.ReadItems("users", 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 1 || pk != "id" {
		t.Fatalf("read = %v pk=%q", rows, pk)
	}
	for _, c := range cols {
		if strings.Contains(strings.ToLower(c), "password") || strings.Contains(strings.ToLower(c), "token") {
			t.Errorf("redaction failed: column %q exposed", c)
		}
	}
	// Page 2.
	rows, _, _, _ = s.ReadItems("users", 1, 1)
	if len(rows) != 1 || rows[0]["id"] != "2" {
		t.Fatalf("page2 = %v", rows)
	}

	if err := s.InsertItem("users", map[string]interface{}{"id": "3", "name": "gamma"}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.UpdateItem("users", "id", "1", map[string]interface{}{"id": "1", "name": "alpha-updated"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	rows, _, _, _ = s.ReadItems("users", 10, 0)
	for _, r := range rows {
		if r["id"] == "1" && r["name"] != "alpha-updated" {
			t.Errorf("update not applied: %v", r)
		}
	}
	if err := s.DeleteItem("users", "id", "3"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	rows, _, _, _ = s.ReadItems("users", 10, 0)
	if len(rows) != 2 {
		t.Errorf("rows after delete = %d, want 2", len(rows))
	}
}
