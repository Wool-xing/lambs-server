package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	_ "github.com/lib/pq"
	"golang.org/x/crypto/bcrypt"
)

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ============================================================
// Config
// ============================================================

var (
	// lambsConfig holds server config (loaded on startup)
	lambsConfig lambsConfigData
)

type lambsConfigData struct {
	JWTSecret    string `json:"jwt_secret"`
	AdminEmail   string `json:"admin_email"`
	Port         int    `json:"port"`
	RefreshInt   int    `json:"refresh_interval"`
	SMTPHost     string `json:"smtp_host"`
	SMTPPort     string `json:"smtp_port"`
	SMTPUser     string `json:"smtp_user"`
	SMTPPassword string `json:"smtp_password"`
	SMTPFrom     string `json:"smtp_from"`
}

// ============================================================
// Models
// ============================================================

type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Repo        string `json:"repo"`
	Desc        string `json:"description"`
	IconURL     string `json:"icon_url"`
	Stack       string `json:"stack"`
	Port        string `json:"port"`
	DB          string `json:"db"`
	DSN         string `json:"dsn"`
	UserCount   int    `json:"users"`
	Status      string `json:"status"`
	Order       int    `json:"sort_order"`
	Pinned      bool   `json:"pinned"`
	IconCls     string `json:"icon_cls"`
	BasePath    string `json:"base_path"`
	BackendURL  string `json:"backend_url"`
	ServiceName string `json:"service_name"`
	Tags        string `json:"tags"`
	OfflineMsg  string `json:"offline_msg"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

type User struct {
	ID            string `json:"id"`
	Username      string `json:"username"`
	Name          string `json:"name"`
	Email         string `json:"email"`
	PasswordHash  string `json:"-"`
	Role          string `json:"role"`
	Status        string `json:"status"`
	ProjectAccess string `json:"project_access"`
	LastLogin     string `json:"last_login"`
}

type Notification struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Read      bool   `json:"read"`
	CreatedAt string `json:"created_at"`
}

type AuditLog struct {
	ID        int    `json:"id"`
	UserID    string `json:"user_id"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

type ApiResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data,omitempty"`
	Error   string      `json:"error,omitempty"`
	Message string      `json:"message,omitempty"`
}

// ============================================================
// Database
// ============================================================

var db *sql.DB

func initDB() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL not set")
	}
	// Convert asyncpg URL to pq format
	dsn = strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("DB open: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err = db.Ping(); err != nil {
		log.Fatalf("DB ping: %v", err)
	}
	log.Println("DB connected")
}

// ============================================================
// JSON helpers
// ============================================================

func jsonOK(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(ApiResponse{Success: true, Data: data})
}

func jsonOKCreated(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(ApiResponse{Success: true, Data: data})
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(ApiResponse{Success: false, Error: msg})
}

// ============================================================
// JWT Auth Middleware
// ============================================================

var jwtKey []byte

type Claims struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			jsonErr(w, 401, "未登录")
			return
		}
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			return jwtKey, nil
		})
		if err != nil || !token.Valid {
			jsonErr(w, 401, "token失效")
			return
		}
		r.Header.Set("X-User-ID", claims.UserID)
		r.Header.Set("X-Username", claims.Username)
		r.Header.Set("X-Role", claims.Role)
		next(w, r)
	}
}

func requireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Role") != "super_admin" {
			jsonErr(w, 403, "需要超管权限")
			return
		}
		next(w, r)
	})
}

func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "https://wool.cc.cd")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		if r.Method == "OPTIONS" {
			w.WriteHeader(200)
			return
		}
		next(w, r)
	}
}

// ============================================================
// Auth Handlers
// ============================================================

func handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, 400, "无效请求")
		return
	}
	var user User
	err := db.QueryRow("SELECT id, username, name, email, password_hash, role, status FROM users WHERE username=$1",
		req.Username).Scan(&user.ID, &user.Username, &user.Name, &user.Email, &user.PasswordHash, &user.Role, &user.Status)
	if err != nil {
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	if user.Status != "active" {
		jsonErr(w, 403, "账号已停用")
		return
	}
	// Frontend sends SHA256(password); stored hash is bcrypt(SHA256(password))
	// Frontend already did SHA256 — just bcrypt.verify directly
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)) != nil {
		jsonErr(w, 401, "用户名或密码错误")
		return
	}
	db.Exec("UPDATE users SET last_login=$1 WHERE id=$2", time.Now().Format(time.RFC3339), user.ID)

	claims := &Claims{
		UserID:   user.ID,
		Username: user.Username,
		Role:     user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(8 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(jwtKey)
	if err != nil {
		jsonErr(w, 500, "token生成失败")
		return
	}
	jsonOK(w, map[string]interface{}{
		"access_token": tokenStr,
		"token_type":   "bearer",
		"user_id":      user.ID,
		"username":     user.Username,
		"name":         user.Name,
		"email":        user.Email,
		"role":         user.Role,
	})
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	var user User
	err := db.QueryRow("SELECT id, username, name, email, role, status FROM users WHERE id=$1", userID).
		Scan(&user.ID, &user.Username, &user.Name, &user.Email, &user.Role, &user.Status)
	if err != nil {
		jsonErr(w, 404, "用户不存在")
		return
	}
	jsonOK(w, user)
}

// ============================================================
// Project Handlers
// ============================================================

func handleListProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, name, repo, description, icon_url, stack, port, db_type, dsn, COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), tags::text, COALESCE(offline_msg,''), COALESCE(created_at::text,''), COALESCE(updated_at::text,'') FROM projects ORDER BY is_pinned DESC, sort_order")
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		rows.Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.Tags, &p.OfflineMsg, &p.CreatedAt, &p.UpdatedAt)
		if p.DSN == "" {
			p.DSN = "—"
		}
		projects = append(projects, p)
	}
	if projects == nil {
		projects = []Project{}
	}
	jsonOK(w, projects)
}

