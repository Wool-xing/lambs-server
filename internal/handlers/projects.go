package handlers

import (
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// parseDatasources normalizes the datasources jsonb into a slice of maps.
func parseDatasources(raw interface{}) []map[string]interface{} {
	switch v := raw.(type) {
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, e := range v {
			if m, ok := e.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	case string:
		var arr []map[string]interface{}
		if json.Unmarshal([]byte(v), &arr) == nil {
			return arr
		}
	}
	return []map[string]interface{}{}
}

// primaryDatasource returns the is_primary source, falling back to the first.
func primaryDatasource(dss []map[string]interface{}) map[string]interface{} {
	if len(dss) == 0 {
		return nil
	}
	for _, d := range dss {
		if b, _ := d["is_primary"].(bool); b {
			return d
		}
	}
	return dss[0]
}

// normalizeDatasources assigns stable ids and a single is_primary marker.
func normalizeDatasources(dss []map[string]interface{}) []map[string]interface{} {
	hasPrimary := false
	for i, d := range dss {
		if id, _ := d["id"].(string); id == "" {
			d["id"] = fmt.Sprintf("ds%d", i+1)
		}
		if b, _ := d["is_primary"].(bool); b {
			hasPrimary = true
		}
	}
	if !hasPrimary && len(dss) > 0 {
		dss[0]["is_primary"] = true
	}
	return dss
}

// resolveDatasource returns the dsn for the requested source id.
// dsID "" means the legacy primary (projects.dsn column).
func resolveDatasource(projectID, dsID, fallbackDSN string) (string, error) {
	if dsID == "" {
		return fallbackDSN, nil
	}
	var raw string
	if err := db.DB.QueryRow("SELECT COALESCE(datasources::text,'[]') FROM projects WHERE id=$1", projectID).Scan(&raw); err != nil {
		return "", fmt.Errorf("项目不存在")
	}
	for _, d := range parseDatasources(raw) {
		if id, _ := d["id"].(string); id == dsID {
			if dsn, ok := d["dsn"].(string); ok && dsn != "" && dsn != "—" {
				return dsn, nil
			}
			return "", fmt.Errorf("该数据源未配置连接串")
		}
	}
	return "", fmt.Errorf("数据源不存在")
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
	query := "SELECT id, name, repo, description, icon_url, stack, port, db_type, dsn, COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]'), COALESCE(datasources::text,'[]'), COALESCE(services::text,'[]'), COALESCE(created_at::text,''), COALESCE(updated_at::text,''), COALESCE(EXTRACT(EPOCH FROM updated_at)::int,0), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE 1=1"
	var args []interface{}
	argIdx := 0
	if statusFilter != "" && statusFilter != "all" {
		argIdx++
		query += " AND status=$" + strconv.Itoa(argIdx)
		args = append(args, statusFilter)
	}
	if searchFilter != "" {
		argIdx++
		query += " AND (name ILIKE $" + strconv.Itoa(argIdx) + " OR repo ILIKE $" + strconv.Itoa(argIdx) + ")"
		args = append(args, "%"+searchFilter+"%")
	}
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
	case "name":
		query += " ORDER BY is_pinned DESC, name ASC"
	case "users":
		query += " ORDER BY is_pinned DESC, users_count DESC"
	default:
		query += " ORDER BY is_pinned DESC, sort_order"
	}
	rows, err := db.DB.Query(query, args...)
	if err != nil {
		log.Printf("ListProjects: %v", err)
		auth.JSONErr(w, 500, "查询项目失败")
		return
	}
	defer rows.Close()
	projects := []models.Project{}
	for rows.Next() {
		var p models.Project
		var updatedUnix int
		var tagsRaw, featuresRaw, tabsRaw, dsRaw, svcRaw sql.NullString
		rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.StartupCommand, &p.HealthURL, &tagsRaw, &p.OfflineMsg, &featuresRaw, &tabsRaw, &dsRaw, &svcRaw, &p.CreatedAt, &p.UpdatedAt, &updatedUnix, &p.BackupIntervalHours, &p.BackupRetentionDays)
		// Icon rides as a cached image URL, not megabytes of base64 in JSON.
		if p.IconURL != "" {
			p.IconURL = fmt.Sprintf("/api/projects/%s/logo?v=%d", p.ID, updatedUnix)
		}
		if p.DSN == "" {
			p.DSN = "—"
		}
		if tagsRaw.Valid {
			json.Unmarshal([]byte(tagsRaw.String), &p.Tags)
		}
		if _, ok := p.Tags.(string); ok {
			var arr []interface{}
			json.Unmarshal([]byte(p.Tags.(string)), &arr)
			p.Tags = arr
		}
		if p.Tags == nil {
			p.Tags = []interface{}{}
		}
		if featuresRaw.Valid {
			json.Unmarshal([]byte(featuresRaw.String), &p.Features)
		}
		if _, ok := p.Features.(string); ok {
			var arr []interface{}
			json.Unmarshal([]byte(p.Features.(string)), &arr)
			p.Features = arr
		}
		if p.Features == nil {
			p.Features = []interface{}{}
		}
		if tabsRaw.Valid {
			json.Unmarshal([]byte(tabsRaw.String), &p.Tabs)
		}
		if _, ok := p.Tabs.(string); ok {
			var arr []interface{}
			json.Unmarshal([]byte(p.Tabs.(string)), &arr)
			p.Tabs = arr
		}
		if p.Tabs == nil {
			p.Tabs = []interface{}{}
		}
		p.Tabs = redactTabs(p.Tabs)
		if dsRaw.Valid {
			json.Unmarshal([]byte(dsRaw.String), &p.Datasources)
		}
		if _, ok := p.Datasources.(string); ok {
			var arr []interface{}
			json.Unmarshal([]byte(p.Datasources.(string)), &arr)
			p.Datasources = arr
		}
		if p.Datasources == nil {
			p.Datasources = []interface{}{}
		}
		if svcRaw.Valid {
			json.Unmarshal([]byte(svcRaw.String), &p.Services)
		}
		if _, ok := p.Services.(string); ok {
			var arr []interface{}
			json.Unmarshal([]byte(p.Services.(string)), &arr)
			p.Services = arr
		}
		if p.Services == nil {
			p.Services = []interface{}{}
		}
		// DSN/datasources/services are super_admin-only — mask for all other roles
		if userRole != "super_admin" {
			p.DSN = "—"
			p.Datasources = []interface{}{}
			p.Services = []interface{}{}
		}
		projects = append(projects, p)
	}
	if projects == nil {
		projects = []models.Project{}
	}
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
			if pid == id {
				allowed = true
				break
			}
		}
		if !allowed {
			auth.JSONErr(w, 403, "无权限访问该项目")
			return
		}
	}
	var p models.Project
	var updatedUnix int
	var tagsRaw, featuresRaw, tabsRaw, dsRaw, svcRaw sql.NullString
	err := db.DB.QueryRow("SELECT id, name, COALESCE(repo,''), COALESCE(description,''), COALESCE(icon_url,''), COALESCE(stack,''), COALESCE(port,''), COALESCE(db_type,''), COALESCE(dsn,''), COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]'), COALESCE(datasources::text,'[]'), COALESCE(services::text,'[]'), COALESCE(EXTRACT(EPOCH FROM updated_at)::int,0), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0) FROM projects WHERE id=$1", id).
		Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.StartupCommand, &p.HealthURL, &tagsRaw, &p.OfflineMsg, &featuresRaw, &tabsRaw, &dsRaw, &svcRaw, &updatedUnix, &p.BackupIntervalHours, &p.BackupRetentionDays)
	if err != nil {
		auth.JSONErr(w, 404, "项目不存在")
		return
	}
	if p.IconURL != "" {
		p.IconURL = fmt.Sprintf("/api/projects/%s/logo?full=1&v=%d", p.ID, updatedUnix)
	}
	if p.DSN == "" {
		p.DSN = "—"
	}
	if tagsRaw.Valid {
		json.Unmarshal([]byte(tagsRaw.String), &p.Tags)
	}
	if _, ok := p.Tags.(string); ok {
		var arr []interface{}
		json.Unmarshal([]byte(p.Tags.(string)), &arr)
		p.Tags = arr
	}
	if p.Tags == nil {
		p.Tags = []interface{}{}
	}
	if featuresRaw.Valid {
		json.Unmarshal([]byte(featuresRaw.String), &p.Features)
	}
	if _, ok := p.Features.(string); ok {
		var arr []interface{}
		json.Unmarshal([]byte(p.Features.(string)), &arr)
		p.Features = arr
	}
	if p.Features == nil {
		p.Features = []interface{}{}
	}
	// Per-type live stat cards: computed from the datasource itself, so new
	// projects get the right cards automatically. Any failure (offline
	// source, unsupported type) keeps the stored features — page must not
	// break, and cards carry no secrets so all roles see them.
	if p.DSN != "" && p.DSN != "—" {
		if stats, err := db.CollectStats(p.DB, p.DSN); err == nil {
			if cards := db.BuildStatCards(p.DB, stats); cards != nil {
				p.Features = cards
			}
		}
	}
	if tabsRaw.Valid {
		json.Unmarshal([]byte(tabsRaw.String), &p.Tabs)
	}
	if _, ok := p.Tabs.(string); ok {
		var arr []interface{}
		json.Unmarshal([]byte(p.Tabs.(string)), &arr)
		p.Tabs = arr
	}
	if p.Tabs == nil {
		p.Tabs = []interface{}{}
	}
	p.Tabs = redactTabs(p.Tabs)
	if dsRaw.Valid {
		json.Unmarshal([]byte(dsRaw.String), &p.Datasources)
	}
	if _, ok := p.Datasources.(string); ok {
		var arr []interface{}
		json.Unmarshal([]byte(p.Datasources.(string)), &arr)
		p.Datasources = arr
	}
	if p.Datasources == nil {
		p.Datasources = []interface{}{}
	}
	if svcRaw.Valid {
		json.Unmarshal([]byte(svcRaw.String), &p.Services)
	}
	if _, ok := p.Services.(string); ok {
		var arr []interface{}
		json.Unmarshal([]byte(p.Services.(string)), &arr)
		p.Services = arr
	}
	if p.Services == nil {
		p.Services = []interface{}{}
	}
	// DSN/datasources/services are super_admin-only — mask for all other roles
	if userRole != "super_admin" {
		p.DSN = "—"
		p.Datasources = []interface{}{}
		p.Services = []interface{}{}
	}
	auth.JSONOK(w, p)
}

