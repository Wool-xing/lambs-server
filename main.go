package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/gate"
	"lambs-server-go/internal/handlers"
	"lambs-server-go/internal/models"
	"lambs-server-go/internal/nginx"
	"lambs-server-go/internal/notify"
	"lambs-server-go/internal/runtime"
)

var lambsConfig models.Config

func sha256Hex(s string) string {
	// Used by handlers via import cycle — defined here as fallback
	return handlers.SHA256Hex(s)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	auth.JSONOK(w, map[string]interface{}{
		"status": "ok", "service": "lambs-server-go", "time": time.Now().Unix(),
	})
}

func handleSystemHealth(w http.ResponseWriter, r *http.Request) {
	// CPU
	cpu := 0.0
	if data, err := os.ReadFile("/proc/stat"); err == nil {
		fields := strings.Fields(strings.Split(string(data), "\n")[0])
		if len(fields) >= 8 {
			vals := make([]uint64, 7)
			for i := 0; i < 7; i++ {
				vals[i], _ = strconv.ParseUint(fields[i+1], 10, 64)
			}
			idle := vals[3] + vals[4]
			total := vals[0] + vals[1] + vals[2] + vals[3] + vals[4] + vals[5] + vals[6]
			// Simplified CPU — no delta tracking across calls
			if total > 0 {
				cpu = float64(total-idle) / float64(total) * 100
			}
		}
	}
	// Memory
	var memTotal, memAvail int
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				f := strings.Fields(line)
				if len(f) > 1 {
					memTotal, _ = strconv.Atoi(f[1])
				}
			}
			if strings.HasPrefix(line, "MemAvailable:") {
				f := strings.Fields(line)
				if len(f) > 1 {
					memAvail, _ = strconv.Atoi(f[1])
				}
			}
		}
	}
	memTotalMB := memTotal / 1024
	memUsedMB := (memTotal - memAvail) / 1024
	// Disk
	var diskUsed, diskTotal float64
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		diskTotal = float64(stat.Blocks*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		diskFree := float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		diskUsed = diskTotal - diskFree
	}
	// Uptime
	var uptimeSec int
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		f := strings.Fields(string(data))
		if len(f) > 0 {
			if u, err := strconv.ParseFloat(f[0], 64); err == nil {
				uptimeSec = int(u)
			}
		}
	}
	auth.JSONOK(w, map[string]interface{}{
		"cpu_percent":     float64(int(cpu*10)) / 10,
		"memory_used_mb":  memUsedMB,
		"memory_total_mb": memTotalMB,
		"disk_used_gb":    float64(int(diskUsed*10)) / 10,
		"disk_total_gb":   float64(int(diskTotal*10)) / 10,
		"uptime_seconds":  uptimeSec,
	})
}

func handleAggregatedLogs(w http.ResponseWriter, r *http.Request) {
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines < 1 || lines > 100 {
		lines = 20
	}
	logs := []map[string]interface{}{}
	auditRows, _ := db.DB.Query("SELECT id, user_id, action, target, detail, created_at::text FROM audit_logs ORDER BY id DESC LIMIT $1", lines/2)
	if auditRows != nil {
		defer auditRows.Close()
		for auditRows.Next() {
			var l models.AuditLog
			auditRows.Scan(&l.ID, &l.UserID, &l.Action, &l.Target, &l.Detail, &l.CreatedAt)
			logs = append(logs, map[string]interface{}{
				"project_name": "Lambs", "level": "info",
				"message": fmt.Sprintf("[%s] %s — %s", l.Action, l.Target, l.Detail),
			})
		}
	}
	pRows, _ := db.DB.Query("SELECT name, status, updated_at::text FROM projects ORDER BY updated_at DESC LIMIT $1", lines/2)
	if pRows != nil {
		defer pRows.Close()
		for pRows.Next() {
			var name, status, updated string
			pRows.Scan(&name, &status, &updated)
			statusLabel := map[string]string{"online": "运行中", "offline": "已离线", "maintenance": "维护中"}[status]
			lvl := "info"
			if status != "online" {
				lvl = "warn"
			}
			logs = append(logs, map[string]interface{}{
				"project_name": name, "level": lvl,
				"message": fmt.Sprintf("状态: %s · 最后更新: %s", statusLabel, updated),
			})
		}
	}
	if logs == nil {
		logs = []map[string]interface{}{}
	}
	auth.JSONOK(w, logs)
}