func handleGetProject(w http.ResponseWriter, r *http.Request, id string) {
	var p Project
	err := db.QueryRow("SELECT id, name, repo, description, icon_url, stack, port, db_type, dsn, COALESCE(users_count,0), status, sort_order, COALESCE(is_pinned,false), COALESCE(icon_cls,''), COALESCE(base_path,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(tags,''), COALESCE(offline_msg,'') FROM projects WHERE id=$1", id).
		Scan(&p.ID, &p.Name, &p.Repo, &p.Desc, &p.IconURL, &p.Stack, &p.Port, &p.DB, &p.DSN, &p.UserCount, &p.Status, &p.Order, &p.Pinned, &p.IconCls, &p.BasePath, &p.BackendURL, &p.ServiceName, &p.Tags, &p.OfflineMsg)
	if err != nil {
		jsonErr(w, 404, "项目不存在")
		return
	}
	if p.DSN == "" {
		p.DSN = "—"
	}
	jsonOK(w, p)
}

func handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var p Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		jsonErr(w, 400, "无效数据")
		return
	}
	if p.Status == "" {
		p.Status = "online"
	}
	if p.Order == 0 {
		p.Order = 999
	}
	err := db.QueryRow("INSERT INTO projects (id, name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, tags, offline_msg) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19) RETURNING id",
		p.ID, p.Name, p.Repo, p.Desc, p.IconURL, p.Stack, p.Port, p.DB, p.DSN, p.UserCount, p.Status, p.Order, p.Pinned, p.IconCls, p.BasePath, p.BackendURL, p.ServiceName, p.Tags, p.OfflineMsg).Scan(&p.ID)
	if err != nil {
		jsonErr(w, 400, "创建失败: "+err.Error())
		return
	}
	// Sync nginx
	go syncNginx()
	jsonOKCreated(w, p)
}

func handleUpdateProject(w http.ResponseWriter, r *http.Request, id string) {
	var p Project
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		jsonErr(w, 400, "无效数据")
		return
	}
	_, err := db.Exec("UPDATE projects SET name=$1, description=$2, icon_url=$3, stack=$4, port=$5, db=$6, dsn=$7, backend_url=$8, service_name=$9, base_path=$10, tags=$11, offline_msg=$12 WHERE id=$13",
		p.Name, p.Desc, p.IconURL, p.Stack, p.Port, p.DB, p.DSN, p.BackendURL, p.ServiceName, p.BasePath, p.Tags, p.OfflineMsg, id)
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	go syncNginx()
	jsonOK(w, map[string]string{"updated": id})
}

func handleDeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	db.Exec("DELETE FROM projects WHERE id=$1", id)
	go syncNginx()
	jsonOK(w, map[string]string{"deleted": id})
}

func handlePatchProjectStatus(w http.ResponseWriter, r *http.Request, id string) {
	var req struct{ Status string `json:"status"` }
	json.NewDecoder(r.Body).Decode(&req)
	var current string
	db.QueryRow("SELECT status FROM projects WHERE id=$1", id).Scan(&current)
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
	db.Exec("UPDATE projects SET status=$1, updated_at=NOW() WHERE id=$2", next, id)
	go syncNginx()
	jsonOK(w, map[string]string{"status": next})
}

func handlePinProject(w http.ResponseWriter, r *http.Request, id string) {
	var pinned bool
	db.QueryRow("SELECT pinned FROM projects WHERE id=$1", id).Scan(&pinned)
	db.Exec("UPDATE projects SET is_pinned=$1 WHERE id=$2", !pinned, id)
	jsonOK(w, map[string]bool{"pinned": !pinned})
}

