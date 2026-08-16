package handlers

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"net/url"
	"strings"

	"lambs-server-go/internal/db"
)

// makeThumb generates a ≤maxSide thumbnail from a data URL image.
// Transparent images stay PNG; opaque ones become JPEG (smaller).
// SVGs and already-small images are returned unchanged — no lossy pass.
func makeThumb(dataURL string, maxSide int) (string, error) {
	if dataURL == "" || maxSide <= 0 {
		return dataURL, nil
	}
	if strings.HasPrefix(dataURL, "data:image/svg") {
		return dataURL, nil // vector, already tiny
	}
	comma := strings.Index(dataURL, ",")
	if comma < 0 {
		return dataURL, nil
	}
	// Same 8MB decode-bomb guard as DataURLBytes (R5 perf review).
	if len(dataURL) > 8<<20 {
		return "", fmt.Errorf("image too large")
	}
	raw, err := base64.StdEncoding.DecodeString(dataURL[comma+1:])
	if err != nil {
		return "", err
	}
	// Size guard: reject absurd images before full decode (decompression
	// bombs would otherwise consume hundreds of MB inside a request).
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
		if cfg.Width > 4096 || cfg.Height > 4096 {
			return "", fmt.Errorf("image too large: %dx%d", cfg.Width, cfg.Height)
		}
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return "", err // unsupported format (e.g. webp) — caller keeps original
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxSide && h <= maxSide {
		return dataURL, nil
	}

	scale := float64(maxSide) / float64(max(w, h))
	nw := int(float64(w)*scale + 0.5)
	nh := int(float64(h)*scale + 0.5)
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < nh; y++ {
		for x := 0; x < nw; x++ {
			sx := (float64(x)+0.5)/scale - 0.5
			sy := (float64(y)+0.5)/scale - 0.5
			dst.Set(x, y, bilinear(img, sx, sy))
		}
	}

	hasAlpha := false
	for y := 0; y < nh && !hasAlpha; y++ {
		for x := 0; x < nw; x++ {
			if _, _, _, a := dst.At(x, y).RGBA(); a < 0xffff {
				hasAlpha = true
				break
			}
		}
	}
	var out bytes.Buffer
	if hasAlpha {
		if err := png.Encode(&out, dst); err != nil {
			return "", err
		}
		return "data:image/png;base64," + base64.StdEncoding.EncodeToString(out.Bytes()), nil
	}
	// Opaque: JPEG at 85% is ~5-15KB for a 128px logo
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 85}); err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(out.Bytes()), nil
}

// bilinear samples the source image at (sx, sy) with bilinear interpolation.
func bilinear(src image.Image, sx, sy float64) color.RGBA {
	x0 := int(sx)
	y0 := int(sy)
	x1 := x0 + 1
	y1 := y0 + 1
	fx := sx - float64(x0)
	fy := sy - float64(y0)

	c00 := rgbaAt(src, x0, y0)
	c10 := rgbaAt(src, x1, y0)
	c01 := rgbaAt(src, x0, y1)
	c11 := rgbaAt(src, x1, y1)

	mix := func(a, b color.RGBA, t float64) color.RGBA {
		return color.RGBA{
			R: uint8(float64(a.R)*(1-t) + float64(b.R)*t),
			G: uint8(float64(a.G)*(1-t) + float64(b.G)*t),
			B: uint8(float64(a.B)*(1-t) + float64(b.B)*t),
			A: uint8(float64(a.A)*(1-t) + float64(b.A)*t),
		}
	}
	return mix(mix(c00, c10, fx), mix(c01, c11, fx), fy)
}

func rgbaAt(src image.Image, x, y int) color.RGBA {
	b := src.Bounds()
	if x < b.Min.X {
		x = b.Min.X
	}
	if x >= b.Max.X {
		x = b.Max.X - 1
	}
	if y < b.Min.Y {
		y = b.Min.Y
	}
	if y >= b.Max.Y {
		y = b.Max.Y - 1
	}
	r, g, bl, a := src.At(x, y).RGBA()
	return color.RGBA{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8), uint8(a >> 8)}
}

// DataURLBytes decodes a data URL into raw image bytes. The media type is
// whitelisted to image formats; anything else (e.g. text/html) is rejected.
func DataURLBytes(data string) ([]byte, string, bool) {
	if !strings.HasPrefix(data, "data:") {
		return nil, "", false
	}
	// Decode-bomb guard: an 8MB data URL expands to ~6MB raw at most —
	// anything bigger is not an icon/logo (R5 perf review).
	if len(data) > 8<<20 {
		return nil, "", false
	}
	semi := strings.Index(data, ";")
	comma := strings.Index(data, ",")
	if comma < 0 || comma < 5 || (semi >= 0 && comma < semi) {
		return nil, "", false
	}
	// No ";" means no parameters: data:image/svg+xml,<raw> — the media type
	// runs straight to the comma.
	ct := data[5:comma]
	if semi >= 0 {
		ct = data[5:semi]
	}
	switch ct {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/x-icon", "image/svg+xml":
	default:
		return nil, "", false
	}
	// "base64" must be the exact LAST parameter segment (charset=utf-8;base64) —
	// substring/suffix matches would misfire on values like ;note=xbase64 (R5 C1, R6).
	seg := data[semi+1 : comma]
	if i := strings.LastIndexByte(seg, ';'); i >= 0 {
		seg = seg[i+1:]
	}
	if seg != "base64" {
		// Non-base64 data URL (e.g. data:image/svg+xml,<svg ...>) — the
		// payload is raw (percent-encoded) text after the comma. SVGs saved
		// without base64 previously 404'd (R3-P2).
		raw, err := url.QueryUnescape(data[comma+1:])
		if err != nil {
			return nil, "", false
		}
		return []byte(raw), ct, true
	}
	raw, err := base64.StdEncoding.DecodeString(data[comma+1:])
	if err != nil {
		return nil, "", false
	}
	return raw, ct, true
}

