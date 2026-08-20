package handlers

import (
	"bytes"
	"context"
	"net/url"
	"fmt"
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
	dir := backupBaseDir
	entries, _ := os.ReadDir(dir)
	files := []map[string]interface{}{}
	for _, e := range entries {
		// Exact project match: "app" must not list "app2_*" backups.
		if e.Name() == id || strings.HasPrefix(e.Name(), id+"_") {
			info, err := e.Info()
			if err != nil {
				log.Printf("backups: info %s: %v", e.Name(), err)
				continue
			}
			files = append(files, map[string]interface{}{"filename": e.Name(), "size_mb": float64(info.Size()) / (1024 * 1024), "created": info.ModTime().Format("2006-01-02 15:04")})
		}
	}
	auth.JSONOK(w, map[string]interface{}{"backups": files})
}

// backupBaseDir is the root of stored backups. Overridable via
// LAMBS_BACKUP_DIR (tests + open-source deployments not on /home/ubuntu).
var backupBaseDir = func() string {
	if d := os.Getenv("LAMBS_BACKUP_DIR"); d != "" {
		return d
	}
	return "/home/ubuntu/lambs-backups"
}()

// parsePGDSN splits a postgres-family DSN into pg_dump arguments. All
// three URL schemes are accepted; "postgres://" was previously untrimmed,
// which corrupted the password into "//xxx" and broke pg_dump auth
// (QA round 5 CI caught it).
func parsePGDSN(dsn string) (user, password, host, port, dbname string) {
	user, host, port, dbname = "lambs_admin", "127.0.0.1", "5433", "lambs"
	trimmed := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(dsn, "postgresql+asyncpg://"), "postgresql://"), "postgres://")
	// URL 解析处理 % 编码与密码内特殊字符 — 手切字符串会拆错 (QA 第 6 轮校准)。
	u, err := url.Parse("postgres://" + trimmed) // scheme needed for host/user parsing
	if err != nil {
		return
	}
	if u.User != nil {
		user = u.User.Username()
		if pw, ok := u.User.Password(); ok {
			password = pw
		}
	}
	if u.Hostname() != "" {
		host = u.Hostname()
	}
	if p := u.Port(); p != "" {
		port = p
	}
	if name := strings.TrimPrefix(u.Path, "/"); name != "" {
		dbname = name
	}
	return
}

func safeBackupPath(id, filename string) (string, error) {
	baseDir := backupBaseDir
	clean := filepath.Clean(filepath.Join(baseDir, filename))
	// Must be within baseDir and filename must belong to this project
	// ("app" must not reach "app2_*" backups). Clean both sides so the
	// prefix check holds on Windows dev machines too (separator mismatch).
	base := filepath.Base(clean)
	if !strings.HasPrefix(clean, filepath.Clean(baseDir)+string(os.PathSeparator)) || (base != id && !strings.HasPrefix(base, id+"_")) {
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
	// WAL-safe restore via the sqlite3 backup API — writing the file directly
	// would orphan -wal/-shm pages and corrupt the live database.
	escaped := strings.Replace(dsn2, "'", "''", -1)
	cmd := exec.Command("sqlite3", backupPath, fmt.Sprintf(".backup '%s'", escaped))
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		log.Printf("restore backup: %s", strings.TrimSpace(errBuf.String()))
		auth.JSONErr(w, 500, "恢复失败")
		return
	}
	auth.JSONOK(w, map[string]string{"restored": filename})
}

func DeleteBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	fpath, err := safeBackupPath(id, filename)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	// 删除失败必须如实返回，不许假成功 (QA 第 3 轮校准)。
	if err := os.Remove(fpath); err != nil {
		log.Printf("DeleteBackup remove: %v", err)
		auth.JSONErr(w, 500, "备份删除失败")
		return
	}
	auth.JSONOK(w, map[string]string{"deleted": filename})
}