func handleReorderProjects(w http.ResponseWriter, r *http.Request) {
	var order []struct {
		ID    string `json:"id"`
		Order int    `json:"sort_order"`
	}
	json.NewDecoder(r.Body).Decode(&order)
	for _, o := range order {
		db.Exec("UPDATE projects SET sort_order=$1 WHERE id=$2", o.Order, o.ID)
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

func handleTestConnection(w http.ResponseWriter, r *http.Request, id string) {
	var dsn string
	db.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	result := testDSN(dsn)
	jsonOK(w, result)
}

func handleSyncProject(w http.ResponseWriter, r *http.Request, id string) {
	var dsn string
	db.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	users := syncUserData(dsn)
	db.Exec("UPDATE projects SET users_count=$1, updated_at=NOW() WHERE id=$2", len(users), id)
	jsonOK(w, map[string]interface{}{"users": users, "count": len(users)})
}

func handleProjectStats(w http.ResponseWriter, r *http.Request) {
	var total, online, offline, maintenance int
	db.QueryRow("SELECT COUNT(*) FROM projects").Scan(&total)
	db.QueryRow("SELECT COUNT(*) FROM projects WHERE status='online'").Scan(&online)
	db.QueryRow("SELECT COUNT(*) FROM projects WHERE status='offline'").Scan(&offline)
	db.QueryRow("SELECT COUNT(*) FROM projects WHERE status='maintenance'").Scan(&maintenance)
	var userCount int
	db.QueryRow("SELECT COUNT(*) FROM users").Scan(&userCount)
	jsonOK(w, map[string]int{
		"total": total, "online": online, "offline": offline,
		"maintenance": maintenance, "users": userCount,
	})
}

func handleProjectLogs(w http.ResponseWriter, r *http.Request, id string) {
	jsonOK(w, []map[string]string{{"msg": "service running", "time": time.Now().Format(time.RFC3339)}})
}

func handleProjectTables(w http.ResponseWriter, r *http.Request, id string) {
	var dsn string
	db.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&dsn)
	data := syncUserData(dsn)
	if data == nil {
		data = []map[string]interface{}{}
	}
	jsonOK(w, data)
}

func handleProjectMembers(w http.ResponseWriter, r *http.Request, id string) {
	rows, err := db.Query("SELECT u.id, u.username, u.name, u.email, u.role, u.status FROM users u WHERE u.project_access LIKE '%' || $1 || '%' OR u.role='super_admin'", id)
	if err != nil {
		jsonOK(w, []User{})
		return
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status)
		users = append(users, u)
	}
	jsonOK(w, users)
}

func handleAddMember(w http.ResponseWriter, r *http.Request, id string) {
	var req struct{ UserID string `json:"user_id"` }
	json.NewDecoder(r.Body).Decode(&req)
	var access string
	db.QueryRow("SELECT COALESCE(project_access,'') FROM users WHERE id=$1", req.UserID).Scan(&access)
	if !strings.Contains(access, id) {
		access = access + "," + id
		access = strings.Trim(access, ",")
		db.Exec("UPDATE users SET project_access=$1 WHERE id=$2", access, req.UserID)
	}
	jsonOK(w, map[string]string{"added": req.UserID})
}

func handleRemoveMember(w http.ResponseWriter, r *http.Request, id, userID string) {
	var access string
	db.QueryRow("SELECT COALESCE(project_access,'') FROM users WHERE id=$1", userID).Scan(&access)
	access = strings.ReplaceAll(access, id, "")
	access = strings.ReplaceAll(access, ",,", ",")
	access = strings.Trim(access, ",")
	db.Exec("UPDATE users SET project_access=$1 WHERE id=$2", access, userID)
	jsonOK(w, map[string]string{"removed": userID})
}

func handleRefreshAll(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, dsn FROM projects WHERE dsn IS NOT NULL AND dsn != '' AND dsn != '—'")
	if err != nil {
		jsonOK(w, map[string]int{"refreshed": 0})
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id, dsn string
		rows.Scan(&id, &dsn)
		users := syncUserData(dsn)
		db.Exec("UPDATE projects SET users_count=$1 WHERE id=$2", len(users), id)
		count++
	}
	jsonOK(w, map[string]int{"refreshed": count})
}

// ============================================================
// User Handlers
// ============================================================

func handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, username, name, email, role, status, COALESCE(project_access,''), COALESCE(last_login::text,'') FROM users ORDER BY created_at DESC")
	if err != nil {
		jsonErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status, &u.ProjectAccess, &u.LastLogin)
		users = append(users, u)
	}
	if users == nil {
		users = []User{}
	}
	jsonOK(w, users)
}

func handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var u User
	json.NewDecoder(r.Body).Decode(&u)
	initPass := os.Getenv("INITIAL_PASSWORD")
	if initPass == "" {
		initPass = "admin123"
	}
	hash, _ := bcrypt.GenerateFromPassword([]byte(initPass), bcrypt.DefaultCost)
	u.ID = fmt.Sprintf("u%d", time.Now().UnixNano())
	_, err := db.Exec("INSERT INTO users (id, username, name, email, password_hash, role, status, project_access) VALUES ($1,$2,$3,$4,$5,$6,'active',$7)",
		u.ID, u.Username, u.Name, u.Email, string(hash), u.Role, u.ProjectAccess)
	if err != nil {
		jsonErr(w, 400, "创建失败: "+err.Error())
		return
	}
	jsonOKCreated(w, u)
}