// ProjectLogo serves a project icon as a plain image (public, cached).
// ?full=1 returns the original, otherwise the 128px thumbnail. Image data
// never rides inside JSON payloads — the browser fetches and caches it.
func ProjectLogo(w http.ResponseWriter, r *http.Request, id string) {
	var icon, thumb string
	db.DB.QueryRow("SELECT COALESCE(icon_url,''), COALESCE(icon_thumb,'') FROM projects WHERE id=$1", id).Scan(&icon, &thumb)
	data := thumb
	if r.URL.Query().Get("full") == "1" && icon != "" {
		data = icon
	}
	if data == "" {
		data = icon // no thumb yet (SVG or already-small image) — serve the original
	}
	raw, ct, ok := DataURLBytes(data)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if ct == "image/svg+xml" {
		w.Header().Set("Content-Security-Policy", "sandbox")
	}
	w.Write(raw)
}

// EnsureThumbs adds thumb columns and backfills thumbnails for legacy rows
// (rows created before thumbnails existed). Runs once at startup; new writes
// generate thumbs inline in Create/Update handlers.
// EnsureThumbs runs the schema part synchronously before the listener
// starts — a delayed ALTER previously left a window where the first
// requests hit a users table without the avatar_thumb column. The slow
// backfill is separate (EnsureThumbsBackfill) so a large icon set or slow
// DB cannot block the HTTP listener from coming up.
func EnsureThumbs() {
	if _, err := db.DB.Exec(`ALTER TABLE projects ADD COLUMN IF NOT EXISTS icon_thumb TEXT`); err != nil {
		log.Printf("EnsureThumbs: projects alter failed: %v", err)
		return
	}
	if _, err := db.DB.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_thumb TEXT`); err != nil {
		log.Printf("EnsureThumbs: users alter failed: %v", err)
		return
	}
	// Legacy rows from an old frontend stored the literal string 'null' as
	// avatar_url — normalize to '' so every read path (ListUsers, auth/me,
	// project members) never serves "null" as an image URL (R3 P2).
	if _, err := db.DB.Exec(`UPDATE users SET avatar_url='' WHERE avatar_url='null'`); err != nil {
		log.Printf("EnsureThumbs: null-avatar cleanup failed: %v", err)
	}
}

// EnsureThumbsBackfill regenerates thumbnails for legacy base64 rows. Safe to
// run concurrently with traffic: it only fills empty icon_thumb/avatar_thumb.
func EnsureThumbsBackfill() {
	rows, err := db.DB.Query("SELECT id, COALESCE(icon_url,''), COALESCE(icon_thumb,'') FROM projects WHERE icon_url LIKE 'data:%' AND (icon_thumb IS NULL OR icon_thumb = '')")
	if err != nil {
		log.Printf("EnsureThumbs: query failed: %v", err)
		return
	}
	type pair struct{ id, icon string }
	var jobs []pair
	for rows.Next() {
		var p pair
		var thumb string
		if err := rows.Scan(&p.id, &p.icon, &thumb); err != nil {
			log.Printf("EnsureThumbs: scan failed: %v", err)
			continue
		}
		jobs = append(jobs, p)
	}
	if err := rows.Err(); err != nil {
		log.Printf("EnsureThumbs: projects rows error: %v", err)
	}
	rows.Close()
	for _, p := range jobs {
		if t, err := makeThumb(p.icon, 128); err == nil && t != p.icon {
			// Guarded UPDATE: a concurrent icon_url change must not be
			// overwritten by a thumb generated from the stale value.
			if _, err := db.DB.Exec("UPDATE projects SET icon_thumb=$1 WHERE id=$2 AND (icon_thumb IS NULL OR icon_thumb = '')", t, p.id); err != nil {
				log.Printf("EnsureThumbs: update %s failed: %v", p.id, err)
			}
		}
	}

	urows, err := db.DB.Query("SELECT id, COALESCE(avatar_url::text,''), COALESCE(avatar_thumb,'') FROM users WHERE avatar_url IS NOT NULL AND avatar_url::text LIKE 'data:%' AND (avatar_thumb IS NULL OR avatar_thumb = '')")
	if err != nil {
		log.Printf("EnsureThumbs: users query failed: %v", err)
		return
	}
	defer urows.Close()
	type upair struct{ id, av string }
	var ujobs []upair
	for urows.Next() {
		var u upair
		var thumb string
		if err := urows.Scan(&u.id, &u.av, &thumb); err != nil {
			log.Printf("EnsureThumbs: users scan failed: %v", err)
			continue
		}
		ujobs = append(ujobs, u)
	}
	if err := urows.Err(); err != nil {
		log.Printf("EnsureThumbs: users rows error: %v", err)
	}
	for _, u := range ujobs {
		if t, err := makeThumb(u.av, 128); err == nil && t != u.av {
			// Guarded UPDATE, same as the projects branch (R5 C2).
			if _, err := db.DB.Exec("UPDATE users SET avatar_thumb=$1 WHERE id=$2 AND (avatar_thumb IS NULL OR avatar_thumb = '')", t, u.id); err != nil {
				log.Printf("EnsureThumbs: users update %s failed: %v", u.id, err)
			}
		}
	}
}
