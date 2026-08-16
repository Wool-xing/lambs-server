package auth

import (
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var JWTKey []byte

var corsOrigin = os.Getenv("CORS_ORIGIN")

// usernameRe is the registration charset gate — blocks control chars and
// HTML that would end up in audit logs and every list view.
var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9_.\-\p{Han}]+$`)

func init() {
	if corsOrigin == "" { corsOrigin = "https://wool.cc.cd" }
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// newSaltHex returns a random 16-byte salt as a 32-char hex string. The salt
// is public (stored per-user, returned by /auth/salt) — its job is to make a
// single rainbow table useless across accounts (R7 salted client hashing).
func NewSaltHex() string {
	b := make([]byte, 16)
	if _, err := crand.Read(b); err != nil {
		log.Printf("newSaltHex: crypto/rand failed: %v", err)
		return ""
	}
	return hex.EncodeToString(b)
}

// isSHA256Hex reports whether s is a lowercase 64-char hex digest — the shape
// the client sends after hashing (password + salt). Anything else is treated
// as a legacy plaintext password.
func IsSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// verifyPassword checks a login payload against the stored bcrypt hash.
// New contract (R7): client sends sha256(password+salt), the DB stores
// bcrypt(that payload). Legacy rows (salt='') store bcrypt(sha256(plain)),
// and the legacy frontend sends plaintext — both match via the sha256Hex
// wrap on the incoming value.
// Returns ok=true on match; legacy=true when the legacy path matched and the
// account should be upgraded to a salt.
func verifyPassword(storedHash, payload string) (ok, legacy bool) {
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(payload)) == nil {
		return true, false
	}
	// Legacy fallback: plaintext from the old frontend, or pre-salt rows
	// verified against bcrypt(sha256(plain)).
	if bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(sha256Hex(payload))) == nil {
		return true, true
	}
	return false, false
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
		var dbVer int
		if db.DB != nil {
			if err := db.DB.QueryRow("SELECT role, status, COALESCE(token_version,0) FROM users WHERE id=$1", userID).Scan(&dbRole, &dbStatus, &dbVer); err == nil {
				if dbStatus != "active" {
					JSONErr(w, 401, "账号已停用")
					return
				}
				// Password change bumps token_version — tokens issued before
				// it must die immediately, not after expiry.
				claimVer := 0
				if v, ok := mapClaims["tv"].(float64); ok {
					claimVer = int(v)
				} else {
					JSONErr(w, 401, "token失效")
					return
				}
				if claimVer != dbVer {
					JSONErr(w, 401, "token失效")
					return
				}
				role = dbRole
			} else {
				// Fail-closed: DB unreachable or user row missing — do not
				// trust JWT claims. Revoked users must never pass through.
				JSONErr(w, 401, "认证校验失败")
				return
			}
		}
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
	var tokenVer int
	var salt string
	err := db.DB.QueryRow("SELECT id, username, name, email, password_hash, role, status, COALESCE(token_version,0), COALESCE(pwd_salt,'') FROM users WHERE username=$1",
		req.Username).Scan(&user.ID, &user.Username, &user.Name, &user.Email, &user.PasswordHash, &user.Role, &user.Status, &tokenVer, &salt)
	if err != nil {
		JSONErr(w, 401, "用户名或密码错误")
		return
	}
	if user.Status != "active" {
		JSONErr(w, 403, "账号已停用")
		return
	}
	ok, legacy := verifyPassword(user.PasswordHash, req.Password)
	if !ok {
		JSONErr(w, 401, "用户名或密码错误")
		return
	}
	// Legacy account (no salt): upgrade in place to the salted contract so
	// the next login only needs the new path (R7 transparent migration).
	if legacy && salt == "" {
		if ns := NewSaltHex(); ns != "" {
			if h, err := bcrypt.GenerateFromPassword([]byte(sha256Hex(req.Password + ns)), bcrypt.DefaultCost); err == nil {
				db.DB.Exec("UPDATE users SET pwd_salt=$1, password_hash=$2 WHERE id=$3", ns, string(h), user.ID)
			}
		}
	}
	// Store UTC — the column is timestamp WITHOUT time zone and the frontend
	// formats as UTC+8. A local-time value gets the +8 applied twice.
	db.DB.Exec("UPDATE users SET last_login=$1 WHERE id=$2", time.Now().UTC().Format(time.RFC3339), user.ID)
	db.DB.Exec("INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ($1,$2,$3,$4,$5)",
		user.ID, user.Username, "登录", user.Username, "登录成功")

	claims := jwt.MapClaims{
		"user_id":  user.ID,
		"username": user.Username,
		"role":     user.Role,
		"tv":       tokenVer,
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

// HandleSalt returns the account's public password salt so the client can
// compute sha256(password+salt) before sending (R7). Unknown usernames get an
// empty salt — no account enumeration beyond what login already reveals.
func HandleSalt(w http.ResponseWriter, r *http.Request) {
	username := strings.TrimSpace(r.URL.Query().Get("username"))
	var salt string
	if username != "" {
		db.DB.QueryRow("SELECT COALESCE(pwd_salt,'') FROM users WHERE username=$1", username).Scan(&salt)
	}
	JSONOK(w, map[string]string{"salt": salt})
}

// HandleRegister creates a viewer account with no project access. A
// super_admin must grant project_access before the account can see anything —
// registration alone opens no data.
func HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Password string `json:"password"`
		Salt     string `json:"salt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONErr(w, 400, "无效请求")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Username == "" || len(req.Username) > 64 {
		JSONErr(w, 400, "用户名不能为空且不超过64位")
		return
	}
	if !usernameRe.MatchString(req.Username) {
		JSONErr(w, 400, "用户名仅支持字母、数字、下划线、点、连字符和中文")
		return
	}
	if !strings.Contains(req.Email, "@") || !strings.Contains(strings.Split(req.Email, "@")[len(strings.Split(req.Email, "@"))-1], ".") {
		JSONErr(w, 400, "邮箱格式不正确")
		return
	}
	if len(req.Password) < 6 {
		JSONErr(w, 400, "密码至少6位")
		return
	}
	// R7 salted contract: the new client sends sha256(password+salt) with the
	// salt it generated locally. A plaintext password (legacy client) keeps
	// the old bcrypt(sha256(plain)) shape.
	var salt string
	if len(req.Salt) > 0 {
		salt = req.Salt
	}
	if salt != "" && (len(salt) != 32 || !IsSHA256Hex(salt)) {
		JSONErr(w, 400, "盐格式不正确")
		return
	}
	payload := req.Password
	if !IsSHA256Hex(payload) {
		payload = sha256Hex(payload) // legacy plaintext → old shape
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(payload), bcrypt.DefaultCost)
	if err != nil {
		JSONErr(w, 500, "密码处理失败")
		return
	}
	var id string
	err = db.DB.QueryRow("INSERT INTO users (id, username, name, email, password_hash, role, status, project_access, pwd_salt) VALUES (gen_random_uuid(),$1,$2,$3,$4,'viewer','active','[]',$5) RETURNING id::text",
		req.Username, req.Username, req.Email, string(hash), salt).Scan(&id)
	if err != nil {
		JSONErr(w, 400, "用户名或邮箱已被注册")
		return
	}
	db.DB.Exec("INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ($1,$2,$3,$4,$5)",
		id, req.Username, "注册", "Lambs", "新用户注册(viewer)")
	// Auto-login: issue the same 8h token the login endpoint would.
	claims := jwt.MapClaims{
		"user_id":  id,
		"username": req.Username,
		"role":     "viewer",
		"tv":       0,
		"exp":      jwt.NewNumericDate(time.Now().Add(8 * time.Hour)),
		"iat":      jwt.NewNumericDate(time.Now()),
	}
	tokenStr, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(JWTKey)
	if err != nil {
		JSONErr(w, 500, "token生成失败")
		return
	}
	JSONCreated(w, map[string]interface{}{
		"access_token": tokenStr,
		"token_type":   "bearer",
		"user":         map[string]interface{}{"id": id, "username": req.Username, "name": req.Username, "email": req.Email, "role": "viewer", "status": "active"},
	})
}