func handleUpdateUser(w http.ResponseWriter, r *http.Request, id string) {
	var u User
	json.NewDecoder(r.Body).Decode(&u)
	db.Exec("UPDATE users SET name=$1, email=$2, role=$3, status=$4, project_access=$5 WHERE id=$6",
		u.Name, u.Email, u.Role, u.Status, u.ProjectAccess, id)
	jsonOK(w, map[string]string{"updated": id})
}

func handleDeleteUser(w http.ResponseWriter, r *http.Request, id string) {
	db.Exec("DELETE FROM users WHERE id=$1", id)
	jsonOK(w, map[string]string{"deleted": id})
}

func handleResetPassword(w http.ResponseWriter, r *http.Request, id string) {
	newPW := fmt.Sprintf("%08d", time.Now().UnixNano()%100000000)
	hash, _ := bcrypt.GenerateFromPassword([]byte(newPW), bcrypt.DefaultCost)
	db.Exec("UPDATE users SET password_hash=$1 WHERE id=$2", string(hash), id)
	jsonOK(w, map[string]string{"new_password": newPW})
}

// ============================================================
// Gate Handlers
// ============================================================

func handleGateCheck(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" || path == "/" {
		jsonOK(w, map[string]bool{"allowed": true})
		return
	}
	rows, err := db.Query("SELECT base_path, status, name FROM projects WHERE base_path IS NOT NULL AND base_path != ''")
	if err != nil {
		jsonOK(w, map[string]bool{"allowed": true})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var bp, status, name string
		rows.Scan(&bp, &status, &name)
		if path == bp || strings.HasPrefix(path, bp+"/") || strings.HasPrefix(path, bp+"?") {
			if status == "offline" {
				jsonErr(w, 403, "该项目已被管理员停用")
				return
			}
			if status == "maintenance" {
				jsonErr(w, 503, "该项目维护中")
				return
			}
		}
	}
	jsonOK(w, map[string]bool{"allowed": true})
}

func handleGateCheckInternal(w http.ResponseWriter, r *http.Request) {
	// Same as check but for nginx auth_request (no auth required)
	handleGateCheck(w, r)
}

func handleGateProjectLogo(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	var iconURL string
	db.QueryRow("SELECT COALESCE(icon_url,'') FROM projects WHERE base_path=$1", strings.TrimPrefix(path, "/")).Scan(&iconURL)
	if iconURL != "" {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write([]byte(iconURL))
		return
	}
	w.WriteHeader(404)
}

// ============================================================
// Settings Handlers
// ============================================================

func handleGetConfig(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, lambsConfig)
}

func handleUpdateConfig(w http.ResponseWriter, r *http.Request) {
	var cfg lambsConfigData
	json.NewDecoder(r.Body).Decode(&cfg)
	lambsConfig = cfg
	jsonOK(w, map[string]string{"saved": "ok"})
}

func handleExportProjects(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=projects.csv")
	w.Write([]byte{0xEF, 0xBB, 0xBF}) // BOM
	cw := csv.NewWriter(w)
	cw.Write([]string{"ID", "Name", "Status", "Database", "Users"})
	rows, _ := db.Query("SELECT id, name, status, db, users FROM projects")
	defer rows.Close()
	for rows.Next() {
		var id, name, status, dbType string
		var users int
		rows.Scan(&id, &name, &status, &dbType, &users)
		cw.Write([]string{id, name, status, dbType, strconv.Itoa(users)})
	}
	cw.Flush()
}

func handleExportUsers(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=users.csv")
	w.Write([]byte{0xEF, 0xBB, 0xBF})
	cw := csv.NewWriter(w)
	cw.Write([]string{"ID", "Username", "Name", "Email", "Role", "Status"})
	rows, _ := db.Query("SELECT id, username, name, email, role, status FROM users")
	defer rows.Close()
	for rows.Next() {
		var u User
		rows.Scan(&u.ID, &u.Username, &u.Name, &u.Email, &u.Role, &u.Status)
		cw.Write([]string{u.ID, u.Username, u.Name, u.Email, u.Role, u.Status})
	}
	cw.Flush()
}

func handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, user_id, action, target, detail, created_at::text FROM audit_logs ORDER BY id DESC LIMIT 50")
	if err != nil {
		jsonOK(w, []AuditLog{})
		return
	}
	defer rows.Close()
	logs := []AuditLog{}
	for rows.Next() {
		var l AuditLog
		rows.Scan(&l.ID, &l.UserID, &l.Action, &l.Target, &l.Detail, &l.CreatedAt)
		logs = append(logs, l)
	}
	jsonOK(w, logs)
}

// ============================================================
// Backup Handlers
// ============================================================

