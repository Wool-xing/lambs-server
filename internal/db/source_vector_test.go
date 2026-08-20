package db

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVectorSourceReadSearch — a local httptest server plays the Qdrant
// API: scroll returns points (id + payload flattened), search returns
// scored hits. Qdrant itself has no container here — the adapter's HTTP
// contract is what's under test.
func TestVectorSourceReadSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/points/scroll"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"result": map[string]interface{}{
					"points": []map[string]interface{}{
						{"id": 7, "payload": map[string]interface{}{"name": "alpha", "v": 0.9}},
						{"id": "uuid-1", "payload": map[string]interface{}{"name": "beta"}},
					},
				},
			})
		case strings.Contains(r.URL.Path, "/points/search"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"result": []map[string]interface{}{
					{"id": 7, "score": 0.95, "payload": map[string]interface{}{"name": "alpha"}},
				},
			})
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{}})
		}
	}))
	defer srv.Close()

	s := &VectorSource{dsn: srv.URL}

	rows, cols, pk, err := s.ReadItems("docs", 10, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 || pk != "id" {
		t.Fatalf("read = %v pk=%q", rows, pk)
	}
	if rows[0]["id"] != "7" || rows[0]["name"] != "alpha" {
		t.Fatalf("row0 = %v", rows[0])
	}
	if rows[1]["id"] != "uuid-1" {
		t.Fatalf("row1 id = %v", rows[1]["id"])
	}
	foundID := false
	for _, c := range cols {
		if c == "id" {
			foundID = true
		}
	}
	if !foundID {
		t.Errorf("cols missing id: %v", cols)
	}

	hits, err := s.Search("docs", []float64{0.1, 0.2}, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || fmt.Sprint(hits[0]["id"]) != "7" {
		t.Fatalf("hits = %v", hits)
	}
}
