package auth

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var JWTKey []byte

var corsOrigin = os.Getenv("CORS_ORIGIN")

func init() {
	if corsOrigin == "" { corsOrigin = "https://wool.cc.cd" }
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// CORS returns middleware that sets CORS headers.
func CORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", corsOrigin)
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

// RequireAuth returns middleware that validates JWT tokens.
func RequireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" || !strings.HasPrefix(authHeader, "Bearer ") {
			JSONErr(w, 401, "未登录")
			return
		}
		tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
		mapClaims := jwt.MapClaims{}
		token, err := jwt.ParseWithClaims(tokenStr, mapClaims, func(t *jwt.Token) (interface{}, error) {
			// Only HS256 is ever issued — accept nothing else (alg confusion defense).
			if t.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
			}
			return JWTKey, nil
		})
		if err != nil || !token.Valid {
			JSONErr(w, 401, "token失效")
			return
		}
		userID, _ := mapClaims["user_id"].(string)
		username, _ := mapClaims["username"].(string)
		role, _ := mapClaims["role"].(string)
		// Fresh role/status from the database — role changes and account
		// deactivation take effect immediately instead of after token expiry.
		// (db.DB is nil only in unit tests — they exercise the claims path.)
		var dbRole, dbStatus string
		if db.DB != nil {
			if err := db.DB.QueryRow("SELECT role, status FROM users WHERE id=$1", userID).Scan(&dbRole, &dbStatus); err == nil {
				if dbStatus != "active" {
					JSONErr(w, 401, "账号已停用")
					return
				}
				role = dbRole
			}
		}
		// On DB errors keep claim values (graceful degradation).
		r.Header.Set("X-User-ID", userID)
		r.Header.Set("X-Username", username)
		r.Header.Set("X-Role", role)
		next(w, r)
	}
}

// RequireSuperAdmin returns middleware that requires super_admin role.
func RequireSuperAdmin(next http.HandlerFunc) http.HandlerFunc {
	return RequireAuth(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Role") != "super_admin" {
			JSONErr(w, 403, "需要超管权限")
			return
		}
		next(w, r)
	})
}

// WithAuth wraps a handler with auth + CORS.
func WithAuth(h http.HandlerFunc) http.HandlerFunc  { return CORS(RequireAuth(h)) }
func WithAdmin(h http.HandlerFunc) http.HandlerFunc { return CORS(RequireSuperAdmin(h)) }

// ── Helpers ────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, data interface{}, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(data)
}

func JSONOK(w http.ResponseWriter, data interface{}) { writeJSON(w, models.ApiResponse{Success: true, Data: data}, 200) }
func JSONCreated(w http.ResponseWriter, data interface{}) { writeJSON(w, models.ApiResponse{Success: true, Data: data}, http.StatusCreated) }
func JSONErr(w http.ResponseWriter, code int, msg string) { writeJSON(w, models.ApiResponse{Success: false, Error: msg}, code) }

// ── Handlers ───────────────────────────────────────────

func HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONErr(w, 400, "无效请求")
		return
	}
	var user models.User
	var avatarURL, lastLogin sql.NullString // don't need these for login
	_ = avatarURL
	_ = lastLogin
	err := db.DB.QueryRow("SELECT id, username, name, email, password_hash, role, status FROM users WHERE username=$1",
		req.Username).Scan(&user.ID, &user.Username, &user.Name, &user.Email, &user.PasswordHash, &user.Role, &user.Status)
	if err != nil {
		JSONErr(w, 401, "用户名或密码错误")
		return
	}
	if user.Status != "active" {
		JSONErr(w, 403, "账号已停用")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(sha256Hex(req.Password))) != nil {
		JSONErr(w, 401, "用户名或密码错误")
		return
	}
	db.DB.Exec("UPDATE users SET last_login=$1 WHERE id=$2", time.Now().Format(time.RFC3339), user.ID)
	db.DB.Exec("INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ($1,$2,$3,$4,$5)",
		user.ID, user.Username, "登录", "Lambs", "登录成功")

	claims := jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     user.Role,
		"exp":      jwt.NewNumericDate(time.Now().Add(8 * time.Hour)),
		"iat":      jwt.NewNumericDate(time.Now()),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	tokenStr, err := token.SignedString(JWTKey)
	if err != nil {
		JSONErr(w, 500, "token生成失败")
		return
	}
	JSONOK(w, map[string]interface{}{
		"access_token": tokenStr,
		"token_type":   "bearer",
		"user_id":      user.ID,
		"username":     user.Username,
		"name":         user.Name,
		"email":        user.Email,
		"role":         user.Role,
	})
}

func HandleMe(w http.ResponseWriter, r *http.Request) {
	userID := r.Header.Get("X-User-ID")
	var user models.User
	var avatarURL, pa sql.NullString
	err := db.DB.QueryRow("SELECT id, username, name, email, role, status, avatar_url, COALESCE(project_access::text,'[]') FROM users WHERE id=$1", userID).
		Scan(&user.ID, &user.Username, &user.Name, &user.Email, &user.Role, &user.Status, &avatarURL, &pa)
	if err != nil {
		JSONErr(w, 404, "用户不存在")
		return
	}
	if avatarURL.Valid {
		user.AvatarURL = avatarURL.String
	}
	if pa.Valid {
		user.ProjectAccess = pa.String
	}
	JSONOK(w, user)
}
