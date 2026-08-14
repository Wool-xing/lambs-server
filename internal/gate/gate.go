package gate

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
)

// HandleCheck verifies whether a path belongs to a blocked (offline/maintenance) project.
func HandleCheck(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || path == "/" {
		auth.JSONOK(w, map[string]bool{"allowed": true})
		return
	}
	rows, err := db.DB.Query("SELECT base_path, status, name FROM projects WHERE base_path IS NOT NULL AND base_path != ''")
	if err != nil {
		auth.JSONErr(w, 503, "gate: database unavailable")
		return
	}
	defer rows.Close()
	for rows.Next() {
		var bp, status, name string
		rows.Scan(&bp, &status, &name)
		if path == bp || strings.HasPrefix(path, bp+"/") || strings.HasPrefix(path, bp+"?") {
			if status == "offline" {
				auth.JSONErr(w, 403, "该项目已被管理员停用")
				return
			}
			if status == "maintenance" {
				auth.JSONErr(w, 403, "该项目维护中")
				return
			}
		}
	}
	auth.JSONOK(w, map[string]bool{"allowed": true})
}

// HandleCheckInternal is the same as HandleCheck but requires no auth (for nginx auth_request).
func HandleCheckInternal(w http.ResponseWriter, r *http.Request) {
	HandleCheck(w, r)
}

// HandleProjectLogo returns the project's icon for use as favicon.
func HandleProjectLogo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	var iconURL string
	db.DB.QueryRow("SELECT COALESCE(icon_url,'') FROM projects WHERE base_path=$1 OR base_path=$2",
		path, strings.TrimPrefix(path, "/")).Scan(&iconURL)
	if iconURL != "" {
		ct := "image/svg+xml"
		if strings.HasPrefix(iconURL, "data:") {
			if i := strings.Index(iconURL, ";"); i > 5 {
				ct = iconURL[5:i]
			}
		}
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=120")
		w.Write([]byte(iconURL))
		return
	}
	w.WriteHeader(404)
}

// HandleOfflinePage renders the branded offline/maintenance page for a managed project.
func HandleOfflinePage(w http.ResponseWriter, r *http.Request) {
	origURI := r.Header.Get("X-Original-URI")
	path := strings.Split(strings.TrimPrefix(origURI, "/"), "/")[0]
	var name, msg, icon, status string
	db.DB.QueryRow("SELECT name, COALESCE(offline_msg,''), COALESCE(icon_url,''), status FROM projects WHERE base_path LIKE $1 LIMIT 1",
		"/"+path+"%").Scan(&name, &msg, &icon, &status)
	if name == "" {
		name = "Project"
		msg = "该项目当前不可用"
	}
	if msg == "" {
		if status == "maintenance" {
			msg = "该项目正在维护中，请稍后再试。"
		}
		if status == "offline" {
			msg = "该项目已被管理员停用。"
		}
	}
	statusLabel := map[string]string{"offline": "已停用", "maintenance": "维护中"}[status]
	if statusLabel == "" {
		statusLabel = "不可用"
	}

	// Theme from cookies
	accent, accentBg, border := "#FFA13B", "rgba(255,161,59,.10)", "rgba(255,161,59,.18)"
	if ac, _ := r.Cookie("lambs_theme_accent"); ac != nil {
		var a struct{ Accent, AccentBg, Border, Glow string }
		if dec, err := url.QueryUnescape(ac.Value); err == nil && json.Unmarshal([]byte(dec), &a) == nil && a.Accent != "" {
			accent, accentBg, border = a.Accent, a.AccentBg, a.Border
		}
	}
	bgGradient := ""
	if gc, _ := r.Cookie("lambs_theme_grad"); gc != nil {
		if dec, err := url.QueryUnescape(gc.Value); err == nil {
			bgGradient = dec
		}
	}
	if bgGradient == "" {
		bgGradient = "radial-gradient(ellipse at 15% 10%,rgba(0,199,190,.22),transparent 55%),radial-gradient(ellipse at 85% 90%,rgba(255,161,59,.18),transparent 55%),radial-gradient(ellipse at 50% 50%,rgba(184,146,255,.12),transparent 65%),radial-gradient(ellipse at 30% 80%,rgba(56,210,148,.10),transparent 45%)"
	}
	bgGradient = bgGradient + ", #0B0E13"
	glassAlpha, glassBlur := 0.72, 20
	if gc, _ := r.Cookie("lambs_theme_glass"); gc != nil {
		var g struct{ Alpha float64; Blur int }
		if dec, err := url.QueryUnescape(gc.Value); err == nil && json.Unmarshal([]byte(dec), &g) == nil {
			if g.Alpha > 0 {
				glassAlpha = g.Alpha
			}
			if g.Blur > 0 {
				glassBlur = g.Blur
			}
		}
	}
	cardBg := fmt.Sprintf("rgba(17,21,28,%.2f)", glassAlpha)

	iconTag := ""
	if icon != "" && strings.HasPrefix(icon, "data:image/") {
		iconTag = fmt.Sprintf(`<img src="%s" alt="" class="logo">`, html.EscapeString(icon))
	}
	faviconTag := ""
	if icon != "" && strings.HasPrefix(icon, "data:image/") {
		faviconTag = fmt.Sprintf(`<link rel="icon" href="%s">`, html.EscapeString(icon))
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(name) + `</title>` + faviconTag + `<style>
*{margin:0;padding:0;box-sizing:border-box}
body{display:flex;align-items:center;justify-content:center;min-height:100vh;font-family:-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:` + bgGradient + `;color:#8B93A3}
.card{display:flex;flex-direction:column;align-items:center;background:` + cardBg + `;backdrop-filter:blur(` + fmt.Sprintf("%d", glassBlur) + `px);-webkit-backdrop-filter:blur(` + fmt.Sprintf("%d", glassBlur) + `px);border:1px solid ` + border + `;border-radius:20px;padding:56px 64px;max-width:460px;width:90%;text-align:center;box-shadow:0 12px 48px rgba(0,0,0,.5)}
.logo{width:72px;height:72px;border-radius:16px;object-fit:cover;margin-bottom:24px}
.status{display:inline-block;padding:5px 16px;border-radius:20px;font-size:12px;font-weight:600;margin-bottom:20px;background:` + accentBg + `;color:` + accent + `;border:1px solid ` + border + `}
h1{font-size:20px;font-weight:600;color:#E2E4E9;margin-bottom:8px}
p{font-size:14px;color:#8B93A3;line-height:1.6;max-width:340px;margin:0 auto}
.footer{margin-top:28px;font-size:11px;color:rgba(148,163,184,.3)}
</style></head><body><div class="card">` + iconTag + `<div class="status">` + html.EscapeString(statusLabel) + `</div><h1>` + html.EscapeString(name) + `</h1><p>` + html.EscapeString(msg) + `</p><div class="footer">Lambs 管理系统</div></div></body></html>`))
}
