package handlers

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
	"lambs-server-go/internal/notify"
	"lambs-server-go/internal/tgbackup"
)

func CreateBackup(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var p models.Project
	err := db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&p.DSN)
	if err != nil || p.DSN == "" || p.DSN == "—" { auth.JSONErr(w, 400, "未配置数据源"); return }
	result := doBackup(id, p.DSN)
	if result["ok"] == true {
		go notify.NotifyAdmin("Lambs备份完成",
			fmt.Sprintf("项目 %s 备份完成\n文件: %v\n大小: %v MB\n时间: %s", id, result["filename"], result["size_mb"], time.Now().Format("2006-01-02 15:04:05")))
	}
	auth.JSONOK(w, result)
}

func ListBackups(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	dir := "/home/ubuntu/lambs-backups"
	entries, _ := os.ReadDir(dir)
	files := []map[string]interface{}{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id) {
			info, _ := e.Info()
			files = append(files, map[string]interface{}{"filename": e.Name(), "size_mb": float64(info.Size()) / (1024 * 1024), "created": info.ModTime().Format("2006-01-02 15:04")})
		}
	}
	auth.JSONOK(w, map[string]interface{}{"backups": files})
}

func safeBackupPath(id, filename string) (string, error) {
	baseDir := "/home/ubuntu/lambs-backups"
	clean := filepath.Clean(filepath.Join(baseDir, filename))
	// Must be within baseDir and filename must start with project id
	if !strings.HasPrefix(clean, baseDir+"/") || !strings.HasPrefix(filepath.Base(clean), id) {
		return "", fmt.Errorf("invalid path")
	}
	return clean, nil
}

func DownloadBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	fpath, err := safeBackupPath(id, filename)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	http.ServeFile(w, r, fpath)
}

func RestoreBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn string; db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	if dsn == "" || dsn == "—" { auth.JSONErr(w, 400, "该项目无独立数据库"); return }
	dsn2 := strings.Replace(dsn, "sqlite:///", "", 1)
	dsn2 = strings.Replace(dsn2, "postgresql+asyncpg://", "postgres://", 1)
	if !strings.Contains(dsn, "sqlite") { auth.JSONErr(w, 400, "仅支持恢复 SQLite 数据库"); return }
	if idx := strings.Index(dsn2, "?"); idx >= 0 { dsn2 = dsn2[:idx] }
	backupPath, err := safeBackupPath(id, filename)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	if _, err := os.Stat(backupPath); os.IsNotExist(err) { auth.JSONErr(w, 404, "备份文件不存在"); return }
	src, _ := os.Open(backupPath); defer src.Close()
	dst, err := os.Create(dsn2)
	if err != nil { auth.JSONErr(w, 500, "无法写入数据库文件"); return }
	defer dst.Close(); io.Copy(dst, src)
	auth.JSONOK(w, map[string]string{"restored": filename})
}

func DeleteBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	fpath, err := safeBackupPath(id, filename)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	os.Remove(fpath)
	auth.JSONOK(w, map[string]string{"deleted": filename})
}

func UploadBackupToTG(w http.ResponseWriter, r *http.Request, id, file string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	fpath, err := safeBackupPath(id, file)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	caption := fmt.Sprintf("Backup: %s @ %s", id, time.Now().Format("2006-01-02 15:04"))
	result, err := tgbackup.Upload(fpath, caption)
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	auth.JSONOK(w, result)
}

