package db

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestVectorListCollections — GET /collections decoded into names.
func TestVectorListCollections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/collections" && r.Method == "GET" {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"result": map[string]interface{}{
					"collections": []map[string]interface{}{{"name": "docs"}, {"name": "logs"}},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	s := &VectorSource{dsn: srv.URL}
	cols, err := s.ListCollections()
	if err != nil {
		t.Fatalf("ListCollections: %v", err)
	}
	if len(cols) != 2 || cols[0] != "docs" || cols[1] != "logs" {
		t.Fatalf("cols = %v", cols)
	}
}

// TestVectorWritePaths — Insert/Update/Delete plus the auto-create branch:
// first points PUT answers 400 "doesn't exist", collection PUT succeeds,
// retried points PUT succeeds.
func TestVectorWritePaths(t *testing.T) {
	var mu sync.Mutex
	putPointsCalls := 0
	collectionCreates := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/collections/") && strings.Contains(r.URL.Path, "/points"):
			// Fail every first vector-bearing upsert per collection state —
			// auto-create then retries. Covers both InsertItem and UpdateItem.
			var body map[string]interface{}
			json.NewDecoder(r.Body).Decode(&body)
			hasVector := false
			if pts, ok := body["points"].([]interface{}); ok && len(pts) > 0 {
				if p, ok := pts[0].(map[string]interface{}); ok {
					_, hasVector = p["vector"]
				}
			}
			if hasVector {
				putPointsCalls++
				if putPointsCalls%2 == 1 {
					http.Error(w, `{"status":{"error":"Collection docs doesn't exist!"}}`, 400)
					return
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{"status": "completed"}})
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/collections/"):
			collectionCreates++
			json.NewEncoder(w).Encode(map[string]interface{}{"result": true})
		case r.Method == "POST" && strings.Contains(r.URL.Path, "/points/delete"):
			json.NewEncoder(w).Encode(map[string]interface{}{"result": map[string]interface{}{"status": "completed"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s := &VectorSource{dsn: srv.URL}

	// vector insert → first call 400s → ensureCollection → retry succeeds
	if err := s.InsertItem("docs", map[string]interface{}{"id": 1, "vector": []interface{}{0.1, 0.2}, "name": "a"}); err != nil {
		t.Fatalf("insert with auto-create: %v", err)
	}
	if collectionCreates != 1 {
		t.Fatalf("ensureCollection calls after insert = %d, want 1", collectionCreates)
	}
	// id-less insert → UUID generated, no vector → mock accepts first try
	if err := s.InsertItem("docs", map[string]interface{}{"name": "b"}); err != nil {
		t.Fatalf("insert id-less: %v", err)
	}
	// update with vector → auto-create path again (ensureCollection re-called)
	if err := s.UpdateItem("docs", "id", "1", map[string]interface{}{"vector": []interface{}{0.3}, "name": "a2"}); err != nil {
		t.Fatalf("update with auto-create: %v", err)
	}
	if collectionCreates != 2 {
		t.Fatalf("ensureCollection calls after update = %d, want 2", collectionCreates)
	}
	if err := s.DeleteItem("docs", "id", "1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.InsertItem("bad; name", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("injection name should error")
	}
	if err := s.UpdateItem("bad; name", "id", "1", map[string]interface{}{"a": 1}); err == nil {
		t.Fatal("injection name should error")
	}
	if err := s.DeleteItem("bad; name", "id", "1"); err == nil {
		t.Fatal("injection name should error")
	}
}

// TestVectorSearchTopKClamp — topK <1 and >100 both snap to 10.
func TestVectorSearchTopKClamp(t *testing.T) {
	var seenLimits []int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seenLimits = append(seenLimits, int(body["limit"].(float64)))
		mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"result": []map[string]interface{}{}})
	}))
	defer srv.Close()
	s := &VectorSource{dsn: srv.URL}
	if _, err := s.Search("docs", []float64{0.1}, 0); err != nil {
		t.Fatalf("search topK=0: %v", err)
	}
	if _, err := s.Search("docs", []float64{0.1}, 500); err != nil {
		t.Fatalf("search topK=500: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seenLimits) != 2 || seenLimits[0] != 10 || seenLimits[1] != 10 {
		t.Fatalf("clamped limits = %v, want [10 10]", seenLimits)
	}
}

// TestVectorErrorPaths — HTTP 500 surfaces "qdrant error", CountItems
// always errors, invalid collection rejected.
func TestVectorErrorPaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal", 500)
	}))
	defer srv.Close()
	s := &VectorSource{dsn: srv.URL}
	if _, err := s.ListCollections(); err == nil || !strings.Contains(err.Error(), "qdrant error") {
		t.Fatalf("ListCollections err = %v", err)
	}
	if _, _, _, err := s.ReadItems("docs", 10, 0); err == nil {
		t.Fatal("ReadItems should surface 500")
	}
	if _, err := s.CountItems("docs"); err == nil {
		t.Fatal("CountItems should always error for vector source")
	}
	if _, err := s.Search("bad; name", []float64{0.1}, 10); err == nil {
		t.Fatal("injection name should error")
	}
}
