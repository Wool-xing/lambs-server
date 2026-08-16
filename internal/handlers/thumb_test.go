package handlers

import (
	"bytes"
	"encoding/base64"
	"testing"
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
		{"svg raw pct-encoded", "data:image/svg+xml,%3Csvg%3E%3C/svg%3E", "image/svg+xml", []byte("<svg></svg>"), true},
		{"html raw rejected", "data:text/html,<script>alert(1)</script>", "", nil, false},
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
