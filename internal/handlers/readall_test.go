package handlers

import (
	"testing"

	"lambs-server-go/internal/db"
)

// fakeDS scripts chunked reads so the sweep can be tested without a real DB.
type fakeDS struct {
	rows  []map[string]interface{}
	calls int
}

func (f *fakeDS) ListCollections() ([]string, error) { return nil, nil }
func (f *fakeDS) CountItems(string) (int, error)     { return len(f.rows), nil }
func (f *fakeDS) ReadItems(_ string, limit, offset int) ([]map[string]interface{}, []string, string, error) {
	f.calls++
	if offset > len(f.rows) {
		offset = len(f.rows)
	}
	end := offset + limit
	if end > len(f.rows) {
		end = len(f.rows)
	}
	chunk := f.rows[offset:end]
	// Mimic REST/Mongo: an empty page carries no cols/pk, so the sweep must
	// keep the cols/pk captured from earlier non-empty chunks.
	if len(chunk) == 0 {
		return chunk, nil, "", nil
	}
	return chunk, []string{"a"}, "id", nil
}
func (f *fakeDS) InsertItem(string, map[string]interface{}) error { return nil }
func (f *fakeDS) UpdateItem(string, string, string, map[string]interface{}) error {
	return nil
}
func (f *fakeDS) DeleteItem(string, string, string) error { return nil }

var _ db.DataSource = (*fakeDS)(nil)

// TestReadAllItems: the sweep must see every row, not just the first capped
// window (R3-6: search/sort on >500-row tables silently dropped rows).
func TestReadAllItems(t *testing.T) {
	mk := func(n int) []map[string]interface{} {
		rows := make([]map[string]interface{}, n)
		for i := range rows {
			rows[i] = map[string]interface{}{"a": i}
		}
		return rows
	}

	t.Run("1200 rows swept in 3 chunks", func(t *testing.T) {
		f := &fakeDS{rows: mk(1200)}
		got, cols, pk, err := readAllItems(f, "t")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 1200 {
			t.Errorf("got %d rows want 1200", len(got))
		}
		if f.calls != 3 {
			t.Errorf("calls=%d want 3", f.calls)
		}
		if len(cols) != 1 || pk != "id" {
			t.Errorf("cols=%v pk=%q", cols, pk)
		}
	})

	t.Run("exact multiple of chunk size terminates", func(t *testing.T) {
		f := &fakeDS{rows: mk(1000)}
		got, cols, pk, err := readAllItems(f, "t")
		if err != nil {
			t.Fatalf("err=%v", err)
		}
		if len(got) != 1000 || f.calls != 3 {
			t.Errorf("got %d calls %d want 1000/3", len(got), f.calls)
		}
		// Empty terminating chunk must not clobber cols/pk (review HIGH).
		if len(cols) != 1 || cols[0] != "a" || pk != "id" {
			t.Errorf("cols=%v pk=%q want preserved", cols, pk)
		}
	})

	t.Run("empty table", func(t *testing.T) {
		f := &fakeDS{rows: mk(0)}
		got, _, _, err := readAllItems(f, "t")
		if err != nil || len(got) != 0 || f.calls != 1 {
			t.Errorf("got %d calls %d err %v", len(got), f.calls, err)
		}
	})

	t.Run("cap hit returns explicit error, not silent truncation", func(t *testing.T) {
		orig := readAllMax
		readAllMax = 1000 // shrink so the fake's full chunks hit the cap
		defer func() { readAllMax = orig }()
		f := &fakeDS{rows: mk(1200)}
		_, _, _, err := readAllItems(f, "t")
		if err == nil {
			t.Error("want error when cap hit, got nil")
		}
	})
}