func handleCreateBackup(w http.ResponseWriter, r *http.Request, id string) {
	var p Project
	err := db.QueryRow("SELECT dsn FROM projects WHERE id=$1", id).Scan(&p.DSN)
	if err != nil || p.DSN == "" || p.DSN == "—" {
		jsonErr(w, 400, "未配置数据源")
		return
	}
	result := doBackup(id, p.DSN)
	jsonOK(w, result)
}

func handleListBackups(w http.ResponseWriter, r *http.Request, id string) {
	dir := "/home/ubuntu/lambs-backups"
	entries, _ := os.ReadDir(dir)
	files := []map[string]interface{}{}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), id) {
			info, _ := e.Info()
			files = append(files, map[string]interface{}{
				"filename": e.Name(),
				"size_mb":  float64(info.Size()) / (1024 * 1024),
				"created":  info.ModTime().Format("2006-01-02 15:04"),
			})
		}
	}
	jsonOK(w, map[string]interface{}{"backups": files})
}

func handleDownloadBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	fpath := fmt.Sprintf("/home/ubuntu/lambs-backups/%s", filename)
	if !strings.HasPrefix(filename, id) {
		jsonErr(w, 404, "备份不存在")
		return
	}
	http.ServeFile(w, r, fpath)
}

func handleDeleteBackup(w http.ResponseWriter, r *http.Request, id, filename string) {
	os.Remove(fmt.Sprintf("/home/ubuntu/lambs-backups/%s", filename))
	jsonOK(w, map[string]string{"deleted": filename})
}

func doBackup(projectID, dsn string) map[string]interface{} {
	ts := time.Now().Format("20060102-150405")
	fname := fmt.Sprintf("%s_%s", projectID, ts)
	dir := "/home/ubuntu/lambs-backups"
	os.MkdirAll(dir, 0755)

	if strings.Contains(dsn, "sqlite") {
		fpath := fmt.Sprintf("%s/%s.db", dir, fname)
		path := strings.Replace(strings.Split(dsn, "?")[0], "sqlite:///", "", 1)
		src, err := os.Open(path)
		if err != nil {
			return map[string]interface{}{"ok": false, "error": err.Error()}
		}
		defer src.Close()
		dst, _ := os.Create(fpath)
		defer dst.Close()
		io.Copy(dst, src)
		info, _ := dst.Stat()
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
			if strings.Contains(hostPart, ":") {
				hp := strings.Split(hostPart, ":")
				host, port = hp[0], hp[1]
			} else {
				host = hostPart
			}
		}
		if len(strings.Split(parts, "/")) > 1 {
			dbname = strings.Split(strings.Split(parts, "/")[len(strings.Split(parts, "/"))-1], "?")[0]
		}
		password := ""
		if strings.Contains(authPart, ":") {
			password = strings.Split(authPart, ":")[1]
		}
		cmd := exec.Command("pg_dump", "-h", host, "-p", port, "-U", user, "-d", dbname, "-f", fpath, "--no-owner", "--no-acl")
		cmd.Env = append(os.Environ(), "PGPASSWORD="+password)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return map[string]interface{}{"ok": false, "error": string(out) + err.Error()}
		}
		info, _ := os.Stat(fpath)
		return map[string]interface{}{"ok": true, "filename": fname + ".sql", "size_mb": float64(info.Size()) / (1024 * 1024)}
	}
	return map[string]interface{}{"ok": false, "error": "unsupported db type"}
}

// ============================================================
// System / Health
// ============================================================

func handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"status":  "ok",
		"service": "lambs-server-go",
		"time":    time.Now().Unix(),
	})
}

// ============================================================
// Notifications
// ============================================================

func handleListNotifications(w http.ResponseWriter, r *http.Request) {
	rows, _ := db.Query("SELECT id, COALESCE(project_id,''), type, title, content, COALESCE(read,false), COALESCE(created_at::text,'') FROM notifications ORDER BY created_at DESC LIMIT 50")
	if rows == nil {
		jsonOK(w, []Notification{})
		return
	}
	defer rows.Close()
	ns := []Notification{}
	for rows.Next() {
		var n Notification
		rows.Scan(&n.ID, &n.ProjectID, &n.Type, &n.Title, &n.Content, &n.Read, &n.CreatedAt)
		ns = append(ns, n)
	}
	if ns == nil {
		ns = []Notification{}
	}
	jsonOK(w, ns)
}

func handleReadNotification(w http.ResponseWriter, r *http.Request, nid string) {
	db.Exec("UPDATE notifications SET read=true WHERE id=$1", nid)
	jsonOK(w, map[string]string{"read": nid})
}

func handleReadAllNotifications(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	db.Exec("UPDATE notifications SET read=true WHERE project_id IN (SELECT id FROM projects) OR project_id IS NULL")
	_ = userID
	jsonOK(w, map[string]string{"status": "ok"})
}

func handleDeleteNotification(w http.ResponseWriter, r *http.Request, nid string) {
	db.Exec("DELETE FROM notifications WHERE id=$1", nid)
	jsonOK(w, map[string]string{"deleted": nid})
}

// ============================================================
// Clone Project
// ============================================================

