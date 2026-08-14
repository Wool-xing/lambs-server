package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
	"lambs-server-go/internal/nginx"
	"lambs-server-go/internal/notify"
	"lambs-server-go/internal/runtime"
)

func SHA256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

var safeColName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateRowCols rejects payload keys that would be spliced into SQL
// column positions by the datasource adapters.
func validateRowCols(data map[string]interface{}) error {
	for k := range data {
		if !safeColName.MatchString(k) {
			return fmt.Errorf("非法列名: %s", k)
		}
	}
	return nil
}

// CheckProjectView returns true if the request's user can VIEW the project:
// super_admin always, any role with projectID in their project_access list.
func CheckProjectView(r *http.Request, projectID string) bool {
	role := r.Header.Get("X-Role")
	if role == "super_admin" {
		return true
	}
	userID := r.Header.Get("X-User-ID")
	var accessStr string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&accessStr)
	var accessIDs []string
	json.Unmarshal([]byte(accessStr), &accessIDs)
	for _, pid := range accessIDs {
		if pid == projectID {
			return true
		}
	}
	return false
}

// checkProjectAccess returns true if the request's user is super_admin,
// or a project_admin who has projectID in their project_access list.
func CheckProjectAccess(r *http.Request, projectID string) bool {
	role := r.Header.Get("X-Role")
	if role == "super_admin" {
		return true
	}
	if role != "project_admin" {
		return false
	}
	userID := r.Header.Get("X-User-ID")
	var accessStr string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&accessStr)
	var accessIDs []string
	json.Unmarshal([]byte(accessStr), &accessIDs)
	for _, pid := range accessIDs {
		if pid == projectID {
			return true
		}
	}
	return false
}

func ListProjects(w http.ResponseWriter, r *http.Request) {
	statusFilter := r.URL.Query().Get("status")
	searchFilter := r.URL.Query().Get("search")
	sortBy := r.URL.Query().Get("sort_by")
	userRole := r.Header.Get("X-Role")
	userID := r.Header.Get("X-User-ID")
	query := "SELECT id, name, repo, description, icon_url, stack, port, db_type, dsn, COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]'), COALESCE(created_at::text,''), COALESCE(updated_at::text,''), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE 1=1"
	var args []interface{}
	argIdx := 0
	if statusFilter != "" && statusFilter != "all" { argIdx++; query += " AND status=$" + strconv.Itoa(argIdx); args = append(args, statusFilter) }
	if searchFilter != "" { argIdx++; query += " AND (name ILIKE $" + strconv.Itoa(argIdx) + " OR repo ILIKE $" + strconv.Itoa(argIdx) + ")"; args = append(args, "%"+searchFilter+"%") }
	// Enforce project_access for non-super_admin users
	if userRole != "super_admin" {
		var accessStr string
		db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&accessStr)
		var accessIDs []string
		json.Unmarshal([]byte(accessStr), &accessIDs)
		if len(accessIDs) == 0 {
			auth.JSONOK(w, map[string]interface{}{"projects": []models.Project{}, "total": 0})
			return
		}
		placeholders := make([]string, len(accessIDs))
		for i, pid := range accessIDs {
			argIdx++
			placeholders[i] = "$" + strconv.Itoa(argIdx)
			args = append(args, pid)
		}
		query += " AND id IN (" + strings.Join(placeholders, ",") + ")"
	}
	switch sortBy {
	case "name": query += " ORDER BY is_pinned DESC, name ASC"
	case "users": query += " ORDER BY is_pinned DESC, users_count DESC"
	default: query += " ORDER BY is_pinned DESC, sort_order"
	}
	rows, err := db.DB.Query(query, args...)
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	defer rows.Close()
	projects := []models.Project{}
	for rows.Next() {
		var p models.Project
		var tagsRaw, featuresRaw, tabsRaw sql.NullString
		rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.StartupCommand, &p.HealthURL, &tagsRaw, &p.OfflineMsg, &featuresRaw, &tabsRaw, &p.CreatedAt, &p.UpdatedAt, &p.BackupIntervalHours, &p.BackupRetentionDays)
		if p.DSN == "" { p.DSN = "—" }
		if tagsRaw.Valid { json.Unmarshal([]byte(tagsRaw.String), &p.Tags) }
		if _, ok := p.Tags.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Tags.(string)), &arr); p.Tags = arr }
		if p.Tags == nil { p.Tags = []interface{}{} }
		if featuresRaw.Valid { json.Unmarshal([]byte(featuresRaw.String), &p.Features) }
		if _, ok := p.Features.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Features.(string)), &arr); p.Features = arr }
		if p.Features == nil { p.Features = []interface{}{} }
		if tabsRaw.Valid { json.Unmarshal([]byte(tabsRaw.String), &p.Tabs) }
		if _, ok := p.Tabs.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Tabs.(string)), &arr); p.Tabs = arr }
		if p.Tabs == nil { p.Tabs = []interface{}{} }
		// DSN is super_admin-only — mask for all other roles
		if userRole != "super_admin" {
			p.DSN = "—"
		}
		projects = append(projects, p)
	}
	if projects == nil { projects = []models.Project{} }
	auth.JSONOK(w, map[string]interface{}{"projects": projects, "total": len(projects)})
}