// HandleMePassword lets a user change their own password (old password check,
// token_version bump revokes every existing token including this one).
func HandleMePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONErr(w, 400, "无效请求")
		return
	}
	if req.New == "" || len(req.New) < 6 {
		JSONErr(w, 400, "新密码至少6位")
		return
	}
	userID := r.Header.Get("X-User-ID")
	var hash string
	if err := db.DB.QueryRow("SELECT password_hash FROM users WHERE id=$1", userID).Scan(&hash); err != nil {
		JSONErr(w, 404, "用户不存在")
		return
	}
	if ok, _ := verifyPassword(hash, req.Old); !ok {
		JSONErr(w, 400, "原密码错误")
		return
	}
	// R7: the new client sends sha256(new+salt) for the new password; a
	// legacy client sends plaintext — wrap it once to keep the old shape.
	newPayload := req.New
	if !IsSHA256Hex(newPayload) {
		newPayload = sha256Hex(newPayload)
	}
	newHash, err := bcrypt.GenerateFromPassword([]byte(newPayload), bcrypt.DefaultCost)
	if err != nil {
		JSONErr(w, 500, "密码处理失败")
		return
	}
	if _, err := db.DB.Exec("UPDATE users SET password_hash=$1, token_version=COALESCE(token_version,0)+1 WHERE id=$2", string(newHash), userID); err != nil {
		log.Printf("ChangePassword: %v", err)
		JSONErr(w, 500, "重置密码失败")
		return
	}
	db.DB.Exec("INSERT INTO audit_logs (user_id, user_name, action, target, detail) VALUES ($1,$2,$3,$4,$5)",
		userID, r.Header.Get("X-Username"), "修改密码", r.Header.Get("X-Username"), "用户自助修改密码")
	JSONOK(w, map[string]string{"message": "密码已修改"})
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
