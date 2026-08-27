package auth

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
	"lambs-server-go/internal/notify"
)

// forgotFixture rebuilds users + verification_codes with full DDL and
// returns exec helpers. Mirrors forgot_verify_test.go conventions.
func forgotFixture(t *testing.T) (func(string, ...interface{}), func(string, ...interface{}) interface{}) {
	dsn := os.Getenv("LAMBS_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LAMBS_TEST_PG_DSN not set — real PostgreSQL verification skipped")
	}
	if err := db.Init(dsn); err != nil {
		t.Fatalf("init db: %v", err)
	}
	JWTKey = []byte("test-key")
	mustExec := func(q string, args ...interface{}) {
		if _, err := db.DB.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	mustExec(`CREATE EXTENSION IF NOT EXISTS pgcrypto`)
	mustExec(`DROP TABLE IF EXISTS users CASCADE; DROP TABLE IF EXISTS verification_codes CASCADE;`)
	mustExec(`CREATE TABLE users (
		id UUID PRIMARY KEY, username TEXT UNIQUE, name TEXT, email TEXT UNIQUE,
		password_hash TEXT, role TEXT DEFAULT 'viewer', status TEXT DEFAULT 'active',
		token_version INT DEFAULT 0, pwd_salt TEXT DEFAULT '',
		project_access JSONB NOT NULL DEFAULT '[]',
		avatar_url TEXT DEFAULT '', avatar_thumb TEXT DEFAULT '',
		last_login TIMESTAMPTZ DEFAULT now(),
		created_at TIMESTAMPTZ DEFAULT now())`)
	EnsureForgotSchema()
	mustExec(`INSERT INTO users (id, username, name, email, password_hash, role) VALUES (gen_random_uuid(),'forgot-user','忘记密码用户','fu@t.c','x','viewer')`)
	t.Cleanup(func() { db.DB = nil })
	return mustExec, func(q string, args ...interface{}) interface{} {
		var v interface{}
		if err := db.DB.QueryRow(q, args...).Scan(&v); err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		return v
	}
}

func postForgot(t *testing.T, path string, body map[string]string) (int, string) {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest("POST", path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	if strings.HasSuffix(path, "verify") {
		HandleForgotVerify(w, r)
	} else {
		HandleForgotRequest(w, r)
	}
	return w.Code, w.Body.String()
}

// fakeSMTP speaks just enough SMTP+STARTTLS for SendMail: EHLO,
// STARTTLS upgrade, MAIL/RCPT/DATA/QUIT. Cert is self-signed — tests set
// LAMBS_SMTP_INSECURE=1 so the client skips verification.
func startFakeSMTP(t *testing.T) (host, port string) {
	cert := smtpTestCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				bw := bufio.NewWriter(c)
				write := func(s string) { bw.WriteString(s); bw.Flush() }
				write("220 fake smtp\r\n")
				tlsUp := false
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					upper := strings.ToUpper(strings.TrimSpace(line))
					switch {
					case strings.HasPrefix(upper, "EHLO"):
						write("250-fake\r\n250 STARTTLS\r\n")
					case strings.HasPrefix(upper, "STARTTLS"):
						write("220 ready\r\n")
						tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}})
						if err := tc.Handshake(); err != nil {
							return
						}
						br = bufio.NewReader(tc)
						bw = bufio.NewWriter(tc)
						tlsUp = true
					case strings.HasPrefix(upper, "MAIL"), strings.HasPrefix(upper, "RCPT"):
						write("250 ok\r\n")
					case strings.HasPrefix(upper, "DATA"):
						write("354 go\r\n")
						for {
							d, err := br.ReadString('\n')
							if err != nil {
								return
							}
							if strings.TrimSpace(d) == "." {
								break
							}
						}
						write("250 queued\r\n")
					case strings.HasPrefix(upper, "QUIT"):
						write("221 bye\r\n")
						return
					default:
						if tlsUp {
							write("250 ok\r\n")
						}
					}
				}
			}(conn)
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	return h, p
}

