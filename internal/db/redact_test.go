package db

import "testing"

func TestRedactSensitive(t *testing.T) {
	tests := []struct {
		name string
		in   []map[string]interface{}
		want []map[string]interface{}
	}{
		{
			name: "removes password and token keys case-insensitive",
			in: []map[string]interface{}{{
				"id":       1,
				"password": "secret",
				"Password": "secret2",
				"api_token": "tok",
				"TOKEN_SECRET": "tok2",
				"name":     "kept",
			}},
			want: []map[string]interface{}{{
				"id":   1,
				"name": "kept",
			}},
		},
		{
			name: "keeps keys that only contain password as substring-less words",
			in: []map[string]interface{}{{
				"passphrase": "not-filtered-by-sql-sources-either",
				"note":       "token here",
				"id":         7,
			}},
			want: []map[string]interface{}{{
				"passphrase": "not-filtered-by-sql-sources-either",
				"note":       "token here",
				"id":         7,
			}},
		},
		{
			name: "empty rows stay empty",
			in:   []map[string]interface{}{},
			want: []map[string]interface{}{},
		},
		{
			name: "nil row map is skipped",
			in:   []map[string]interface{}{nil, {"id": 1, "password": "x"}},
			want: []map[string]interface{}{nil, {"id": 1}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RedactSensitive(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tt.want))
			}
			for i := range tt.want {
				if tt.want[i] == nil {
					if got[i] != nil {
						t.Errorf("row %d: got non-nil %v, want nil", i, got[i])
					}
					continue
				}
				if got[i] == nil {
					t.Errorf("row %d: got nil, want %v", i, tt.want[i])
					continue
				}
				for k, v := range tt.want[i] {
					if got[i][k] != v {
						t.Errorf("row %d key %q = %v, want %v", i, k, got[i][k], v)
					}
				}
				if len(got[i]) != len(tt.want[i]) {
					t.Errorf("row %d has %d keys, want %d", i, len(got[i]), len(tt.want[i]))
				}
			}
		})
	}
}

func TestRedactSensitiveDoesNotMutateInput(t *testing.T) {
	in := []map[string]interface{}{{"password": "secret", "id": 1}}
	RedactSensitive(in)
	if _, ok := in[0]["password"]; !ok {
		t.Fatal("input map was mutated; RedactSensitive must return a new slice/maps")
	}
}

func TestRedactSensitiveCols(t *testing.T) {
	got := RedactSensitiveCols([]string{"id", "password", "api_token", "name", "TOKEN"})
	want := []string{"id", "name"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	// input not mutated
	in := []string{"password", "id"}
	RedactSensitiveCols(in)
	if in[0] != "password" {
		t.Fatal("input slice was mutated")
	}
}
