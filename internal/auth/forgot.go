package auth

import (
	crand "crypto/rand"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/notify"

	"golang.org/x/crypto/bcrypt"
)

// EnsureForgotSchema creates the verification_codes table if missing,
// and migrates the legacy Python-era table (no username column).
func EnsureForgotSchema() {
	db.DB.Exec(`CREATE TABLE IF NOT EXISTS verification_codes (
		id BIGSERIAL PRIMARY KEY,
		username TEXT NOT NULL,
		email TEXT NOT NULL,
		code TEXT NOT NULL,
		used BOOLEAN NOT NULL DEFAULT FALSE,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		expires_at TIMESTAMPTZ NOT NULL
	)`)
	db.DB.Exec(`ALTER TABLE verification_codes ADD COLUMN IF NOT EXISTS username TEXT NOT NULL DEFAULT ''`)
	db.DB.Exec(`ALTER TABLE verification_codes ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ`)
	db.DB.Exec(`ALTER TABLE verification_codes ADD COLUMN IF NOT EXISTS attempts INT NOT NULL DEFAULT 0`)
	db.DB.Exec(`ALTER TABLE users ADD COLUMN IF NOT EXISTS token_version INT NOT NULL DEFAULT 0`)
}

func randomCode() (string, error) {
	n, err := crand.Int(crand.Reader, big.NewInt(1000000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HandleForgotRequest verifies identity (username+email) and emails a 6-digit code.
func HandleForgotRequest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Email    string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONErr(w, 400, "无效请求")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	if req.Username == "" || req.Email == "" {
		JSONErr(w, 400, "请输入用户名和邮箱")
		return
	}
	var uid string
	err := db.DB.QueryRow("SELECT id FROM users WHERE username=$1 AND email=$2 AND status='active'", req.Username, req.Email).Scan(&uid)
	if err != nil {
		JSONErr(w, 400, "账号信息不匹配")
		return
	}
	// 60s resend cooldown per user
	var lastCreated time.Time
	db.DB.QueryRow("SELECT COALESCE(MAX(created_at), NOW() - INTERVAL '1 day') FROM verification_codes WHERE username=$1", req.Username).Scan(&lastCreated)
	if time.Since(lastCreated) < 60*time.Second {
		JSONErr(w, 429, "请勿频繁发送，请稍后再试")
		return
	}
	code, err := randomCode()
	if err != nil {
		JSONErr(w, 500, "验证码生成失败")
		return
	}
	// Invalidate older unused codes for this user
	db.DB.Exec("UPDATE verification_codes SET used=TRUE WHERE username=$1 AND used=FALSE", req.Username)
	if _, err := db.DB.Exec("INSERT INTO verification_codes (username, email, code, used, expires_at) VALUES ($1,$2,$3,FALSE, NOW() + INTERVAL '5 minutes')",
		req.Username, req.Email, code); err != nil {
		JSONErr(w, 500, "验证码生成失败")
		return
	}
	body := fmt.Sprintf("您的 Lambs 管理系统密码重置验证码是：%s\n\n验证码 5 分钟内有效。如非本人操作请忽略此邮件。", code)
	if err := notify.SendMailForget(req.Email, "Lambs 密码重置验证码", body); err != nil {
		JSONErr(w, 503, err.Error())
		return
	}
	JSONOK(w, map[string]string{"message": "验证码已发送，请查收邮箱"})
}

// HandleForgotVerify checks the code and sets the new password.
func HandleForgotVerify(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		Email       string `json:"email"`
		Code        string `json:"code"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		JSONErr(w, 400, "无效请求")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	req.Email = strings.TrimSpace(req.Email)
	req.Code = strings.TrimSpace(req.Code)
	if req.NewPassword == "" || len(req.NewPassword) < 6 {
		JSONErr(w, 400, "新密码至少6位")
		return
	}
	// Brute-force gate: at most 5 attempts per code before it must be re-issued.
	const maxAttempts = 5
	var id int64
	var dbCode string
	var attempts int
	err := db.DB.QueryRow("SELECT id, code, COALESCE(attempts,0) FROM verification_codes WHERE username=$1 AND email=$2 AND used=FALSE AND expires_at > NOW() ORDER BY id DESC LIMIT 1",
		req.Username, req.Email).Scan(&id, &dbCode, &attempts)
	if err != nil {
		JSONErr(w, 400, "验证码错误或已过期")
		return
	}
	if attempts >= maxAttempts {
		JSONErr(w, 400, "尝试次数过多，请重新获取验证码")
		return
	}
	if dbCode != req.Code {
		db.DB.Exec("UPDATE verification_codes SET attempts=attempts+1 WHERE id=$1", id)
		JSONErr(w, 400, fmt.Sprintf("验证码错误，剩余 %d 次尝试", maxAttempts-attempts-1))
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(sha256Hex(req.NewPassword)), bcrypt.DefaultCost)
	if err != nil {
		JSONErr(w, 500, "密码处理失败")
		return
	}
	// Bump token_version — every existing token for this account dies now.
	if _, err := db.DB.Exec("UPDATE users SET password_hash=$1, token_version=COALESCE(token_version,0)+1 WHERE username=$2", string(hash), req.Username); err != nil {
		JSONErr(w, 500, err.Error())
		return
	}
	db.DB.Exec("UPDATE verification_codes SET used=TRUE WHERE id=$1", id)
	JSONOK(w, map[string]string{"message": "密码已重置"})
}
