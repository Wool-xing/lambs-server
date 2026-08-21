package handlers

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

// pngDataURL renders a w x h image to a PNG data URL. With alpha=true the
// left half is transparent (drives the PNG-thumbnail branch — a single
// transparent pixel would be averaged away by the bilinear downscale).
func pngDataURL(t *testing.T, w, h int, alpha bool) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if alpha && x < w/2 {
				img.Set(x, y, color.RGBA{0, 0, 0, 0})
				continue
			}
			img.Set(x, y, color.RGBA{200, 30, 90, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// TestMakeThumb — full branch matrix for the thumbnail pipeline. thumb.go
// was the lowest-coverage file in the repo (22.1%): these exercise every
// guard and both output encoders.
func TestMakeThumb(t *testing.T) {
	tiny := pngDataURL(t, 32, 32, false)              // within maxSide → passthrough
	bigOpaque := pngDataURL(t, 600, 400, false)       // downscale → JPEG
	bigAlpha := pngDataURL(t, 600, 400, true)         // downscale → PNG
	huge := pngDataURL(t, 4097, 8, false)             // decode-config bomb guard

	var jbuf bytes.Buffer
	jimg := image.NewRGBA(image.Rect(0, 0, 500, 300))
	jpeg.Encode(&jbuf, jimg, &jpeg.Options{Quality: 90})
	jpegSrc := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jbuf.Bytes())

	cases := []struct {
		name    string
		in      string
		maxSide int
		wantErr string // empty = no error
		wantPfx string // expected output data URL prefix; empty = don't check
		wantDim int    // expected max side after decode; 0 = don't check
	}{
		{"empty passthrough", "", 128, "", "", 0},
		{"zero maxSide passthrough", bigOpaque, 0, "", "", 0},
		{"svg passthrough", "data:image/svg+xml;base64,PHN2Zz48L3N2Zz4=", 128, "", "data:image/svg+xml", 0},
		{"no comma passthrough", "data:image/png;base64-no-comma", 128, "", "data:image/png;base64-no-comma", 0},
		{"oversized data url", "data:image/png;base64," + strings.Repeat("A", 8<<20), 128, "image too large", "", 0},
		{"bad base64", "data:image/png;base64,!!!", 128, "illegal base64", "", 0},
		{"config bomb", huge, 128, "image too large: 4097x8", "", 0},
		{"tiny passthrough", tiny, 128, "", tiny, 0},
		{"big opaque -> jpeg", bigOpaque, 128, "", "data:image/jpeg;base64,", 128},
		{"big alpha -> png", bigAlpha, 128, "", "data:image/png;base64,", 128},
		{"jpeg source downscale", jpegSrc, 128, "", "data:image/jpeg;base64,", 128},
		{"unsupported format", "data:image/bmp;base64,Qk1OAAAA", 128, "image: unknown format", "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := makeThumb(c.in, c.maxSide)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("makeThumb: %v", err)
			}
			if c.wantPfx != "" {
				if !strings.HasPrefix(got, c.wantPfx) {
					t.Fatalf("output prefix = %q, want %q", got[:min(40, len(got))], c.wantPfx)
				}
			}
			if c.wantDim > 0 {
				raw, _, ok := DataURLBytes(got)
				if !ok {
					t.Fatalf("output not decodable data URL")
				}
				cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
				if err != nil {
					t.Fatalf("decode config: %v", err)
				}
				if cfg.Width > c.wantDim || cfg.Height > c.wantDim {
					t.Errorf("thumb %dx%d exceeds maxSide %d", cfg.Width, cfg.Height, c.wantDim)
				}
			}
		})
	}
}

// TestBilinearSamplesInside — corner sampling never leaves the image bounds
// (rgbaAt clamps), even when the interpolated position lands outside.
func TestBilinearSamplesInside(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			src.Set(x, y, color.RGBA{uint8(x * 60), uint8(y * 60), 128, 255})
		}
	}
	for _, pos := range []struct{ x, y float64 }{{-1, -1}, {10, 10}, {3.9, 0.1}, {0.5, 3.5}} {
		c := bilinear(src, pos.x, pos.y)
		if c.A == 0 {
			t.Errorf("bilinear(%v,%v) = %+v, sample escaped bounds", pos.x, pos.y, c)
		}
	}
}

