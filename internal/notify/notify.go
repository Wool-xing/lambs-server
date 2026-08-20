package notify

import (
	"crypto/tls"
	"fmt"
	"log"
	"net/smtp"
	"os"

	"lambs-server-go/internal/models"
)

var config *models.Config

// SetConfig wires the server config into the notify package.
func SetConfig(cfg *models.Config) {
	config = cfg
}

// NotifyAdmin sends an email to the configured admin address.
func NotifyAdmin(subject, body string) {
	if config == nil || config.AdminEmail == "" {
		return
	}
	if err := SendMail(config.AdminEmail, subject, body); err != nil {
		log.Printf("notify: admin mail failed: %v", err)
	}
}

// SendMailForget sends a password-reset email. Errors are returned so the
// caller can tell the user the truth — a configured-but-failing SMTP must not
// report "验证码已发送" (R8: the old silent-swallow burned codes + cooldown).
func SendMailForget(to, subject, body string) error {
	if config == nil || config.SMTPHost == "" || config.SMTPFrom == "" {
		return fmt.Errorf("邮件服务未配置，请联系管理员重置密码")
	}
	return SendMail(to, subject, body)
}

// SendMail sends a plain-text email over SMTP with mandatory STARTTLS.
// All failures are returned (logged by the caller as needed).
func SendMail(to, subject, body string) error {
	if config == nil || config.SMTPHost == "" || config.SMTPFrom == "" || to == "" {
		return fmt.Errorf("smtp not configured")
	}
	addr := config.SMTPHost + ":" + config.SMTPPort
	var auth smtp.Auth
	if config.SMTPUser != "" {
		auth = smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)
	}

	c, err := smtp.Dial(addr)
	if err != nil {
		return fmt.Errorf("smtp dial: %w", err)
	}
	defer c.Close()
	if err := c.Hello("lambs"); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}
	// STARTTLS is mandatory: credentials must never cross the wire in
	// plaintext even against a misconfigured server (R8).
	ok, _ := c.Extension("STARTTLS")
	if !ok {
		return fmt.Errorf("smtp server does not support STARTTLS")
	}
	// 内网自签 SMTP 场景：LAMBS_SMTP_INSECURE=1 跳过证书验证
	// (自托管默认仍强制验证 — 不安全的默认值不可取)。
	tlsCfg := &tls.Config{ServerName: config.SMTPHost}
	if os.Getenv("LAMBS_SMTP_INSECURE") == "1" {
		tlsCfg.InsecureSkipVerify = true
	}
	if err := c.StartTLS(tlsCfg); err != nil {
		return fmt.Errorf("smtp starttls: %w", err)
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(config.SMTPFrom); err != nil {
		return fmt.Errorf("smtp mail: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("smtp rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	msg := "From: " + config.SMTPFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		body + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("smtp quit: %w", err)
	}
	log.Printf("notify: email sent to %s: %s", to, subject)
	return nil
}