func GetProject(w http.ResponseWriter, r *http.Request, id string) {
	// Check project_access for non-super_admin
	userRole := r.Header.Get("X-Role")
	if userRole != "super_admin" {
		var accessStr string
		db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", r.Header.Get("X-User-ID")).Scan(&accessStr)
		var accessIDs []string
		json.Unmarshal([]byte(accessStr), &accessIDs)
		allowed := false
		for _, pid := range accessIDs {
			if pid == id { allowed = true; break }
		}
		if !allowed { auth.JSONErr(w, 403, "无权限访问该项目"); return }
	}
	var p models.Project
	var tagsRaw, featuresRaw, tabsRaw sql.NullString
	err := db.DB.QueryRow("SELECT id, name, repo, description, icon_url, stack, port, db_type, dsn, COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]'), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE id=$1", id).
		Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.StartupCommand, &p.HealthURL, &tagsRaw, &p.OfflineMsg, &featuresRaw, &tabsRaw, &p.BackupIntervalHours, &p.BackupRetentionDays)
	if err != nil { auth.JSONErr(w, 404, "项目不存在"); return }
	if p.DSN == "" { p.DSN = "—" }
	if tagsRaw.Valid { json.Unmarshal([]byte(tagsRaw.String), &p.Tags) }
	if _, ok := p.Tags.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Tags.(string)), &arr); p.Tags = arr }
	if p.Tags == nil { p.Tags = []interface{}{} }
	if featuresRaw.Valid { json.Unmarshal([]byte(featuresRaw.String), &p.Features) }
	if _, ok := p.Features.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Features.(string)), &arr); p.Features = arr }
	if p.Features == nil { p.Features = []interface{}{} }
	if tabsRaw.Valid { json.Unmarshal([]byte(tabsRaw.String), &p.Tabs) }
	if _, ok := p.Tabs.(string); ok { var arr []interface{}; json.Unmarshal([]byte(p.Tabs.(string)), &arr); p.Tabs = arr }
	if p.Tabs == nil { p.Tabs = []interface{}{} }
	// DSN is super_admin-only — mask for all other roles
	if userRole != "super_admin" {
		p.DSN = "—"
	}
	auth.JSONOK(w, p)
}

