package db

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRESTSourceCountItems: REST has no dedicated count endpoint, so the
// count comes from fetching the collection — the stub used to return an
// error, which made ProjectTables fall back to total=len(window) and the
// frontend believe >page_size tables had exactly one page.
func TestRESTSourceCountItems(t *testing.T) {
	mk := func(t *testing.T, body string) *RESTSource {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return &RESTSource{dsn: srv.URL}
	}

	t.Run("bare array", func(t *testing.T) {
		src := mk(t, `[{"id":"1"},{"id":"2"},{"id":"3"}]`)
		n, err := src.CountItems("users")
		if err != nil || n != 3 {
			t.Errorf("got %d err=%v want 3", n, err)
		}
	})

	t.Run("rows wrapper", func(t *testing.T) {
		src := mk(t, `{"rows":[{"a":1},{"a":2}]}`)
		n, err := src.CountItems("users")
		if err != nil || n != 2 {
			t.Errorf("got %d err=%v want 2", n, err)
		}
	})

	t.Run("5xx propagates error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(503)
		}))
		t.Cleanup(srv.Close)
		src := &RESTSource{dsn: srv.URL}
		if _, err := src.CountItems("users"); err == nil {
			t.Error("want error on 503, got nil")
		}
	})
}