func CreateProject(w http.ResponseWriter, r *http.Request) {
	var p models.Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		auth.JSONErr(w, 400, "无效数据")
		return
	}
	if p.ID == "" {
		p.ID = p.Repo
	} // auto-generate ID from repo name
	// 项目 ID 落进日志/代理文件名与路径 — 必须限制字符集防 ../ 穿越 (R24)
	if !regexp.MustCompile(`^[a-zA-Z0-9._-]+$`).MatchString(p.ID) {
		auth.JSONErr(w, 400, "项目 ID 仅允许字母、数字、点、下划线、连字符")
		return
	}
	if p.Status == "" {
		p.Status = "online"
	}
	if p.Order == 0 {
		p.Order = 999
	}
	if p.IconCls == "" {
		p.IconCls = "blue"
	}
	featuresJSON := "[]"
	tabsJSON := "[]"
	if p.Features != nil {
		if b, err := json.Marshal(p.Features); err == nil {
			featuresJSON = string(b)
		}
	}
	if p.Tabs != nil {
		if b, err := json.Marshal(p.Tabs); err == nil {
			tabsJSON = string(b)
		}
	}
	tagsJSON := "[]"
	if p.Tags != nil {
		if b, err := json.Marshal(p.Tags); err == nil {
			tagsJSON = string(b)
		}
	}
	// Datasources: explicit array wins; otherwise derive one from legacy dsn.
	dss := parseDatasources(p.Datasources)
	if len(dss) == 0 && p.DSN != "" && p.DSN != "—" {
		dss = []map[string]interface{}{{"id": "ds1", "name": "主数据源", "type": p.DB, "dsn": p.DSN, "is_primary": true}}
	}
	dss = normalizeDatasources(dss)
	dsJSON := "[]"
	if b, err := json.Marshal(dss); err == nil {
		dsJSON = string(b)
	}
	// Primary source mirrors legacy dsn/db_type columns.
	if prim := primaryDatasource(dss); prim != nil {
		if s, ok := prim["dsn"].(string); ok {
			p.DSN = s
		}
		if t, ok := prim["type"].(string); ok {
			p.DB = t
		}
	}
	// Shared services: dedupe by name (a project referencing the same shared
	// service twice must not inflate the refcount).
	svcs := parseDatasources(p.Services)
	seen := map[string]bool{}
	uniq := []map[string]interface{}{}
	for _, s := range svcs {
		if name, ok := s["name"].(string); ok && name != "" && !seen[name] {
			seen[name] = true
			uniq = append(uniq, s)
		}
	}
	svcJSON := "[]"
	if b, err := json.Marshal(uniq); err == nil {
		svcJSON = string(b)
	}
	// Auto-allocate a runtime port when left empty.
	if p.Port == "" || p.Port == "—" {
		if port, err := runtime.PortMgr.Allocate(p.ID); err == nil {
			p.Port = fmt.Sprintf("%d", port)
		}
	}
	iconThumb := ""
	if p.IconURL != "" {
		if t, err := makeThumb(p.IconURL, 128); err == nil && t != p.IconURL {
			iconThumb = t
		}
	}
	err := db.DB.QueryRow("INSERT INTO projects (id, name, repo, description, icon_url, icon_thumb, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, startup_command, health_url, tags, offline_msg, features, tabs, datasources, services, backup_interval_hours, backup_retention_days) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23::jsonb,$24::jsonb,$25::jsonb,$26::jsonb,$27,$28) RETURNING id",
		p.ID, p.Name, p.Repo, p.Desc, p.IconURL, iconThumb, p.Stack, p.Port, p.DB, p.DSN, p.UserCount, p.Status, p.Order, p.Pinned, p.IconCls, p.BasePath, p.BackendURL, p.ServiceName, p.StartupCommand, p.HealthURL, tagsJSON, p.OfflineMsg, featuresJSON, tabsJSON, dsJSON, svcJSON, p.BackupIntervalHours, p.BackupRetentionDays).Scan(&p.ID)
	if err != nil {
		log.Printf("CreateProject insert: %v", err)
		auth.JSONErr(w, 400, "创建失败")
		return
	}
	p.Datasources = dss
	p.Services = uniq
	go nginx.Sync()
	// A newly created online project goes through the same lifecycle as a
	// status switch to online: shared services first, then its own process.
	if p.Status == "online" {
		go runtime.ProcMgr.AttachServices(p.ID)
		go runtime.TCPProxyMgr.Start(p.ID)
		go runtime.ProcMgr.Start(p.ID)
	}
	auth.JSONCreated(w, p)
}

func UpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	body, _ := io.ReadAll(r.Body)
	var p models.Project
	if err := json.Unmarshal(body, &p); err != nil {
		auth.JSONErr(w, 400, "无效数据")
		return
	}
	// Non-super_admin must never modify dsn — force keep-current.
	// (Value-based masking check is fragile: payload encoding varies.)
	// Same for every process-control field: startup_command/service_name feed
	// procmgr's exec, port feeds nginx proxy_pass — a project_admin writing
	// them is low-privilege RCE/config injection (R12 security).
	if r.Header.Get("X-Role") != "super_admin" {
		p.DSN = ""
		p.StartupCommand = ""
		p.ServiceName = ""
		p.BackendURL = ""
		p.HealthURL = ""
		p.Port = ""
	}
	// Port feeds nginx proxy_pass directly — must be a plain 1-65535 number
	// for everyone, super_admin included (R12 security: config injection).
	if p.Port != "" {
		if n, err := strconv.Atoi(p.Port); err != nil || n < 1 || n > 65535 {
			auth.JSONErr(w, 400, "端口必须是 1-65535 的数字")
			return
		}
	}
	// Detect which optional fields were present in the request
	var raw map[string]json.RawMessage
	hasInterval, hasRetention, hasDS, hasSvc := false, false, false, false
	if json.Unmarshal(body, &raw) == nil {
		_, hasInterval = raw["backup_interval_hours"]
		_, hasRetention = raw["backup_retention_days"]
		_, hasDS = raw["datasources"]
		_, hasSvc = raw["services"]
	}
	var cur models.Project
	var curDS, curSvc sql.NullString
	if err := db.DB.QueryRow("SELECT name, description, icon_url, stack, port, db_type, dsn, backend_url, service_name, base_path, COALESCE(tags::text,'[]'), offline_msg, COALESCE(startup_command,''), COALESCE(health_url,''), COALESCE(backup_interval_hours,0), COALESCE(backup_retention_days,0), COALESCE(datasources::text,'[]'), COALESCE(services::text,'[]') FROM projects WHERE id=$1", id).
		Scan(&cur.Name, &cur.Desc, &cur.IconURL, &cur.Stack, &cur.Port, &cur.DB, &cur.DSN, &cur.BackendURL, &cur.ServiceName, &cur.BasePath, &cur.Tags, &cur.OfflineMsg, &cur.StartupCommand, &cur.HealthURL, &cur.BackupIntervalHours, &cur.BackupRetentionDays, &curDS, &curSvc); err != nil {
		auth.JSONErr(w, 404, "项目不存在")
		return
	}
	if !hasInterval {
		p.BackupIntervalHours = cur.BackupIntervalHours
	}
	if !hasRetention {
		p.BackupRetentionDays = cur.BackupRetentionDays
	}
	if p.Name == "" {
		p.Name = cur.Name
	}
	if p.Desc == "" {
		p.Desc = cur.Desc
	}
	// A bare path means the frontend echoed the logo URL back — keep current.
	if strings.HasPrefix(p.IconURL, "/") {
		p.IconURL = ""
	}
	if p.IconURL == "" {
		p.IconURL = cur.IconURL
	}
	if p.Stack == "" {
		p.Stack = cur.Stack
	}
	if p.Port == "" {
		p.Port = cur.Port
	}
	if p.DB == "" {
		p.DB = cur.DB
	}
	if p.DSN == "" {
		p.DSN = cur.DSN
	}
	if p.BasePath == "" {
		p.BasePath = cur.BasePath
	}
	if p.ServiceName == "" {
		p.ServiceName = cur.ServiceName
	}
	if p.BackendURL == "" {
		p.BackendURL = cur.BackendURL
	}
	if p.StartupCommand == "" {
		p.StartupCommand = cur.StartupCommand
	}
	if p.HealthURL == "" {
		p.HealthURL = cur.HealthURL
	}
	if p.OfflineMsg == "" {
		p.OfflineMsg = cur.OfflineMsg
	}
	if p.Tags == nil {
		p.Tags = cur.Tags
	}
	tagsJSON, _ := json.Marshal(p.Tags)
	// Datasources: super_admin may replace the list; primary source then
	// mirrors dsn/db_type. Absent from payload → keep current.
	dsJSON := "[]"
	if curDS.Valid {
		dsJSON = curDS.String
	}
	if hasDS && r.Header.Get("X-Role") == "super_admin" {
		dss := normalizeDatasources(parseDatasources(p.Datasources))
		if prim := primaryDatasource(dss); prim != nil {
			if s, ok := prim["dsn"].(string); ok {
				p.DSN = s
			}
			if t, ok := prim["type"].(string); ok {
				p.DB = t
			}
		}
		if b, err := json.Marshal(dss); err == nil {
			dsJSON = string(b)
		}
	}
	// Shared services: super_admin only, dedupe by name.
	svcJSON := "[]"
	if curSvc.Valid {
		svcJSON = curSvc.String
	}
	if hasSvc && r.Header.Get("X-Role") == "super_admin" {
		seen := map[string]bool{}
		uniq := []map[string]interface{}{}
		for _, s := range parseDatasources(p.Services) {
			if name, ok := s["name"].(string); ok && name != "" && !seen[name] {
				seen[name] = true
				uniq = append(uniq, s)
			}
		}
		if b, err := json.Marshal(uniq); err == nil {
			svcJSON = string(b)
		}
	}
	iconThumb := ""
	if strings.HasPrefix(p.IconURL, "data:") && p.IconURL != cur.IconURL {
		if t, err := makeThumb(p.IconURL, 128); err == nil && t != p.IconURL {
			iconThumb = t
		}
	}
	_, err := db.DB.Exec("UPDATE projects SET name=$1, description=$2, icon_url=$3, icon_thumb=COALESCE(NULLIF($4,''), icon_thumb), stack=$5, port=$6, db_type=$7, dsn=$8, backend_url=$9, service_name=$10, base_path=$11, tags=$12::jsonb, offline_msg=$13, startup_command=$14, health_url=$15, backup_interval_hours=$16, backup_retention_days=$17, datasources=$18::jsonb, services=$19::jsonb WHERE id=$20",
		p.Name, p.Desc, p.IconURL, iconThumb, p.Stack, p.Port, p.DB, p.DSN, p.BackendURL, p.ServiceName, p.BasePath, string(tagsJSON), p.OfflineMsg, p.StartupCommand, p.HealthURL, p.BackupIntervalHours, p.BackupRetentionDays, dsJSON, svcJSON, id)
	if err != nil {
		log.Printf("UpdateProject %s: %v", id, err)
		auth.JSONErr(w, 500, "更新项目失败")
		return
	}
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"updated": id})
}

func DeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	// Detach/stop BEFORE deleting the row — DetachServices reads the
	// project's services from the DB; after deletion they would be gone
	// and the refcount would never decrement.
	runtime.ProcMgr.Stop(id)
	runtime.TCPProxyMgr.Stop(id)
	runtime.ProcMgr.DetachServices(id)
	runtime.PortMgr.Free(id)
	res, err := db.DB.Exec("DELETE FROM projects WHERE id=$1", id)
	if err != nil {
		log.Printf("DeleteProject %s: %v", id, err)
		auth.JSONErr(w, 500, "删除项目失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		auth.JSONErr(w, 404, "项目不存在")
		return
	}
	auditLog(r, "删除项目", id, "项目及运行配置已删除")
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"deleted": id})
}

func PatchProjectStatus(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var req struct {
		Status     string `json:"status"`
		OfflineMsg string `json:"offline_msg"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	var current string
	if err := db.DB.QueryRow("SELECT status FROM projects WHERE id=$1", id).Scan(&current); err != nil {
		auth.JSONErr(w, 404, "项目不存在")
		return
	}
	next := "offline"
	if current == "offline" {
		next = "maintenance"
	}
	if current == "maintenance" {
		next = "online"
	}
	if req.Status != "" {
		next = req.Status
	}
	// Whitelist gate: arbitrary values would poison the three-state machine
	// (status drives nginx gate, health monitor and process lifecycle).
	if next != "online" && next != "offline" && next != "maintenance" {
		auth.JSONErr(w, 400, "状态只能是 online/offline/maintenance")
		return
	}
	// 同态守卫:状态未变 → 只落 offline_msg(如有),不发通知、不启停代理 —
	// 同态 PATCH 也会重启一遍代理+刷一条通知是越权矩阵 pin 的噪音行为。
	if next == current {
		if req.OfflineMsg != "" {
			if _, err := db.DB.Exec("UPDATE projects SET offline_msg=$1 WHERE id=$2", req.OfflineMsg, id); err != nil {
				log.Printf("PatchProjectStatus offline_msg: %v", err)
				auth.JSONErr(w, 500, "状态更新失败")
				return
			}
		}
		auth.JSONOK(w, map[string]string{"status": next})
		return
	}
	if _, err := db.DB.Exec("UPDATE projects SET status=$1, updated_at=NOW() WHERE id=$2", next, id); err != nil {
		log.Printf("PatchProjectStatus update: %v", err)
		auth.JSONErr(w, 500, "状态更新失败")
		return
	}
	var pname string
	db.DB.QueryRow("SELECT name FROM projects WHERE id=$1", id).Scan(&pname)

	// Unified process lifecycle: shared services first (referenced by the
	// project), then the project's own process. Reverse order on stop.
	if next == "online" {
		go runtime.ProcMgr.AttachServices(id)
		go runtime.TCPProxyMgr.Start(id)
		go runtime.ProcMgr.Start(id)
	} else {
		runtime.ProcMgr.Stop(id)
		runtime.TCPProxyMgr.Stop(id)
		runtime.ProcMgr.DetachServices(id)
	}

	// 动作词（用户分区：状态名词用在线/离线/维护中，动作通知保留上线/停用）。
	statusLabel := map[string]string{"online": "已上线", "offline": "已停用", "maintenance": "维护中"}[next]
	nid := fmt.Sprintf("n%d", time.Now().UnixNano())
	db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
		nid, id, "status", "项目状态变更", fmt.Sprintf("「%s」%s", pname, statusLabel))
	go notify.NotifyAdmin("Lambs项目状态变更",
		fmt.Sprintf("项目「%s」%s\n时间: %s", pname, statusLabel, time.Now().Format("2006-01-02 15:04:05")))
	go nginx.Sync()
	auth.JSONOK(w, map[string]string{"status": next})
}

func PinProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
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
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var dsn, healthURL string
	db.DB.QueryRow("SELECT COALESCE(dsn,''), COALESCE(health_url,'') FROM projects WHERE id=$1", id).Scan(&dsn, &healthURL)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	if err := db.CheckDSNHost(dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	if healthURL != "" {
		if err := db.CheckDSNHost(healthURL); err != nil {
			auth.JSONErr(w, 400, err.Error())
			return
		}
	}
	auth.JSONOK(w, db.TestHealth(dsn, healthURL))
}

func SyncProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	users := db.SyncUserData(dsn)
	userCount := len(users)
	db.DB.Exec("UPDATE projects SET users_count=$1, updated_at=NOW() WHERE id=$2", userCount, id)
	var fRaw sql.NullString
	db.DB.QueryRow("SELECT features::text FROM projects WHERE id=$1", id).Scan(&fRaw)
	if fRaw.Valid && fRaw.String != "" {
		var feats []map[string]interface{}
		if json.Unmarshal([]byte(fRaw.String), &feats) == nil {
			updated := false
			for i, f := range feats {
				if l, ok := f["label"]; ok && l == "用户数" {
					feats[i]["value"] = userCount
					updated = true
					break
				}
			}
			if !updated {
				feats = append(feats, map[string]interface{}{"label": "用户数", "value": userCount})
			}
			b, _ := json.Marshal(feats)
			db.DB.Exec("UPDATE projects SET features=$1 WHERE id=$2", string(b), id)
		}
	}
	auth.JSONOK(w, map[string]interface{}{"users": users, "count": userCount})
}

func ProjectStats(w http.ResponseWriter, r *http.Request) {
	userRole := r.Header.Get("X-Role")
	userID := r.Header.Get("X-User-ID")
	where := ""
	args := []interface{}{}
	if userRole != "super_admin" {
		// Non-admins see stats scoped to their own projects — counts of the
		// whole fleet are an information leak for a viewer with no access.
		var accessStr string
		db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&accessStr)
		var accessIDs []string
		json.Unmarshal([]byte(accessStr), &accessIDs)
		if len(accessIDs) == 0 {
			auth.JSONOK(w, map[string]int{"total_projects": 0, "online": 0, "offline": 0, "maintenance": 0, "total_users": 0})
			return
		}
		placeholders := make([]string, len(accessIDs))
		for i, pid := range accessIDs {
			placeholders[i] = "$" + strconv.Itoa(i+1)
			args = append(args, pid)
		}
		where = " WHERE id IN (" + strings.Join(placeholders, ",") + ")"
	}
	count := func(q string, a ...interface{}) int {
		var n int
		db.DB.QueryRow(q, a...).Scan(&n)
		return n
	}
	total := count("SELECT COUNT(*) FROM projects"+where, args...)
	online := count("SELECT COUNT(*) FROM projects WHERE status='online'"+strings.Replace(where, "WHERE", "AND", 1), args...)
	offline := count("SELECT COUNT(*) FROM projects WHERE status='offline'"+strings.Replace(where, "WHERE", "AND", 1), args...)
	maintenance := count("SELECT COUNT(*) FROM projects WHERE status='maintenance'"+strings.Replace(where, "WHERE", "AND", 1), args...)
	// 卡片语义:"累计注册用户 = 覆盖所有项目"——Lambs 平台用户 + 被管项目
	// 同步来的最终用户数之和。两者人群不同,不是重复计数。
	var lambsUsers, projectUsers int
	db.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&lambsUsers)
	db.DB.QueryRow("SELECT COALESCE(SUM(users_count), 0) FROM projects"+where, args...).Scan(&projectUsers)
	auth.JSONOK(w, map[string]int{"total_projects": total, "online": online, "offline": offline, "maintenance": maintenance, "total_users": lambsUsers + projectUsers, "project_users": projectUsers})
}

// VectorSearch runs a similarity search on a Qdrant datasource.
// Body: {ds?, collection, vector: [...], top_k}
func VectorSearch(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectView(r, id) {
		auth.JSONErr(w, 403, "无权限访问该项目")
		return
	}
	var req struct {
		DS         string    `json:"ds"`
		Collection string    `json:"collection"`
		Vector     []float64 `json:"vector"`
		TopK       int       `json:"top_k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auth.JSONErr(w, 400, "无效请求")
		return
	}
	if req.Collection == "" || len(req.Vector) == 0 {
		auth.JSONErr(w, 400, "缺少 collection 或 vector")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, req.DS, dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	vs, ok := src.(*db.VectorSource)
	if !ok {
		auth.JSONErr(w, 400, "该数据源不支持向量检索")
		return
	}
	hits, err := vs.Search(req.Collection, req.Vector, req.TopK)
	if err != nil {
		log.Printf("VectorSearch %s: %v", req.Collection, err)
		auth.JSONErr(w, 500, "向量检索失败")
		return
	}
	auth.JSONOK(w, map[string]interface{}{"hits": hits, "count": len(hits)})
}

func ProjectTables(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectView(r, id) {
		auth.JSONErr(w, 403, "无权限访问该项目")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	table := r.URL.Query().Get("table")
	if err := db.CheckDSNHost(dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	// Pagination: page/page_size query params. Absent = capped read (500 rows).
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	limit, offset := 0, 0
	if page > 0 && pageSize > 0 {
		if pageSize > 500 {
			pageSize = 500
		}
		if page > 1000000 {
			page = 1000000
		}
		limit, offset = pageSize, (page-1)*pageSize
	}
	search := r.URL.Query().Get("search")
	sortCol := r.URL.Query().Get("sort_col")
	var total int
	// With search/sort active, sweep the full table in capped chunks, then
	// filter+sort in memory — consistent across every data source type and
	// no silent row loss beyond the 500-row window (R3-6).
	var data []map[string]interface{}
	var cols []string
	var pk string
	if search != "" || sortCol != "" {
		data, cols, pk, err = readAllItems(src, table)
	} else {
		data, cols, pk, err = src.ReadItems(table, limit, offset)
	}
	if err != nil {
		log.Printf("ProjectTables read: %v", err)
		auth.JSONErr(w, 500, "读取数据失败")
		return
	}
	// Single-point redaction: never expose password/token values through the
	// data browser, regardless of data source (REST/Mongo/Redis rows were
	// previously returned unfiltered).
	data = db.RedactSensitive(data)
	cols = db.RedactSensitiveCols(cols)
	if search != "" {
		q := strings.ToLower(strings.TrimSpace(search))
		filtered := make([]map[string]interface{}, 0, len(data))
		for _, row := range data {
			match := false
			for _, c := range cols {
				if strings.Contains(strings.ToLower(fmt.Sprintf("%v", row[c])), q) {
					match = true
					break
				}
			}
			if match {
				filtered = append(filtered, row)
			}
		}
		data = filtered
	}
	if sortCol != "" {
		for _, c := range cols {
			if c == sortCol {
				sortRows(data, sortCol, strings.ToLower(r.URL.Query().Get("sort_dir")))
				break
			}
		}
	}
	if search != "" || sortCol != "" {
		// Re-paginate the in-memory result.
		total = len(data)
		if page > 0 && pageSize > 0 {
			start := (page - 1) * pageSize
			if start > len(data) {
				start = len(data)
			}
			end := start + pageSize
			if end > len(data) {
				end = len(data)
			}
			data = data[start:end]
		}
	}
	if r.URL.Query().Get("format") == "csv" {
		exportCSV(w, id+"_"+table, cols, data)
		return
	}
	actualTable := table
	if actualTable == "" && len(cols) > 0 {
		actualTable = "users"
	}
	if search == "" && sortCol == "" {
		var cerr error
		total, cerr = src.CountItems(table)
		if cerr != nil {
			// Sources without a count endpoint: fall back to the rows we
			// have, so the frontend can still page instead of total=0.
			total = len(data)
		}
	}
	auth.JSONOK(w, map[string]interface{}{"columns": cols, "rows": data, "table": actualTable, "pk": pk, "total": total, "page": page, "page_size": pageSize})
}

// ListTableNames returns all user tables in the project's database.
func ListTableNames(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectView(r, id) {
		auth.JSONErr(w, 403, "无权限访问该项目")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	tables, err := src.ListCollections()
	if err != nil {
		log.Printf("ListTableNames: %v", err)
		auth.JSONErr(w, 500, "读取表列表失败")
		return
	}
	if tables == nil {
		tables = []string{}
	}
	auth.JSONOK(w, map[string]interface{}{"tables": tables})
}

// refreshTabsSnapshot re-syncs one table's snapshot in the project's tabs jsonb.
// dsn must be the datasource the row was written to — not necessarily the primary.
func refreshTabsSnapshot(projectID, table, dsn string) {
	var tabsRaw string
	db.DB.QueryRow("SELECT COALESCE(tabs::text,'[]') FROM projects WHERE id=$1", projectID).Scan(&tabsRaw)
	src, err := db.NewDataSource(dsn)
	if err != nil {
		return
	}
	rows, cols, pk, err := src.ReadItems(table, 0, 0)
	if err != nil {
		return
	}
	// Same redaction as the data browser: tabs are served back to viewers.
	rows = db.RedactSensitive(rows)
	cols = db.RedactSensitiveCols(cols)
	var tabRows [][]interface{}
	for _, r := range rows {
		arr := make([]interface{}, len(cols))
		for i, c := range cols {
			arr[i] = r[c]
		}
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
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	table := r.URL.Query().Get("table")
	pkCol := r.URL.Query().Get("pk")
	pkVal := r.URL.Query().Get("pkval")
	if table == "" || pkCol == "" {
		auth.JSONErr(w, 400, "缺少table/pk参数")
		return
	}
	// pkCol is spliced into the WHERE clause by the adapters — same charset
	// gate as row keys, otherwise "id\" OR 1=1 --" rewrites every row.
	if !safeColName.MatchString(pkCol) {
		auth.JSONErr(w, 400, "非法主键列名")
		return
	}
	var row map[string]interface{}
	json.NewDecoder(r.Body).Decode(&row)
	if err := validateRowCols(row); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	if pkVal == "" {
		// Empty pkval = insert new row (sources like Qdrant generate their own IDs)
		if err := src.InsertItem(table, row); err != nil {
			log.Printf("InsertTableRow: %v", err)
			auth.JSONErr(w, 500, "新增数据失败")
			return
		}
		auditLog(r, "新增数据", id, fmt.Sprintf("table=%s", table))
	} else if err := src.UpdateItem(table, pkCol, pkVal, row); err != nil {
		log.Printf("UpdateTableRow: %v", err)
		auth.JSONErr(w, 500, "更新数据失败")
		return
	} else {
		auditLog(r, "修改数据", id, fmt.Sprintf("table=%s pk=%s=%s", table, pkCol, pkVal))
	}
	refreshTabsSnapshot(id, table, dsn)
	auth.JSONOK(w, map[string]string{"updated": pkVal})
}

// auditLog records an admin action. Best-effort — audit write failures must
// never break the operation itself.
func auditLog(r *http.Request, action, target, detail string) {
	db.DB.Exec("INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ($1,$2,$3,$4,$5)",
		r.Header.Get("X-User-ID"), r.Header.Get("X-Username"), action, target, detail)
}

func DeleteTableRow(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	table := r.URL.Query().Get("table")
	pkCol := r.URL.Query().Get("pk")
	pkVal := r.URL.Query().Get("pkval")
	if table == "" || pkCol == "" || pkVal == "" {
		auth.JSONErr(w, 400, "缺少table/pk/pkval参数")
		return
	}
	if !safeColName.MatchString(pkCol) {
		auth.JSONErr(w, 400, "非法主键列名")
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	if err := src.DeleteItem(table, pkCol, pkVal); err != nil {
		log.Printf("DeleteTableRow: %v", err)
		auth.JSONErr(w, 500, "删除数据失败")
		return
	}
	auditLog(r, "删除数据", id, fmt.Sprintf("table=%s pk=%s=%s", table, pkCol, pkVal))
	refreshTabsSnapshot(id, table, dsn)
	auth.JSONOK(w, map[string]string{"deleted": pkVal})
}

func InsertTableRow(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var dsn string
	db.DB.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	var err error
	if dsn, err = resolveDatasource(id, r.URL.Query().Get("ds"), dsn); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	table := r.URL.Query().Get("table")
	if table == "" {
		auth.JSONErr(w, 400, "缺少table参数")
		return
	}
	var row map[string]interface{}
	json.NewDecoder(r.Body).Decode(&row)
	if err := validateRowCols(row); err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	src, err := db.NewDataSource(dsn)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	if err := src.InsertItem(table, row); err != nil {
		log.Printf("InsertTableRow: %v", err)
		auth.JSONErr(w, 500, "新增数据失败")
		return
	}
	auditLog(r, "新增数据", id, fmt.Sprintf("table=%s", table))
	refreshTabsSnapshot(id, table, dsn)
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
	if !CheckProjectView(r, id) {
		auth.JSONErr(w, 403, "无权限访问该项目")
		return
	}
	rows, err := db.DB.Query("SELECT u.id, u.username, u.name, u.email, u.role, u.status, COALESCE(u.avatar_url::text,'') FROM users u WHERE u.project_access @> to_jsonb($1::text) OR u.role='super_admin'", id)
	if err != nil {
		auth.JSONOK(w, map[string]interface{}{"members": []models.User{}, "non_members": []models.User{}})
		return
	}
	defer rows.Close()
	members := []models.User{}
	for rows.Next() {
		var u models.User
		var av string
		rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status, &av)
		u.AvatarURL = av
		members = append(members, u)
	}
	if members == nil {
		members = []models.User{}
	}
	nRows, _ := db.DB.Query("SELECT id, username, name, email, role, status, COALESCE(avatar_url::text,'') FROM users WHERE role != 'super_admin' AND (project_access IS NULL OR project_access::text NOT LIKE '%' || $1 || '%')", id)
	nonMembers := []models.User{}
	if nRows != nil {
		defer nRows.Close()
		for nRows.Next() {
			var u models.User
			var av string
			nRows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status, &av)
			u.AvatarURL = av
			nonMembers = append(nonMembers, u)
		}
	}
	if nonMembers == nil {
		nonMembers = []models.User{}
	}
	auth.JSONOK(w, map[string]interface{}{"members": members, "non_members": nonMembers})
}

func AddMember(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var req struct {
		UserID string `json:"user_id"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	var access string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", req.UserID).Scan(&access)
	// Exact array membership — substring matching makes "app" collide with
	// "app2" and silently skips the grant.
	var arr []string
	json.Unmarshal([]byte(access), &arr)
	already := false
	for _, pid := range arr {
		if pid == id {
			already = true
			break
		}
	}
	if !already {
		arr = append(arr, id)
		newAccess, _ := json.Marshal(arr)
		db.DB.Exec("UPDATE users SET project_access=$1 WHERE id=$2", string(newAccess), req.UserID)
	}
	auth.JSONOK(w, map[string]string{"added": req.UserID})
}

func RemoveMember(w http.ResponseWriter, r *http.Request, id, userID string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var access string
	db.DB.QueryRow("SELECT COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).Scan(&access)
	var arr []string
	json.Unmarshal([]byte(access), &arr)
	var newArr []string
	for _, pid := range arr {
		if pid != id {
			newArr = append(newArr, pid)
		}
	}
	newAccess, _ := json.Marshal(newArr)
	db.DB.Exec("UPDATE users SET project_access=$1 WHERE id=$2", string(newAccess), userID)
	auth.JSONOK(w, map[string]string{"removed": userID})
}

func RefreshAll(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query("SELECT id, dsn FROM projects WHERE dsn IS NOT NULL AND dsn != '' AND dsn != '—'")
	if err != nil {
		auth.JSONOK(w, map[string]int{"refreshed": 0})
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, dsn string
		rows.Scan(&id, &dsn)
		users := db.SyncUserData(dsn)
		db.DB.Exec("UPDATE projects SET users_count=$1 WHERE id=$2", len(users), id)
		count++
	}
	auth.JSONOK(w, map[string]int{"refreshed": count})
}

func CloneProject(w http.ResponseWriter, r *http.Request, id string) {
	if !CheckProjectAccess(r, id) {
		auth.JSONErr(w, 403, "需要项目管理员权限")
		return
	}
	var orig models.Project
	err := db.DB.QueryRow("SELECT name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, COALESCE(tags::text,'[]'), COALESCE(offline_msg,''), COALESCE(features::text,'[]'), COALESCE(tabs::text,'[]') FROM projects WHERE id=$1", id).
		Scan(&orig.Name, &orig.Repo, &orig.Desc, &orig.IconURL, &orig.Stack, &orig.Port, &orig.DB, &orig.DSN, &orig.UserCount, &orig.Status, &orig.Order, &orig.Pinned, &orig.IconCls, &orig.BasePath, &orig.BackendURL, &orig.ServiceName, &orig.Tags, &orig.OfflineMsg, &orig.Features, &orig.Tabs)
	if err != nil {
		auth.JSONErr(w, 404, "项目不存在")
		return
	}
	newID := orig.Repo + "-clone"
	orig.ID = newID
	orig.Name = orig.Name + " (副本)"
	orig.Status = "offline"
	featJSON, _ := json.Marshal(orig.Features)
	tabsJSON, _ := json.Marshal(orig.Tabs)
	tagsJSON, _ := json.Marshal(orig.Tags)
	if string(featJSON) == "null" {
		featJSON = []byte("[]")
	}
	if string(tabsJSON) == "null" {
		tabsJSON = []byte("[]")
	}
	if string(tagsJSON) == "null" {
		tagsJSON = []byte("[]")
	}
	// Clone's datasources = single primary source mirroring the legacy dsn.
	cloneDS := []map[string]interface{}{}
	if orig.DSN != "" && orig.DSN != "—" {
		cloneDS = []map[string]interface{}{{"id": "ds1", "name": "主数据源", "type": orig.DB, "dsn": orig.DSN, "is_primary": true}}
	}
	dsJSON, _ := json.Marshal(cloneDS)
	// Clone copies metadata + datasource only. Process/routing fields (port,
	// service_name, startup_command, health_url, base_path, backend_url) are
	// intentionally NOT copied — a clone reusing them would clash with the
	// source project when enabled.
	_, err = db.DB.Exec("INSERT INTO projects (id, name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, tags, offline_msg, features, tabs, datasources) VALUES ($1,$2,$3,$4,$5,$6,'',$7,$8,$9,$10,$11,$12,$13,$14::jsonb,$15,$16::jsonb,$17::jsonb,$18::jsonb)",
		orig.ID, orig.Name, orig.Repo, orig.Desc, orig.IconURL, orig.Stack, orig.DB, orig.DSN, 0, orig.Status, orig.Order, orig.Pinned, orig.IconCls, string(tagsJSON), orig.OfflineMsg, string(featJSON), string(tabsJSON), string(dsJSON))
	if err != nil {
		log.Printf("DuplicateProject insert: %v", err)
		auth.JSONErr(w, 400, "创建副本失败")
		return
	}
	auth.JSONCreated(w, orig)
}