func CreateProject(w http.ResponseWriter, r *http.Request) {
	var p models.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil { auth.JSONErr(w, 400, "无效数据"); return }
	if p.ID == "" { p.ID = p.Repo } // auto-generate ID from repo name
	if p.Status == "" { p.Status = "online" }
	if p.Order == 0 { p.Order = 999 }
	if p.IconCls == "" { p.IconCls = "blue" }
	featuresJSON := "[]"; tabsJSON := "[]"
	if p.Features != nil { if b, err := json.Marshal(p.Features); err == nil { featuresJSON = string(b) } }
	if p.Tabs != nil { if b, err := json.Marshal(p.Tabs); err == nil { tabsJSON = string(b) } }
	tagsJSON := "[]"
	if p.Tags != nil { if b, err := json.Marshal(p.Tags); err == nil { tagsJSON = string(b) } }
	err := db.DB.QueryRow("INSERT INTO projects (id, name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, startup_command, health_url, tags, offline_msg, features, tabs, backup_interval_hours, backup_retention_days) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22::jsonb,$23::jsonb,$24,$25) RETURNING id",
		p.ID, p.Name, p.Repo, p.Desc, p.IconURL, p.Stack, p.Port, p.DB, p.DSN, p.UserCount, p.Status, p.Order, p.Pinned, p.IconCls, p.BasePath, p.BackendURL, p.ServiceName, p.StartupCommand, p.HealthURL, tagsJSON, p.OfflineMsg, featuresJSON, tabsJSON, p.BackupIntervalHours, p.BackupRetentionDays).Scan(&p.ID)
	if err != nil { auth.JSONErr(w, 400, "创建失败: "+err.Error()); return }
	go nginx.Sync()
	auth.JSONCreated(w, p)
}

func UpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	body, _ := io.ReadAll(r.Body)
	var p models.Project
	if err := json.Unmarshal(body, &p); err != nil { auth.JSONErr(w, 400, "无效数据"); return }
	// Non-super_admin must never modify dsn — force keep-current.
	// (Value-based masking check is fragile: payload encoding varies.)
	if r.Header.Get("X-Role") != "super_admin" {
		p.DSN = ""
	}
	// Detect which backup fields were present in the request
	var raw map[string]json.RawMessage
	hasInterval, hasRetention := false, false
	if json.Unmarshal(body, &raw) == nil {
		_, hasInterval = raw["backup_interval_hours"]
		_, hasRetention = raw["backup_retention_days"]
	}
	var cur models.Project
	db.DB.QueryRow("SELECT name, description, icon_url, stack, port, db_type, dsn, backend_url, service_name, base_path, COALESCE(tags::text,'[]'), offline_msg, COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE id=$1", id).
		Scan(&cur.Name, &cur.Desc, &cur.IconURL, &cur.Stack, &cur.Port, &cur.DB, &cur.DSN, &cur.BackendURL, &cur.ServiceName, &cur.BasePath, &cur.Tags, &cur.OfflineMsg, &cur.StartupCommand, &cur.HealthURL, &cur.BackupIntervalHours, &cur.BackupRetentionDays)
	if !hasInterval { p.BackupIntervalHours = cur.BackupIntervalHours }
	if !hasRetention { p.BackupRetentionDays = cur.BackupRetentionDays }
	if p.Name == "" { p.Name = cur.Name }
	if p.Desc == "" { p.Desc = cur.Desc }
	if p.Stack == "" { p.Stack = cur.Stack }
	if p.Port == "" { p.Port = cur.Port }
	if p.DB == "" { p.DB = cur.DB }
	if p.DSN == "" { p.DSN = cur.DSN }
	if p.BasePath == "" { p.BasePath = cur.BasePath }
	if p.ServiceName == "" { p.ServiceName = cur.ServiceName }
	if p.BackendURL == "" { p.BackendURL = cur.BackendURL }
	if p.StartupCommand == "" { p.StartupCommand = cur.StartupCommand }
	if p.HealthURL == "" { p.HealthURL = cur.HealthURL }
	if p.OfflineMsg == "" { p.OfflineMsg = cur.OfflineMsg }
	if p.Tags == nil { p.Tags = cur.Tags }
	tagsJSON, _ := json.Marshal(p.Tags)
	_, err := db.DB.Exec("UPDATE projects SET name=$1, description=$2, icon_url=$3, stack=$4, port=$5, db_type=$6, dsn=$7, backend_url=$8, service_name=$9, base_path=$10, tags=$11::jsonb, offline_msg=$12, startup_command=$13, health_url=$14, backup_interval_hours=$15, backup_retention_days=$16 WHERE id=$17",
		p.Name, p.Desc, p.IconURL, p.Stack, p.Port, p.DB, p.DSN, p.BackendURL, p.ServiceName, p.BasePath, string(tagsJSON), p.OfflineMsg, p.StartupCommand, p.HealthURL, p.BackupIntervalHours, p.BackupRetentionDays, id)
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"updated": id})
}

func DeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	res, err := db.DB.Exec("DELETE FROM projects WHERE id=$1", id)
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	if n, _ := res.RowsAffected(); n == 0 { auth.JSONErr(w, 404, "项目不存在"); return }
	runtime.ProcMgr.Stop(id)
	runtime.TCPProxyMgr.Stop(id)
	runtime.PortMgr.Free(id)
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"deleted": id})
}

func PatchProjectStatus(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var req struct{ Status string `json:"status"` }
	json.NewDecoder(r.Body).Decode(&req)
	var current string
	db.DB.QueryRow("SELECT status FROM projects WHERE id=$1", id).Scan(&current)
	next := "offline"
	if current == "offline" { next = "maintenance" }
	if current == "maintenance" { next = "online" }
	if req.Status != "" { next = req.Status }
	db.DB.Exec("UPDATE projects SET status=$1, updated_at=NOW() WHERE id=$2", next, id)
	var pname string
	db.DB.QueryRow("SELECT name FROM projects WHERE id=$1", id).Scan(&pname)

	// Unified process lifecycle: Lambs manages project processes
	var svcName string
	db.DB.QueryRow("SELECT COALESCE(service_name,'') FROM projects WHERE id=$1", id).Scan(&svcName)
	if next == "online" {
		if svcName != "" {
			go runtime.ProcMgr.Start(id)
		}
		go runtime.TCPProxyMgr.Start(id)
	} else {
		runtime.ProcMgr.Stop(id)
		runtime.TCPProxyMgr.Stop(id)
	}

	statusLabel := map[string]string{"online": "上线", "offline": "已停用", "maintenance": "维护中"}[next]
	nid := fmt.Sprintf("n%d", time.Now().UnixNano())
	db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
		nid, id, "status", "项目状态变更", fmt.Sprintf("「%s」已变更为「%s」", pname, statusLabel))
	go notify.NotifyAdmin("Lambs项目状态变更",
		fmt.Sprintf("项目「%s」状态变更为「%s」\n时间: %s", pname, statusLabel, time.Now().Format("2006-01-02 15:04:05")))
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"status": next})
}

func PinProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var pinned bool
	db.DB.QueryRow("SELECT is_pinned FROM projects WHERE id=$1", id).Scan(&pinned)
	db.DB.Exec("UPDATE projects SET is_pinned=$1 WHERE id=$2", !pinned, id)
	auth.JSONOK(w, map[string]bool{"is_pinned": !pinned})
}

func ReorderProjects(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	// Format 1: {ordered_ids: [...]}
	var body struct {
		OrderedIDs []string `json:"ordered_ids"`
	}
	if json.Unmarshal(raw, &body) == nil && len(body.OrderedIDs) > 0 {
		for i, id := range body.OrderedIDs {
			db.DB.Exec("UPDATE projects SET sort_order=$1 WHERE id=$2", i+1, id)
		}
		auth.JSONOK(w, map[string]string{"status": "ok"})
		return
	}
	// Format 2: [{id, sort_order}]
	var old []struct {
		ID    string `json:"id"`
		Order int    `json:"sort_order"`
	}
	if json.Unmarshal(raw, &old) == nil {
		for _, o := range old {
			db.DB.Exec("UPDATE projects SET sort_order=$1 WHERE id=$2", o.Order, o.ID)
		}
	}
	auth.JSONOK(w, map[string]string{"status": "ok"})
}

func TestConnection(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn, healthURL string
	db.DB.QueryRow("SELECT COALESCE(dsn,''), COALESCE(health_url,'') FROM projects WHERE id=$1", id).Scan(&dsn, &healthURL)
	auth.JSONOK(w, db.TestHealth(dsn, healthURL))
}

func SyncProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	users := db.SyncUserData(dsn)
	userCount := len(users)
	db.DB.Exec("UPDATE projects SET users_count=$1, updated_at=NOW() WHERE id=$2", userCount, id)
	var fRaw sql.NullString
	db.DB.QueryRow("SELECT features::text FROM projects WHERE id=$1", id).Scan(&fRaw)
	if fRaw.Valid && fRaw.String != "" {
		var feats []map[string]interface{}
		if json.Unmarshal([]byte(fRaw.String), &feats) == nil {
			updated := false
			for i, f := range feats { if l, ok := f["label"]; ok && l == "用户数" { feats[i]["value"] = userCount; updated = true; break } }
			if !updated { feats = append(feats, map[string]interface{}{"label":"用户数","value":userCount}) }
			b, _ := json.Marshal(feats)
			db.DB.Exec("UPDATE projects SET features=$1 WHERE id=$2", string(b), id)
		}
	}
	auth.JSONOK(w, map[string]interface{}{"users": users, "count": userCount})
}

func ProjectStats(w http.ResponseWriter, r *http.Request) {
	var total, online, offline, maintenance int
	db.DB.QueryRow("SELECT COUNT(*) FROM projects").Scan(&total)
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE status='online'").Scan(&online)
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE status='offline'").Scan(&offline)
	db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE status='maintenance'").Scan(&maintenance)
	var lambsUsers, projectUsers int
	db.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&lambsUsers)
	db.DB.QueryRow("SELECT COALESCE(SUM(users_count), 0) FROM projects").Scan(&projectUsers)
	auth.JSONOK(w, map[string]int{"total_projects": total, "online": online, "offline": offline, "maintenance": maintenance, "total_users": lambsUsers + projectUsers})
}

