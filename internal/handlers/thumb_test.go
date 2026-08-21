package handlers

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"net/http/httptest"
	"testing"

	"lambs-server-go/internal/db"
)

func TestDataURLBytes(t *testing.T) {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{0x89, 'P', 'N', 'G', 1, 2, 3})
	jpeg := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString([]byte{0xFF, 0xD8, 0xFF})
	svg := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg></svg>"))
	html := "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte("<script>alert(1)</script>"))

	cases := []struct {
		name    string
		in      string
		wantCT  string
		wantRaw []byte
		wantOK  bool
	}{
		{"png", png, "image/png", []byte{0x89, 'P', 'N', 'G', 1, 2, 3}, true},
		{"jpeg", jpeg, "image/jpeg", []byte{0xFF, 0xD8, 0xFF}, true},
		{"svg", svg, "image/svg+xml", []byte("<svg></svg>"), true},
		{"html rejected", html, "", nil, false},
		{"empty", "", "", nil, false},
		{"not data url", "https://x/y.png", "", nil, false},
		{"bad base64", "data:image/png;base64,!!!not-base64!!!", "", nil, false},
		{"svg raw (no base64)", "data:image/svg+xml,<svg></svg>", "image/svg+xml", []byte("<svg></svg>"), true},
		{"svg with charset param", "data:image/svg+xml;charset=utf-8;base64," + base64.StdEncoding.EncodeToString([]byte("<svg></svg>")), "image/svg+xml", []byte("<svg></svg>"), true},
		{"svg note-param containing base64", "data:image/svg+xml;note=xbase64,<svg></svg>", "image/svg+xml", []byte("<svg></svg>"), true},
		{"svg raw pct-encoded", "data:image/svg+xml,%3Csvg%3E%3C/svg%3E", "image/svg+xml", []byte("<svg></svg>"), true},
		{"html raw rejected", "data:text/html,<script>alert(1)</script>", "", nil, false},
		{"oversized rejected", "data:image/png;base64," + base64.StdEncoding.EncodeToString(make([]byte, 8<<20+1)), "", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, ct, ok := DataURLBytes(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok=%v want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if ct != c.wantCT {
				t.Errorf("ct=%q want %q", ct, c.wantCT)
			}
			if !bytes.Equal(raw, c.wantRaw) {
				t.Errorf("raw=%v want %v", raw, c.wantRaw)
			}
		})
	}
}

// TestProjectLogoMissingRow — no project row: 404, never a panic. The lazy
// connection makes QueryRow/Scan fail and the handler must degrade to 404
// (route-matrix gap: this endpoint had zero route-level coverage).
func TestProjectLogoMissingRow(t *testing.T) {
	tdb, _ := sql.Open("postgres", "postgres://u:p@127.0.0.1:1/none")
	old := db.DB
	db.DB = tdb
	t.Cleanup(func() { db.DB = old })

	r := httptest.NewRequest("GET", "/api/projects/no-such/logo", nil)
	w := httptest.NewRecorder()
	ProjectLogo(w, r, "no-such")
	if w.Code != 404 {
		t.Errorf("logo = %d, want 404", w.Code)
	}
}