func UploadBackupToTG(w http.ResponseWriter, r *http.Request, id, file string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	fpath, err := safeBackupPath(id, file)
	if err != nil { auth.JSONErr(w, 404, "备份不存在"); return }
	caption := fmt.Sprintf("Backup: %s @ %s", id, time.Now().Format("2006-01-02 15:04"))
	result, err := tgbackup.Upload(fpath, caption)
	if err != nil { log.Printf("tg upload: %v", err); auth.JSONErr(w, 500, "上传失败，请稍后再试"); return }
	auth.JSONOK(w, result)
}

func doBackup(projectID, dsn string) map[string]interface{} {
	ts := time.Now().Format("20060102-150405")
	fname := fmt.Sprintf("%s_%s", projectID, ts)
	dir := backupBaseDir
	// 备份含生产数据 — 目录 0700、文件 0600 (QA 第 6 轮校准)。
	os.MkdirAll(dir, 0700)
	if strings.Contains(dsn, "sqlite") {
		fpath := fmt.Sprintf("%s/%s.db", dir, fname)
		path := strings.Replace(strings.Split(dsn, "?")[0], "sqlite:///", "", 1)
		// Use SQLite's online backup API for a consistent snapshot even while the app writes
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sqlite3", path, fmt.Sprintf(".backup '%s'", fpath))
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			log.Printf("sqlite backup: %s", strings.TrimSpace(errBuf.String()))
			return map[string]interface{}{"ok": false, "error": "备份执行失败"}
		}
		info, err := os.Stat(fpath)
		if err != nil { log.Printf("backup stat: %v", err); return map[string]interface{}{"ok": false, "error": "备份失败"} }
		os.Chmod(fpath, 0600)
		return map[string]interface{}{"ok": true, "filename": fname + ".db", "size_mb": float64(info.Size()) / (1024 * 1024)}
	}
	if strings.Contains(dsn, "postgresql") || strings.Contains(dsn, "postgres") {
		fpath := fmt.Sprintf("%s/%s.sql", dir, fname)
		user, password, host, port, dbname := parsePGDSN(dsn)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "pg_dump", "-h", host, "-p", port, "-U", user, "-d", dbname, "-f", fpath, "--no-owner", "--no-acl")
		cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
		out, err := cmd.CombinedOutput()
		if err != nil { log.Printf("pg_dump: %s %v", string(out), err); return map[string]interface{}{"ok": false, "error": "备份执行失败"} }
		info, err := os.Stat(fpath)
		if err != nil { log.Printf("pg_dump stat: %v", err); return map[string]interface{}{"ok": false, "error": "备份文件缺失"} }
		os.Chmod(fpath, 0600)
		return map[string]interface{}{"ok": true, "filename": fname + ".sql", "size_mb": float64(info.Size()) / (1024 * 1024)}
	}
	return map[string]interface{}{"ok": false, "error": "unsupported db type"}
}

// RunScheduledBackups checks all projects with backup schedules and runs due backups + retention cleanup.
func RunScheduledBackups() {
	rows, err := db.DB.Query("SELECT id, COALESCE(dsn,''), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE backup_interval_hours > 0")
	if err != nil { return }
	defer rows.Close()
	dir := backupBaseDir
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
			// Off-box copy: upload to Telegram backup channel (GPG-encrypted).
			// Local-only backups die with the box — this closes the gap.
			if fname, _ := result["filename"].(string); fname != "" {
				fpath := dir + "/" + fname
				if _, err := tgbackup.Upload(fpath, fmt.Sprintf("Scheduled backup: %s @ %s", pid, time.Now().Format("2006-01-02 15:04"))); err != nil {
					log.Printf("scheduled-backup: %s TG upload failed: %v", pid, err)
				}
			}
			go notify.NotifyAdmin("Lambs自动备份完成",
				fmt.Sprintf("项目 %s 自动备份完成\n文件: %v\n大小: %v MB\n时间: %s", pid, result["filename"], result["size_mb"], time.Now().Format("2006-01-02 15:04:05")))
		}
	}
}