func doBackup(projectID, dsn string) map[string]interface{} {
	ts := time.Now().Format("20060102-150405")
	fname := fmt.Sprintf("%s_%s", projectID, ts)
	dir := "/home/ubuntu/lambs-backups"
	os.MkdirAll(dir, 0755)
	if strings.Contains(dsn, "sqlite") {
		fpath := fmt.Sprintf("%s/%s.db", dir, fname)
		path := strings.Replace(strings.Split(dsn, "?")[0], "sqlite:///", "", 1)
		// Use SQLite's online backup API for a consistent snapshot even while the app writes
		cmd := exec.Command("sqlite3", path, fmt.Sprintf(".backup '%s'", fpath))
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return map[string]interface{}{"ok": false, "error": "sqlite backup: " + strings.TrimSpace(errBuf.String())}
		}
		info, err := os.Stat(fpath)
		if err != nil { return map[string]interface{}{"ok": false, "error": err.Error()} }
		return map[string]interface{}{"ok": true, "filename": fname + ".db", "size_mb": float64(info.Size()) / (1024 * 1024)}
	}
	if strings.Contains(dsn, "postgresql") || strings.Contains(dsn, "postgres") {
		fpath := fmt.Sprintf("%s/%s.sql", dir, fname)
		parts := strings.Split(strings.TrimPrefix(strings.TrimPrefix(dsn, "postgresql://"), "postgresql+asyncpg://"), "?")[0]
		user, host, port, dbname := "lambs_admin", "127.0.0.1", "5433", "lambs"
		authPart := strings.Split(parts, "@")[0]
		if len(strings.Split(parts, "@")) > 1 {
			user = strings.Split(authPart, ":")[0]
			hostPart := strings.Split(strings.Split(parts, "@")[1], "/")[0]
			if strings.Contains(hostPart, ":") { hp := strings.Split(hostPart, ":"); host, port = hp[0], hp[1] } else { host = hostPart }
		}
		if len(strings.Split(parts, "/")) > 1 { dbname = strings.Split(strings.Split(parts, "/")[len(strings.Split(parts, "/"))-1], "?")[0] }
		password := ""
		if strings.Contains(authPart, ":") { password = strings.Split(authPart, ":")[1] }
		cmd := exec.Command("pg_dump", "-h", host, "-p", port, "-U", user, "-d", dbname, "-f", fpath, "--no-owner", "--no-acl")
		cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
		out, err := cmd.CombinedOutput()
		if err != nil { return map[string]interface{}{"ok": false, "error": string(out) + err.Error()} }
		info, _ := os.Stat(fpath)
		return map[string]interface{}{"ok": true, "filename": fname + ".sql", "size_mb": float64(info.Size()) / (1024 * 1024)}
	}
	return map[string]interface{}{"ok": false, "error": "unsupported db type"}
}

// RunScheduledBackups checks all projects with backup schedules and runs due backups + retention cleanup.
func RunScheduledBackups() {
	rows, err := db.DB.Query("SELECT id, COALESCE(dsn,''), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE backup_interval_hours > 0")
	if err != nil { return }
	defer rows.Close()
	dir := "/home/ubuntu/lambs-backups"
	for rows.Next() {
		var pid, dsn string
		var intervalHours, retentionDays int
		rows.Scan(&pid, &dsn, &intervalHours, &retentionDays)
		if dsn == "" || dsn == "—" { continue }

		// Find latest backup file for this project
		entries, _ := os.ReadDir(dir)
		var latest time.Time
		var latestName string
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), pid+"_") { continue }
			info, err := e.Info()
			if err != nil { continue }
			if info.ModTime().After(latest) { latest = info.ModTime(); latestName = e.Name() }
		}
		// Retention cleanup runs on every tick regardless of backup due-ness
		if retentionDays > 0 {
			cutoff := time.Now().AddDate(0, 0, -retentionDays)
			for _, e := range entries {
				if !strings.HasPrefix(e.Name(), pid+"_") { continue }
				info, err := e.Info()
				if err != nil { continue }
				if info.ModTime().Before(cutoff) {
					os.Remove(dir + "/" + e.Name())
					log.Printf("scheduled-backup: removed expired %s", e.Name())
				}
			}
		}
		// Due if no backup yet, or older than interval
		if latestName != "" && time.Since(latest) < time.Duration(intervalHours)*time.Hour {
			continue
		}
		result := doBackup(pid, dsn)
		log.Printf("scheduled-backup: %s ok=%v %v", pid, result["ok"], result)
		if result["ok"] == true {
			go notify.NotifyAdmin("Lambs自动备份完成",
				fmt.Sprintf("项目 %s 自动备份完成\n文件: %v\n大小: %v MB\n时间: %s", pid, result["filename"], result["size_mb"], time.Now().Format("2006-01-02 15:04:05")))
		}
	}
}
