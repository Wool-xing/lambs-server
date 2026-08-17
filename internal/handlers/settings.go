package handlers

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
)

func GetConfig(w http.ResponseWriter, r *http.Request, cfg *models.Config) {
	// Return sanitized copy — never expose secrets
	safe := *cfg
	safe.JWTSecret = ""    // loaded from JWT_SECRET env var, never via API
	safe.SMTPPassword = "" // never expose SMTP credential
	auth.JSONOK(w, &safe)
}

func UpdateConfig(w http.ResponseWriter, r *http.Request, cfg *models.Config) {
	var incoming models.Config
	if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
		auth.JSONErr(w, 400, "无效数据")
		return
	}
	// Never accept secrets from API — these come from env/config file
	if incoming.JWTSecret != "" { incoming.JWTSecret = "" }
	if incoming.SMTPPassword != "" { incoming.SMTPPassword = "" }
	// Only apply safe fields
	cfg.AdminEmail = incoming.AdminEmail
	cfg.Port = incoming.Port
	cfg.RefreshInt = incoming.RefreshInt
	cfg.SMTPHost = incoming.SMTPHost
	cfg.SMTPPort = incoming.SMTPPort
	cfg.SMTPUser = incoming.SMTPUser
	cfg.SMTPFrom = incoming.SMTPFrom
	cfg.RuntimeEnabled = incoming.RuntimeEnabled
	cfg.RuntimeBase = incoming.RuntimeBase
	// Persist to config file, preserving secret values from the existing file
	cfgPath := os.Getenv("LAMBS_CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "/home/ubuntu/apps/lambs-server/lambs_config.json"
	}
	var old models.Config
	if data, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(data, &old)
	}
	cfg.JWTSecret = old.JWTSecret
	cfg.SMTPPassword = old.SMTPPassword
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err == nil {
		err = os.WriteFile(cfgPath, data, 0600)
	}
	if err != nil {
		log.Printf("UpdateConfig persist: %v", err)
		auth.JSONErr(w, 500, "配置保存失败")
		return
	}
	auth.JSONOK(w, map[string]string{"saved": "ok"})
}