func ProjectTables(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectView(r, id) { auth.JSONErr(w, 403, "无权限访问该项目"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	table := r.URL.Query().Get("table")
	src, err := db.NewDataSource(dsn)
	if err != nil { auth.JSONErr(w, 400, err.Error()); return }
	data, cols, pk, err := src.ReadItems(table)
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	if r.URL.Query().Get("format") == "csv" {
		exportCSV(w, id+"_"+table, cols, data)
		return
	}
	actualTable := table
	if actualTable == "" && len(cols) > 0 { actualTable = "users" }
	auth.JSONOK(w, map[string]interface{}{"columns": cols, "rows": data, "table": actualTable, "pk": pk})
}

// ListTableNames returns all user tables in the project's database.
func ListTableNames(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectView(r, id) { auth.JSONErr(w, 403, "无权限访问该项目"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	src, err := db.NewDataSource(dsn)
	if err != nil { auth.JSONErr(w, 400, err.Error()); return }
	tables, err := src.ListCollections()
	if err != nil { auth.JSONErr(w, 500, err.Error()); return }
	if tables == nil { tables = []string{} }
	auth.JSONOK(w, map[string]interface{}{"tables": tables})
}

// refreshTabsSnapshot re-syncs one table's snapshot in the project's tabs jsonb.
func refreshTabsSnapshot(projectID, table string) {
	var dsn, tabsRaw string
	db.DB.QueryRow("SELECT dsn, COALESCE(tabs::text,'[]') FROM projects WHERE id=$1", projectID).Scan(&dsn, &tabsRaw)
	src, err := db.NewDataSource(dsn)
	if err != nil { return }
	rows, cols, pk, err := src.ReadItems(table)
	if err != nil { return }
	var tabRows [][]interface{}
	for _, r := range rows {
		arr := make([]interface{}, len(cols))
		for i, c := range cols { arr[i] = r[c] }
		tabRows = append(tabRows, arr)
	}
	var tabs []map[string]interface{}
	json.Unmarshal([]byte(tabsRaw), &tabs)
	found := false
	for _, t := range tabs {
		if t["name"] == table {
			t["cols"] = cols
			t["rows"] = tabRows
			t["pk"] = pk
			found = true
		}
	}
	if !found {
		tabs = append(tabs, map[string]interface{}{"name": table, "cols": cols, "rows": tabRows, "pk": pk})
	}
	newTabs, _ := json.Marshal(tabs)
	db.DB.Exec("UPDATE projects SET tabs=$1::jsonb WHERE id=$2", string(newTabs), projectID)
}

func UpdateTableRow(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	table := r.URL.Query().Get("table")
	pkCol := r.URL.Query().Get("pk")
	pkVal := r.URL.Query().Get("pkval")
	if table == "" || pkCol == "" || pkVal == "" { auth.JSONErr(w, 400, "缺少table/pk/pkval参数"); return }
	var row map[string]interface{}
	json.NewDecoder(r.Body).Decode(&row)
	if err := validateRowCols(row); err != nil { auth.JSONErr(w, 400, err.Error()); return }
	src, err := db.NewDataSource(dsn)
	if err != nil { auth.JSONErr(w, 400, err.Error()); return }
	if err := src.UpdateItem(table, pkCol, pkVal, row); err != nil { auth.JSONErr(w, 500, err.Error()); return }
	refreshTabsSnapshot(id, table)
	auth.JSONOK(w, map[string]string{"updated": pkVal})
}

func DeleteTableRow(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	table := r.URL.Query().Get("table")
	pkCol := r.URL.Query().Get("pk")
	pkVal := r.URL.Query().Get("pkval")
	if table == "" || pkCol == "" || pkVal == "" { auth.JSONErr(w, 400, "缺少table/pk/pkval参数"); return }
	src, err := db.NewDataSource(dsn)
	if err != nil { auth.JSONErr(w, 400, err.Error()); return }
	if err := src.DeleteItem(table, pkCol, pkVal); err != nil { auth.JSONErr(w, 500, err.Error()); return }
	refreshTabsSnapshot(id, table)
	auth.JSONOK(w, map[string]string{"deleted": pkVal})
}

func InsertTableRow(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	table := r.URL.Query().Get("table")
	if table == "" { auth.JSONErr(w, 400, "缺少table参数"); return }
	var row map[string]interface{}
	json.NewDecoder(r.Body).Decode(&row)
	if err := validateRowCols(row); err != nil { auth.JSONErr(w, 400, err.Error()); return }
	src, err := db.NewDataSource(dsn)
	if err != nil { auth.JSONErr(w, 400, err.Error()); return }
	if err := src.InsertItem(table, row); err != nil { auth.JSONErr(w, 500, err.Error()); return }
	refreshTabsSnapshot(id, table)
	auth.JSONCreated(w, row)
}

func exportCSV(w http.ResponseWriter, filename string, cols []string, rows []map[string]interface{}) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.csv", filename))
	// BOM for Excel
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write(cols)
	for _, row := range rows {
		record := make([]string, len(cols))
		for i, c := range cols {
			record[i] = fmt.Sprintf("%v", row[c])
		}
		cw.Write(record)
	}
	cw.Flush()
}

func ProjectMembers(w http.ResponseWriter, r *http.Request, id string) {
	rows, err := db.DB.Query("SELECT u.id, u.username, u.name, u.email, u.role, u.status, COALESCE(u.avatar_url::text,'') FROM users u WHERE u.project_access::text LIKE '%' || $1 || '%' OR u.role='super_admin'", id)
	if err != nil { auth.JSONOK(w, map[string]interface{}{"members": []models.User{}, "non_members": []models.User{}}); return }
	defer rows.Close()
	members := []models.User{}
	for rows.Next() { var u models.User; var av string; rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status, &av); u.AvatarURL = av; members = append(members, u) }
	if members == nil { members = []models.User{} }
	nRows, _ := db.DB.Query("SELECT id, username, name, email, role, status, COALESCE(avatar_url::text,'') FROM users WHERE role != 'super_admin' AND (project_access IS NULL OR project_access::text NOT LIKE '%' || $1 || '%')", id)
	nonMembers := []models.User{}
	if nRows != nil { defer nRows.Close(); for nRows.Next() { var u models.User; var av string; nRows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status, &av); u.AvatarURL = av; nonMembers = append(nonMembers, u) } }
	if nonMembers == nil { nonMembers = []models.User{} }
	auth.JSONOK(w, map[string]interface{}{"members": members, "non_members": nonMembers})
}

func AddMember(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var req struct{ UserID string `json:"user_id"` }
	json.NewDecoder(r.Body).Decode(&req)
	var access string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", req.UserID).Scan(&access)
	if !strings.Contains(access, id) {
		var arr []string
		json.Unmarshal([]byte(access), &arr)
		arr = append(arr, id)
		newAccess, _ := json.Marshal(arr)
		db.DB.Exec("UPDATE users SET project_access=$1 WHERE id=$2", string(newAccess), req.UserID)
	}
	auth.JSONOK(w, map[string]string{"added": req.UserID})
}

func RemoveMember(w http.ResponseWriter, r *http.Request, id, userID string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var access string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&access)
	var arr []string; json.Unmarshal([]byte(access), &arr)
	var newArr []string
	for _, pid := range arr { if pid != id { newArr = append(newArr, pid) } }
	newAccess, _ := json.Marshal(newArr)
	db.DB.Exec("UPDATE users SET project_access=$1 WHERE id=$2", string(newAccess), userID)
	auth.JSONOK(w, map[string]string{"removed": userID})
}

