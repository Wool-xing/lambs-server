package db

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRestListCollectionsShapes — array form, wrapper form, 500, and
// non-JSON bodies all resolve per the ListCollections contract.
func TestRestListCollectionsShapes(t *testing.T) {
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "array":
			json.NewEncoder(w).Encode([]string{"a", "b"})
		case "wrapper":
			json.NewEncoder(w).Encode(map[string]interface{}{"collections": []string{"c1"}})
		case "err500":
			http.Error(w, "boom", 500)
		case "garbage":
			w.Write([]byte("not json at all"))
		}
	}))
	defer srv.Close()
	s := &RESTSource{dsn: srv.URL}

	mode = "array"
	if cols, err := s.ListCollections(); err != nil || len(cols) != 2 || cols[1] != "b" {
		t.Fatalf("array form = %v, %v", cols, err)
	}
	mode = "wrapper"
	if cols, err := s.ListCollections(); err != nil || len(cols) != 1 || cols[0] != "c1" {
		t.Fatalf("wrapper form = %v, %v", cols, err)
	}
	mode = "err500"
	if _, err := s.ListCollections(); err == nil {
		t.Fatal("500 should error")
	}
	mode = "garbage"
	if cols, err := s.ListCollections(); err != nil || len(cols) != 0 {
		t.Fatalf("garbage = %v, %v", cols, err)
	}
}