func handleCloneProject(w http.ResponseWriter, r *http.Request, id string) {
	var orig Project
	err := db.QueryRow("SELECT name, repo, description, icon_url, stack, port, db, dsn, users, status, sort_order, pinned, icon_cls, base_path, backend_url, service_name, tags, offline_msg FROM projects WHERE id=$1", id).
		Scan(&orig.Name, &orig.Repo, &orig.Desc, &orig.IconURL, &orig.Stack, &orig.Port, &orig.DB, &orig.DSN, &orig.UserCount, &orig.Status, &orig.Order, &orig.Pinned, &orig.IconCls, &orig.BasePath, &orig.BackendURL, &orig.ServiceName, &orig.Tags, &orig.OfflineMsg)
	if err != nil {
		jsonErr(w, 404, "项目不存在")
		return
	}
	newID := orig.Repo + "-clone"
	orig.ID = newID
	orig.Name = orig.Name + " (副本)"
	orig.Status = "offline"
	db.Exec("INSERT INTO projects (id, name, repo, description, icon_url, stack, port, db_type, dsn, users_count, status, sort_order, is_pinned, icon_cls, base_path, backend_url, service_name, tags, offline_msg) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)",
		orig.ID, orig.Name, orig.Repo, orig.Desc, orig.IconURL, orig.Stack, orig.Port, orig.DB, orig.DSN, orig.UserCount, orig.Status, orig.Order, orig.Pinned, orig.IconCls, orig.BasePath, orig.BackendURL, orig.ServiceName, orig.Tags, orig.OfflineMsg)
	jsonOKCreated(w, orig)
}

// ============================================================
// Nginx Sync Service
// ============================================================

func syncNginx() {
	rows, err := db.Query("SELECT name, base_path, backend_url FROM projects WHERE base_path IS NOT NULL AND base_path != ''")
	if err != nil {
		return
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		var p Project
		rows.Scan(&p.Name, &p.BasePath, &p.BackendURL)
		projects = append(projects, p)
	}

	conf := buildNginxConfig(projects)
	cmd := exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5", "ubuntu@$WOOL_IP",
		fmt.Sprintf("sudo tee /etc/nginx/sites-available/lambs-managed.conf > /dev/null"))
	cmd.Stdin = bytes.NewReader([]byte(conf))
	cmd.Run()

	cmd = exec.Command("ssh", "-o", "StrictHostKeyChecking=no", "-o", "ConnectTimeout=5", "ubuntu@$WOOL_IP",
		"sudo nginx -t && sudo systemctl reload nginx")
	cmd.Run()
}

func buildNginxConfig(projects []Project) string {
	lines := []string{"# Auto-generated by Lambs. Last sync: managed projects.", ""}
	for _, p := range projects {
		backend := p.BackendURL
		if backend == "" {
			backend = fmt.Sprintf("http://%s:3501", lambsIP)
		}
		lines = append(lines, fmt.Sprintf(`
# %s — Lambs managed
location = /%s/favicon.svg {
    proxy_pass " + lambsIP + ":3602/api/gate/project-logo?path=/%s;
    proxy_set_header Host $host;
    expires 1h;
}
location = /lambs-gate-%s {
    internal;
    proxy_pass " + lambsIP + ":3602/api/gate/check-internal?path=/%s;
    proxy_pass_request_body off;
    proxy_set_header Content-Length "";
    proxy_set_header Host $host;
}
location /%s/api/ {
    auth_request /lambs-gate-%s;
    proxy_pass %s/api/;
    proxy_set_header Host $host;
    proxy_set_header X-Real-IP $remote_addr;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
}
location /%s {
    auth_request /lambs-gate-%s;
    try_files $uri $uri/ /%s/index.html;
}`, p.Name, p.BasePath, p.BasePath, p.BasePath, p.BasePath,
			p.BasePath, p.BasePath, backend, p.BasePath, p.BasePath, p.BasePath))
	}
	return strings.Join(lines, "\n")
}

// ============================================================
// Health Check Service
// ============================================================

func testDSN(dsn string) map[string]interface{} {
	if dsn == "" || dsn == "—" {
		return map[string]interface{}{"reachable": false, "error": "未配置数据源"}
	}
	if strings.HasPrefix(dsn, "http") {
		resp, err := http.Get(dsn)
		if err != nil {
			return map[string]interface{}{"reachable": false, "error": err.Error()}
		}
		resp.Body.Close()
		return map[string]interface{}{"reachable": resp.StatusCode < 500, "latency_ms": 0, "db_type": "rest_api"}
	}
	// Try SQLAlchemy-style connection test
	dsn2 := strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)
	dsn2 = strings.Replace(dsn2, "sqlite:///", "", 1)
	if strings.Contains(dsn, "postgres") {
		tdb, err := sql.Open("postgres", dsn2+"?connect_timeout=5")
		if err == nil {
			err = tdb.Ping()
			tdb.Close()
			if err == nil {
				return map[string]interface{}{"reachable": true, "latency_ms": 0, "db_type": "postgresql"}
			}
		}
		return map[string]interface{}{"reachable": false, "error": err.Error()}
	}
	return map[string]interface{}{"reachable": true, "latency_ms": 0}
}