func smtpTestCert(t *testing.T) tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestHandleForgotRequestBranches(t *testing.T) {
	mustExec, _ := forgotFixture(t)

	// bad JSON (raw malformed bytes — json.Marshal can't produce invalid JSON) → 400
	r := httptest.NewRequest("POST", "/api/auth/forgot-password", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	HandleForgotRequest(w, r)
	if w.Code != 400 {
		t.Fatalf("bad json = %d, want 400", w.Code)
	}

	// (missing/empty username+email cases covered by TestHandleForgotRequestValidation)
	// CRLF injection → 400
	if c, _ := postForgot(t, "/api/auth/forgot-password", map[string]string{"username": "forgot-user", "email": "fu@t.c\r\nBcc: x"}); c != 400 {
		t.Fatalf("crlf email = %d", c)
	}
	// unknown user → 400
	if c, _ := postForgot(t, "/api/auth/forgot-password", map[string]string{"username": "ghost", "email": "g@t.c"}); c != 400 {
		t.Fatalf("unknown user = %d", c)
	}
	// SMTP unconfigured (notify config nil) → 503, code row consumed before send
	if c, _ := postForgot(t, "/api/auth/forgot-password", map[string]string{"username": "forgot-user", "email": "fu@t.c"}); c != 503 {
		t.Fatalf("smtp unconfigured = %d, want 503", c)
	}
	// 60s cooldown: fresh code row from the 503 attempt above → 429
	mustExec(`UPDATE verification_codes SET created_at = NOW() WHERE username='forgot-user'`)
	if c, _ := postForgot(t, "/api/auth/forgot-password", map[string]string{"username": "forgot-user", "email": "fu@t.c"}); c != 429 {
		t.Fatalf("cooldown = %d, want 429", c)
	}
}

func TestHandleForgotRequestSuccess(t *testing.T) {
	mustExec, _ := forgotFixture(t)
	host, port := startFakeSMTP(t)
	t.Setenv("LAMBS_SMTP_INSECURE", "1")
	notify.SetConfig(&models.Config{SMTPHost: host, SMTPPort: port, SMTPFrom: "noreply@lambs.local"})
	t.Cleanup(func() { notify.SetConfig(nil) })

	c, body := postForgot(t, "/api/auth/forgot-password", map[string]string{"username": "forgot-user", "email": "fu@t.c"})
	if c != 200 {
		t.Fatalf("success = %d (%s)", c, body)
	}
	// newest code row: unused + MAC-format code (not plaintext)
	var code, used string
	db.DB.QueryRow("SELECT code, used::text FROM verification_codes WHERE username='forgot-user' ORDER BY id DESC LIMIT 1").Scan(&code, &used)
	if used != "false" {
		t.Fatalf("newest code used = %s", used)
	}
	if len(code) != 64 {
		t.Fatalf("stored code %q not 64-hex (plaintext leak?)", code)
	}
	// older rows invalidated
	var open int
	db.DB.QueryRow("SELECT COUNT(*) FROM verification_codes WHERE username='forgot-user' AND used=FALSE").Scan(&open)
	if open != 1 {
		t.Fatalf("open code rows = %d, want 1", open)
	}
	mustExec(`DELETE FROM verification_codes`) // keep fixture clean for cooldown tests
}

func TestHandleForgotVerifyExtraBranches(t *testing.T) {
	mustExec, _ := forgotFixture(t)

	// bad JSON → 400
	r := httptest.NewRequest("POST", "/api/auth/forgot-password/verify", strings.NewReader("{bad"))
	w := httptest.NewRecorder()
	HandleForgotVerify(w, r)
	if w.Code != 400 {
		t.Fatalf("bad json = %d", w.Code)
	}
	// short password → 400
	if c, _ := postForgot(t, "/api/auth/forgot-password/verify", map[string]string{
		"username": "forgot-user", "email": "fu@t.c", "code": "123456", "new_password": "123"}); c != 400 {
		t.Fatalf("short pw = %d", c)
	}
	// no matching code → 400
	if c, _ := postForgot(t, "/api/auth/forgot-password/verify", map[string]string{
		"username": "forgot-user", "email": "fu@t.c", "code": "123456", "new_password": "newsecret1"}); c != 400 {
		t.Fatalf("no code = %d", c)
	}
	// attempts >= 5 → 400 尝试次数过多
	mustExec(`INSERT INTO verification_codes (username, email, code, used, attempts, expires_at) VALUES ('forgot-user','fu@t.c',$1,FALSE,5,NOW()+INTERVAL '5 minutes')`, codeMAC("123456"))
	if c, body := postForgot(t, "/api/auth/forgot-password/verify", map[string]string{
		"username": "forgot-user", "email": "fu@t.c", "code": "123456", "new_password": "newsecret1"}); c != 400 || !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("attempts cap = %d (%s)", c, body)
	}
	// per-account failure gate: newest code row under attempts-cap, but
	// SUM(attempts) across the 10-minute window >= 20 → 429
	mustExec(`DELETE FROM verification_codes`)
	for i := 0; i < 4; i++ {
		mustExec(`INSERT INTO verification_codes (username, email, code, used, attempts, created_at, expires_at) VALUES ('forgot-user','fu@t.c',$1,TRUE,5,NOW(),NOW()+INTERVAL '5 minutes')`, codeMAC("123456"))
	}
	mustExec(`INSERT INTO verification_codes (username, email, code, used, attempts, expires_at) VALUES ('forgot-user','fu@t.c',$1,FALSE,0,NOW()+INTERVAL '5 minutes')`, codeMAC("123456"))
	if c, _ := postForgot(t, "/api/auth/forgot-password/verify", map[string]string{
		"username": "forgot-user", "email": "fu@t.c", "code": "123456", "new_password": "newsecret1"}); c != 429 {
		t.Fatalf("recent-fails gate = %d, want 429", c)
	}
}

func TestHandleForgotVerifyPlaintextPassword(t *testing.T) {
	mustExec, _ := forgotFixture(t)
	mustExec(`UPDATE users SET pwd_salt='salt-xyz' WHERE username='forgot-user'`)
	mustExec(`INSERT INTO verification_codes (username, email, code, used, expires_at) VALUES ('forgot-user','fu@t.c',$1,FALSE,NOW()+INTERVAL '5 minutes')`, codeMAC("123456"))

	// Legacy client sends plaintext — handler wraps sha256(pw+salt) then bcrypts.
	c, body := postForgot(t, "/api/auth/forgot-password/verify", map[string]string{
		"username": "forgot-user", "email": "fu@t.c", "code": "123456", "new_password": "plainpw9"})
	if c != 200 {
		t.Fatalf("plaintext verify = %d (%s)", c, body)
	}
	var hash string
	db.DB.QueryRow("SELECT password_hash FROM users WHERE username='forgot-user'").Scan(&hash)
	want := sha256Hex("plainpw9" + "salt-xyz")
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(want)); err != nil {
		t.Fatalf("stored hash does not match sha256(pw+salt): %v", err)
	}
}