func ExportProjects(w http.ResponseWriter, r *http.Request) {
	// Query first: a DB error must produce a clean JSON error, not a CSV
	// body with BOM/header already written followed by JSON (R3-P2).
	rows, err := db.DB.Query("SELECT id, name, status, db_type, users_count FROM projects")
	if err != nil {
		auth.JSONErr(w, 500, "导出失败")
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=projects.csv")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"ID", "Name", "Status", "Database", "Users"})
	for rows.Next() {
		var id, name, status, dbType string
		var users int
		if err := rows.Scan(&id, &name, &status, &dbType, &users); err != nil {
			log.Printf("ExportProjects: scan failed: %v", err)
			continue
		}
		cw.Write([]string{id, name, status, dbType, strconv.Itoa(users)})
	}
	if err := rows.Err(); err != nil {
		log.Printf("ExportProjects: rows error: %v", err)
	}
	cw.Flush()
}

func ExportUsers(w http.ResponseWriter, r *http.Request) {
	// Query first (same BOM/JSON mixing fix as ExportProjects, R3-P2).
	rows, err := db.DB.Query("SELECT id, username, name, email, role, status FROM users")
	if err != nil {
		auth.JSONErr(w, 500, "导出失败")
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=users.csv")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"ID", "Username", "Name", "Email", "Role", "Status"})
	for rows.Next() {
		var u models.User
		if err := rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status); err != nil {
			log.Printf("ExportUsers: scan failed: %v", err)
			continue
		}
		cw.Write([]string{u.ID, u.Username, u.Name, u.Email, u.Role, u.Status})
	}
	if err := rows.Err(); err != nil {
		log.Printf("ExportUsers: rows error: %v", err)
	}
	cw.Flush()
}

func ExportProjectUsers(w http.ResponseWriter, r *http.Request, id string) {
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=project-users-%s.csv", id))
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	if strings.Contains(dsn, "sqlite") {
		dsn2 := strings.TrimPrefix(strings.TrimPrefix(dsn, "sqlite:///"), "sqlite://")
		if idx := strings.Index(dsn2, "?"); idx >= 0 { dsn2 = dsn2[:idx] }
		for _, table := range []string{"users", "user", "accounts", "member"} {
			cmd := exec.Command("sqlite3", "-header", "-csv", dsn2, fmt.Sprintf("SELECT * FROM %s;", table))
			var out bytes.Buffer; cmd.Stdout = &out
			if err := cmd.Run(); err != nil || out.Len() == 0 { continue }
			var filtered bytes.Buffer; header := true; var pwCols []int
			for _, line := range strings.Split(out.String(), "\n") {
				fields := strings.Split(line, ",")
				if header { for i, f := range fields { lf := strings.ToLower(f); if strings.Contains(lf, "password") || strings.Contains(lf, "token") || strings.Contains(lf, "hash") { pwCols = append(pwCols, i) } }; header = false }
				var row []string
				for i, f := range fields { skip := false; for _, ci := range pwCols { if i == ci { skip = true; break } }; if !skip { row = append(row, f) } }
				if len(row) > 0 { filtered.WriteString(strings.Join(row, ",") + "\n") }
			}
			w.Write(filtered.Bytes()); return
		}
		w.Write([]byte("No user table found\n")); return
	}
	users := db.SyncUserData(dsn)
	cw := csv.NewWriter(w)
	if len(users) > 0 {
		var cols []string; for k := range users[0] { cols = append(cols, k) }
		cw.Write(cols)
		for _, row := range users {
			vals := make([]string, len(cols))
			for i, c := range cols {
				v := fmt.Sprintf("%v", row[c])
				// Formula-injection guard: cells starting with = + - @ are
				// executed by Excel on open (R12 security).
				if strings.HasPrefix(v, "=") || strings.HasPrefix(v, "+") || strings.HasPrefix(v, "-") || strings.HasPrefix(v, "@") {
					v = "'" + v
				}
				vals[i] = v
			}
			cw.Write(vals)
		}
	}
	cw.Flush()
}

func Datasources(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query("SELECT id, name, repo, stack, db_type, COALESCE(dsn,'—'), status FROM projects ORDER BY name")
	if err != nil { auth.JSONOK(w, map[string]interface{}{"datasources": []interface{}{}}); return }
	defer rows.Close()
	var ds []map[string]interface{}
	for rows.Next() { var id, name, repo, stack, dbType, dsn, status string; rows.Scan(&id, &name, &repo, &stack, &dbType, &dsn, &status); ds = append(ds, map[string]interface{}{"id": id, "name": name, "repo": repo, "stack": stack, "db_type": dbType, "dsn": dsn, "status": status}) }
	auth.JSONOK(w, map[string]interface{}{"datasources": ds})
}

func AuditLogs(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if page < 1 { page = 1 }
	if pageSize < 1 || pageSize > 200 { pageSize = 50 }
	offset := (page - 1) * pageSize
	var total int; db.DB.QueryRow("SELECT COUNT(*) FROM audit_logs").Scan(&total)
	rows, err := db.DB.Query("SELECT id, user_id, action, target, detail, created_at::text FROM audit_logs ORDER BY id DESC LIMIT $1 OFFSET $2", pageSize, offset)
	if err != nil { auth.JSONOK(w, map[string]interface{}{"logs": []models.AuditLog{}, "total": 0, "page": page, "page_size": pageSize}); return }
	defer rows.Close()
	logs := []models.AuditLog{}
	for rows.Next() { var l models.AuditLog; rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Target, &l.Detail, &l.CreatedAt); logs = append(logs, l) }
	auth.JSONOK(w, map[string]interface{}{"logs": logs, "total": total, "page": page, "page_size": pageSize})
}