func syncUserData(dsn string) []map[string]interface{} {
	if dsn == "" || dsn == "—" {
		return nil
	}
	dsn2 := strings.Replace(dsn, "postgresql+asyncpg://", "postgres://", 1)
	dsn2 = strings.Replace(dsn2, "sqlite:///", "", 1)
	if strings.Contains(dsn, "postgres") {
		tdb, err := sql.Open("postgres", dsn2+"?connect_timeout=5")
		if err != nil {
			return nil
		}
		defer tdb.Close()
		for _, table := range []string{"users", "user", "accounts", "member"} {
			rows, err := tdb.Query(fmt.Sprintf("SELECT * FROM %s LIMIT 500", table))
			if err != nil {
				continue
			}
			defer rows.Close()
			cols, _ := rows.Columns()
			var result []map[string]interface{}
			for rows.Next() {
				vals := make([]interface{}, len(cols))
				ptrs := make([]interface{}, len(cols))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				rows.Scan(ptrs...)
				row := make(map[string]interface{})
				for i, c := range cols {
					if !strings.Contains(strings.ToLower(c), "password") && !strings.Contains(strings.ToLower(c), "token") {
						row[c] = fmt.Sprintf("%v", vals[i])
					}
				}
				result = append(result, row)
			}
			return result
		}
	}
	return nil
}

// ============================================================
// Router & Path Extraction Helpers
// ============================================================

type route struct {
	method  string
	pattern string
	handler http.HandlerFunc
	auth    bool   // "" = public, "auth" = auth required, "admin" = super_admin required
}

func extractID(path, prefix string) string {
	s := strings.TrimPrefix(path, prefix)
	if idx := strings.Index(s, "/"); idx >= 0 {
		return s[:idx]
	}
	return s
}

// extractID2 extracts second path segment (e.g. /projects/{id}/members/{uid})
func extractID2(path, prefix, suffix string) (string, string) {
	s := strings.TrimPrefix(path, prefix)
	parts := strings.Split(strings.TrimSuffix(s, suffix), "/")
	if len(parts) >= 2 {
		return parts[0], parts[1]
	}
	return "", ""
}


	var lambsIP = os.Getenv("LAMBS_IP")
	if lambsIP == "" { lambsIP = "100.92.91.11" }
	var woolIP = os.Getenv("WOOL_IP")
	if woolIP == "" { woolIP = "100.126.18.126" }