func handleProjectLogs(w http.ResponseWriter, r *http.Request, id string) {
	auth.JSONOK(w, []map[string]string{{"msg": "service running", "time": time.Now().Format(time.RFC3339)}})
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Config
	cfgPath := os.Getenv("LAMBS_CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "/home/ubuntu/apps/lambs-server/lambs_config.json"
	}
	if data, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(data, &lambsConfig)
	}
	notify.SetConfig(&lambsConfig)

	// JWT
	auth.JWTKey = []byte(os.Getenv("JWT_SECRET"))
	if len(auth.JWTKey) == 0 {
		auth.JWTKey = []byte(lambsConfig.JWTSecret)
	}
	if len(auth.JWTKey) == 0 {
		log.Fatal("JWT_SECRET not set")
	}

	// Database
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL not set")
	}
	if err := db.Init(dsn); err != nil {
		log.Fatal(err)
	}
	defer db.DB.Close()

	if lambsConfig.RuntimeBase == "" {
		lambsConfig.RuntimeBase = "/home/ubuntu/apps"
	}

	// Background workers
	go nginx.Sync()
	go nginx.AutoRefresh()
	go runtime.ProcMgr.HealthMonitor(func() bool { return lambsConfig.RuntimeEnabled })
	go runtime.TCPProxyMgr.IdleMonitor()
	// Scheduled backups: run at startup (catch up missed windows), then every 30 minutes
	go func() {
		time.Sleep(10 * time.Second)
		handlers.RunScheduledBackups()
		ticker := time.NewTicker(30 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			handlers.RunScheduledBackups()
		}
	}()
	// Auto-start online projects on boot
	go func() {
		time.Sleep(3 * time.Second)
		rows, err := db.DB.Query("SELECT id, COALESCE(port,'') FROM projects WHERE status='online' AND ((service_name IS NOT NULL AND service_name != '') OR (port IS NOT NULL AND port != ''))")
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var id, port string
				rows.Scan(&id, &port)
				if port != "" {
					runtime.TCPProxyMgr.Start(id)
				}
				runtime.ProcMgr.Start(id)
			}
		}
	}()

	// ── Routes ──────────────────────────────────────────
	mux := http.NewServeMux()

	// Public
	mux.HandleFunc("POST /api/auth/login", auth.CORS(auth.HandleLogin))
	mux.HandleFunc("GET /api/health", auth.CORS(handleHealth))
	mux.HandleFunc("GET /api/gate/check-internal", auth.CORS(gate.HandleCheckInternal))
	mux.HandleFunc("GET /api/gate/offline-page", auth.CORS(gate.HandleOfflinePage))
	mux.HandleFunc("GET /api/gate/project-logo", auth.CORS(gate.HandleProjectLogo))
	mux.HandleFunc("POST /api/auth/register", auth.CORS(func(w http.ResponseWriter, r *http.Request) {
		auth.JSONErr(w, 400, "Registration not available")
	}))

	a := auth.WithAuth
	sa := auth.WithAdmin

	// Auth
	mux.HandleFunc("GET /api/auth/me", a(auth.HandleMe))
	mux.HandleFunc("GET /api/me", a(auth.HandleMe))

	// Projects
	mux.HandleFunc("GET /api/projects", a(handlers.ListProjects))
	mux.HandleFunc("GET /api/projects/stats", a(handlers.ProjectStats))
	mux.HandleFunc("GET /api/projects/{id}", a(func(w http.ResponseWriter, r *http.Request) { handlers.GetProject(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/projects", sa(handlers.CreateProject))
	mux.HandleFunc("PUT /api/projects/{id}", a(func(w http.ResponseWriter, r *http.Request) { handlers.UpdateProject(w, r, r.PathValue("id")) }))
	mux.HandleFunc("DELETE /api/projects/{id}", sa(func(w http.ResponseWriter, r *http.Request) { handlers.DeleteProject(w, r, r.PathValue("id")) }))
	mux.HandleFunc("PATCH /api/projects/{id}/status", a(func(w http.ResponseWriter, r *http.Request) { handlers.PatchProjectStatus(w, r, r.PathValue("id")) }))
	mux.HandleFunc("PATCH /api/projects/{id}/pin", a(func(w http.ResponseWriter, r *http.Request) { handlers.PinProject(w, r, r.PathValue("id")) }))
	mux.HandleFunc("PATCH /api/projects/reorder", sa(handlers.ReorderProjects))
	mux.HandleFunc("POST /api/projects/{id}/test-connection", a(func(w http.ResponseWriter, r *http.Request) { handlers.TestConnection(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/projects/{id}/sync", a(func(w http.ResponseWriter, r *http.Request) { handlers.SyncProject(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/projects/refresh-all", sa(handlers.RefreshAll))
	mux.HandleFunc("GET /api/projects/{id}/logs", a(func(w http.ResponseWriter, r *http.Request) { handleProjectLogs(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/projects/{id}/tables", a(func(w http.ResponseWriter, r *http.Request) { handlers.ProjectTables(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/projects/{id}/tables/list", a(func(w http.ResponseWriter, r *http.Request) { handlers.ListTableNames(w, r, r.PathValue("id")) }))
	mux.HandleFunc("PUT /api/projects/{id}/data/row", a(func(w http.ResponseWriter, r *http.Request) { handlers.UpdateTableRow(w, r, r.PathValue("id")) }))
	mux.HandleFunc("DELETE /api/projects/{id}/data/row", a(func(w http.ResponseWriter, r *http.Request) { handlers.DeleteTableRow(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/projects/{id}/data/row", a(func(w http.ResponseWriter, r *http.Request) { handlers.InsertTableRow(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/projects/{id}/members", a(func(w http.ResponseWriter, r *http.Request) { handlers.ProjectMembers(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/projects/{id}/members", a(func(w http.ResponseWriter, r *http.Request) { handlers.AddMember(w, r, r.PathValue("id")) }))
	mux.HandleFunc("DELETE /api/projects/{id}/members/{uid}", a(func(w http.ResponseWriter, r *http.Request) { handlers.RemoveMember(w, r, r.PathValue("id"), r.PathValue("uid")) }))
	mux.HandleFunc("POST /api/projects/{id}/clone", sa(func(w http.ResponseWriter, r *http.Request) { handlers.CloneProject(w, r, r.PathValue("id")) }))

	// Users
	mux.HandleFunc("GET /api/users", sa(handlers.ListUsers))
	mux.HandleFunc("POST /api/users", sa(handlers.CreateUser))
	mux.HandleFunc("PUT /api/users/{id}", sa(func(w http.ResponseWriter, r *http.Request) { handlers.UpdateUser(w, r, r.PathValue("id")) }))
	mux.HandleFunc("DELETE /api/users/{id}", sa(func(w http.ResponseWriter, r *http.Request) { handlers.DeleteUser(w, r, r.PathValue("id")) }))
	mux.HandleFunc("POST /api/users/{id}/reset-password", sa(func(w http.ResponseWriter, r *http.Request) { handlers.ResetPassword(w, r, r.PathValue("id")) }))

	// Gate
	mux.HandleFunc("GET /api/gate/check", a(gate.HandleCheck))

	// Settings
	mux.HandleFunc("GET /api/settings/config", sa(func(w http.ResponseWriter, r *http.Request) { handlers.GetConfig(w, r, &lambsConfig) }))
	mux.HandleFunc("PUT /api/settings/config", sa(func(w http.ResponseWriter, r *http.Request) { handlers.UpdateConfig(w, r, &lambsConfig) }))
	mux.HandleFunc("GET /api/settings/export/projects", sa(handlers.ExportProjects))
	mux.HandleFunc("GET /api/settings/export/users", sa(handlers.ExportUsers))
	mux.HandleFunc("GET /api/settings/export/project-users/{id}", sa(func(w http.ResponseWriter, r *http.Request) { handlers.ExportProjectUsers(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/settings/audit-logs", sa(handlers.AuditLogs))
	mux.HandleFunc("GET /api/settings/datasources", sa(handlers.Datasources))

	// Backups
	mux.HandleFunc("POST /api/backups/{id}", a(func(w http.ResponseWriter, r *http.Request) { handlers.CreateBackup(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/backups/{id}", a(func(w http.ResponseWriter, r *http.Request) { handlers.ListBackups(w, r, r.PathValue("id")) }))
	mux.HandleFunc("GET /api/backups/{id}/download/{file}", a(func(w http.ResponseWriter, r *http.Request) { handlers.DownloadBackup(w, r, r.PathValue("id"), r.PathValue("file")) }))
	mux.HandleFunc("DELETE /api/backups/{id}/download/{file}", a(func(w http.ResponseWriter, r *http.Request) { handlers.DeleteBackup(w, r, r.PathValue("id"), r.PathValue("file")) }))
	mux.HandleFunc("POST /api/backups/{id}/restore/{file}", a(func(w http.ResponseWriter, r *http.Request) { handlers.RestoreBackup(w, r, r.PathValue("id"), r.PathValue("file")) }))
	mux.HandleFunc("POST /api/backups/{id}/upload-tg/{file}", a(func(w http.ResponseWriter, r *http.Request) { handlers.UploadBackupToTG(w, r, r.PathValue("id"), r.PathValue("file")) }))

	// Notifications
	mux.HandleFunc("GET /api/notifications", a(handlers.ListNotifications))
	mux.HandleFunc("POST /api/notifications/{nid}/read", a(func(w http.ResponseWriter, r *http.Request) { handlers.ReadNotification(w, r, r.PathValue("nid")) }))
	mux.HandleFunc("POST /api/notifications/read-all", a(handlers.ReadAllNotifications))
	mux.HandleFunc("DELETE /api/notifications/{nid}", a(func(w http.ResponseWriter, r *http.Request) { handlers.DeleteNotification(w, r, r.PathValue("nid")) }))

	// System
	mux.HandleFunc("GET /api/system/health", a(handleSystemHealth))
	mux.HandleFunc("GET /api/logs/aggregated", a(handleAggregatedLogs))

	// Runtime API
	mux.HandleFunc("POST /api/runtime/ports/allocate/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		port, err := runtime.PortMgr.Allocate(r.PathValue("id"))
		if err != nil { auth.JSONErr(w, 500, err.Error()); return }
		auth.JSONOK(w, map[string]interface{}{"project_id": r.PathValue("id"), "port": port})
	}))
	mux.HandleFunc("POST /api/runtime/proc/start/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		if err := runtime.ProcMgr.Start(r.PathValue("id")); err != nil { auth.JSONErr(w, 500, err.Error()); return }
		auth.JSONOK(w, runtime.ProcMgr.Status(r.PathValue("id")))
	}))
	mux.HandleFunc("POST /api/runtime/proc/stop/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		runtime.ProcMgr.Stop(r.PathValue("id"))
		auth.JSONOK(w, map[string]string{"stopped": r.PathValue("id")})
	}))
	mux.HandleFunc("POST /api/runtime/proc/restart/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		runtime.ProcMgr.Restart(r.PathValue("id"))
		auth.JSONOK(w, runtime.ProcMgr.Status(r.PathValue("id")))
	}))
	mux.HandleFunc("GET /api/runtime/proc/status/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		auth.JSONOK(w, runtime.ProcMgr.Status(r.PathValue("id")))
	}))
	mux.HandleFunc("GET /api/runtime/proc/list", sa(func(w http.ResponseWriter, r *http.Request) {
		auth.JSONOK(w, map[string]interface{}{"processes": runtime.ProcMgr.List(), "count": len(runtime.ProcMgr.List())})
	}))
	mux.HandleFunc("POST /api/runtime/proxy/start/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		if err := runtime.TCPProxyMgr.Start(r.PathValue("id")); err != nil { auth.JSONErr(w, 500, err.Error()); return }
		auth.JSONOK(w, map[string]string{"proxy": r.PathValue("id"), "status": "started"})
	}))
	mux.HandleFunc("POST /api/runtime/proxy/stop/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		runtime.TCPProxyMgr.Stop(r.PathValue("id"))
		auth.JSONOK(w, map[string]string{"proxy": r.PathValue("id"), "status": "stopped"})
	}))

	// Start
	port := lambsConfig.Port
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			port = p
		}
	}
	if port == 0 {
		port = 3602
	}
	log.Printf("Lambs Go Server on :%d", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", port), mux))
}