func RefreshAll(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query("SELECT id, dsn FROM projects WHERE dsn IS NOT NULL AND dsn != '' AND dsn != '—'")
	if err != nil { auth.JSONOK(w, map[string]int{"refreshed": 0}); return }
	defer rows.Close()
	count := 0
	for rows.Next() { var id, dsn string; rows.Scan(&id, &dsn); users := db.SyncUserData(dsn); db.DB.Exec("UPDATE projects SET users_count=$1 WHERE id=$2", len(users), id); count++ }
	auth.JSONOK(w, map[string]int{"refreshed": count})
}

func CloneProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) { auth.JSONErr(w, 403, "需要项目管理员权限"); return }
	var orig models.Project
	err := db.DB.QueryRow("SELECT name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]') FROM projects WHERE id=$1", id).
		Scan(&orig.Name, &orig.Repo, &orig.Desc, &orig.IconURL, &orig.Stack, &orig.Port, &orig.DB, &orig.DSN, &orig.UserCount, &orig.Status, &orig.Order, &orig.Pinned, &orig.IconCls, &orig.BasePath, &orig.BackendURL, &orig.ServiceName, &orig.Tags, &orig.OfflineMsg, &orig.Features, &orig.Tabs)
	if err != nil { auth.JSONErr(w, 404, "项目不存在"); return }
	newID := orig.Repo + "-clone"
	orig.ID = newID
	orig.Name = orig.Name + " (副本)"
	orig.Status = "offline"
	featJSON, _ := json.Marshal(orig.Features); tabsJSON, _ := json.Marshal(orig.Tabs); tagsJSON, _ := json.Marshal(orig.Tags)
	if string(featJSON) == "null" { featJSON = []byte("[]") }
	if string(tabsJSON) == "null" { tabsJSON = []byte("[]") }
	if string(tagsJSON) == "null" { tagsJSON = []byte("[]") }
	// Clone copies metadata + datasource only. Process/routing fields (port,
	// service_name, startup_command, health_url, base_path, backend_url) are
	// intentionally NOT copied — a clone reusing them would clash with the
	// source project when enabled.
	_, err = db.DB.Exec("INSERT INTO projects (id, name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, tags, offline_msg, features, tabs) VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15,$16::jsonb,$17::jsonb)",
		orig.ID, orig.Name, orig.Repo, orig.Desc, orig.IconURL, orig.Stack, orig.DB, orig.DSN, 0, orig.Status, orig.Order, orig.Pinned, orig.IconCls, string(tagsJSON), orig.OfflineMsg, string(featJSON), string(tabsJSON))
	if err != nil { auth.JSONErr(w, 400, "创建副本失败: "+err.Error()); return }
	auth.JSONCreated(w, orig)
}