func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	lambsIP := os.Getenv("LAMBS_IP")
	if lambsIP == "" { lambsIP = "100.92.91.11" }
	woolIP := os.Getenv("WOOL_IP")
	if woolIP == "" { woolIP = "100.126.18.126" }
	_ = lambsIP
	_ = woolIP

	cfgPath := os.Getenv("LAMBS_CONFIG_PATH")
	if cfgPath == "" { cfgPath = "/home/ubuntu/apps/lambs-server/lambs_config.json" }
	if data, err := os.ReadFile(cfgPath); err == nil {
		json.Unmarshal(data, &lambsConfig)
	}
	jwtKey = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtKey) == 0 { jwtKey = []byte(lambsConfig.JWTSecret) }
	if len(jwtKey) == 0 { log.Fatal("JWT_SECRET not set") }
	}
	jwtKey = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtKey) == 0 { jwtKey = []byte(lambsConfig.JWTSecret) }
	if len(jwtKey) == 0 { log.Fatal("JWT_SECRET not set") }
	initDB()
	defer db.Close()

	mux := http.NewServeMux()

	// === Public Routes ===
	mux.HandleFunc("POST /api/auth/login", corsMiddleware(handleLogin))
	mux.HandleFunc("GET /api/health", corsMiddleware(handleHealth))
	mux.HandleFunc("GET /api/system/health", corsMiddleware(handleHealth))
	mux.HandleFunc("GET /api/gate/check-internal", corsMiddleware(handleGateCheckInternal))
	mux.HandleFunc("GET /api/gate/offline-page", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(`<!DOCTYPE html><html><head><meta charset="utf-8"><title>维护中</title><style>body{display:flex;align-items:center;justify-content:center;height:100vh;margin:0;background:#0B0E13;color:#8B93A3;font-family:sans-serif}div{text-align:center}h1{color:#FFA13B}</style></head><body><div><h1>🔐 系统维护中</h1><p>该项目正在维护，请稍后再试</p></div></body></html>`))
	}))
	mux.HandleFunc("GET /api/gate/project-logo", corsMiddleware(handleGateProjectLogo))
	mux.HandleFunc("POST /api/auth/register", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		jsonErr(w, 400, "注册功能暂未开放")
	}))

	// === Auth Required ===
	a := func(h http.HandlerFunc) http.HandlerFunc { return corsMiddleware(authMiddleware(h)) }
	sa := func(h http.HandlerFunc) http.HandlerFunc { return corsMiddleware(requireSuperAdmin(h)) }

	// Auth
	mux.HandleFunc("GET /api/auth/me", a(handleMe))
	mux.HandleFunc("GET /api/me", a(handleMe))

	// Projects
	mux.HandleFunc("GET /api/projects", a(handleListProjects))
	mux.HandleFunc("GET /api/projects/stats", a(handleProjectStats))
	mux.HandleFunc("GET /api/projects/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		handleGetProject(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/projects", a(handleCreateProject))
	mux.HandleFunc("PUT /api/projects/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		handleUpdateProject(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("DELETE /api/projects/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteProject(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("PATCH /api/projects/{id}/status", a(func(w http.ResponseWriter, r *http.Request) {
		handlePatchProjectStatus(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("PATCH /api/projects/{id}/pin", a(func(w http.ResponseWriter, r *http.Request) {
		handlePinProject(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("PATCH /api/projects/reorder", a(handleReorderProjects))
	mux.HandleFunc("POST /api/projects/{id}/test-connection", a(func(w http.ResponseWriter, r *http.Request) {
		handleTestConnection(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/projects/{id}/sync", a(func(w http.ResponseWriter, r *http.Request) {
		handleSyncProject(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/projects/refresh-all", a(handleRefreshAll))
	mux.HandleFunc("GET /api/projects/{id}/logs", a(func(w http.ResponseWriter, r *http.Request) {
		handleProjectLogs(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /api/projects/{id}/tables", a(func(w http.ResponseWriter, r *http.Request) {
		handleProjectTables(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /api/projects/{id}/members", a(func(w http.ResponseWriter, r *http.Request) {
		handleProjectMembers(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/projects/{id}/members", a(func(w http.ResponseWriter, r *http.Request) {
		handleAddMember(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("DELETE /api/projects/{id}/members/{uid}", a(func(w http.ResponseWriter, r *http.Request) {
		handleRemoveMember(w, r, r.PathValue("id"), r.PathValue("uid"))
	}))
	mux.HandleFunc("POST /api/projects/{id}/clone", a(func(w http.ResponseWriter, r *http.Request) {
		handleCloneProject(w, r, r.PathValue("id"))
	}))

	// Users
	mux.HandleFunc("GET /api/users", sa(handleListUsers))
	mux.HandleFunc("POST /api/users", sa(handleCreateUser))
	mux.HandleFunc("PUT /api/users/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		handleUpdateUser(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("DELETE /api/users/{id}", sa(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteUser(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("POST /api/users/{id}/reset-password", sa(func(w http.ResponseWriter, r *http.Request) {
		handleResetPassword(w, r, r.PathValue("id"))
	}))

	// Gate
	mux.HandleFunc("GET /api/gate/check", a(handleGateCheck))

	// Settings
	mux.HandleFunc("GET /api/settings/config", sa(handleGetConfig))
	mux.HandleFunc("PUT /api/settings/config", sa(handleUpdateConfig))
	mux.HandleFunc("GET /api/settings/export/projects", sa(handleExportProjects))
	mux.HandleFunc("GET /api/settings/export/users", sa(handleExportUsers))
	mux.HandleFunc("GET /api/settings/audit-logs", sa(handleAuditLogs))

	// Backups
	mux.HandleFunc("POST /api/backups/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		handleCreateBackup(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /api/backups/{id}", a(func(w http.ResponseWriter, r *http.Request) {
		handleListBackups(w, r, r.PathValue("id"))
	}))
	mux.HandleFunc("GET /api/backups/{id}/download/{file}", a(func(w http.ResponseWriter, r *http.Request) {
		handleDownloadBackup(w, r, r.PathValue("id"), r.PathValue("file"))
	}))
	mux.HandleFunc("DELETE /api/backups/{id}/download/{file}", a(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteBackup(w, r, r.PathValue("id"), r.PathValue("file"))
	}))

	// Notifications
	mux.HandleFunc("GET /api/notifications", a(handleListNotifications))
	mux.HandleFunc("POST /api/notifications/{nid}/read", a(func(w http.ResponseWriter, r *http.Request) {
		handleReadNotification(w, r, r.PathValue("nid"))
	}))
	mux.HandleFunc("POST /api/notifications/read-all", a(handleReadAllNotifications))
	mux.HandleFunc("DELETE /api/notifications/{nid}", a(func(w http.ResponseWriter, r *http.Request) {
		handleDeleteNotification(w, r, r.PathValue("nid"))
	}))


	// Catch-all OPTIONS for CORS preflight
	mux.HandleFunc("OPTIONS /", corsMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))

	port := lambsConfig.Port
	if envPort := os.Getenv("PORT"); envPort != "" {
		if p, err := strconv.Atoi(envPort); err == nil {
			port = p
		}
	}
	if port == 0 {
		port = 3602
	}
	addr := fmt.Sprintf(":%d", port)
	log.Printf("Lambs Go Server starting on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

