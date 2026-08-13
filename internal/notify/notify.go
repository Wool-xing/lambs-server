package notify

import (
	"crypto/tls"
	"log"
	"net/smtp"

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
	SendMail(config.AdminEmail, subject, body)
}

// SendMail sends a plain-text email over SMTP with STARTTLS.
// Silently skips if SMTP is not configured.
func SendMail(to, subject, body string) {
	if config == nil || config.SMTPHost == "" || config.SMTPFrom == "" || to == "" {
		return
	}
	addr := config.SMTPHost + ":" + config.SMTPPort
	var auth smtp.Auth
	if config.SMTPUser != "" {
		auth = smtp.PlainAuth("", config.SMTPUser, config.SMTPPassword, config.SMTPHost)
	}

	c, err := smtp.Dial(addr)
	if err != nil {
		log.Printf("notify: smtp dial failed: %v", err)
		return
	}
	defer c.Close()
	if err := c.Hello("lambs"); err != nil {
		log.Printf("notify: smtp hello failed: %v", err)
		return
	}
	// Upgrade to TLS if server supports STARTTLS
	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: config.SMTPHost}); err != nil {
			log.Printf("notify: starttls failed: %v", err)
			return
		}
	}
	if auth != nil {
		if err := c.Auth(auth); err != nil {
			log.Printf("notify: smtp auth failed: %v", err)
			return
		}
	}
	if err := c.Mail(config.SMTPFrom); err != nil {
		log.Printf("notify: smtp mail failed: %v", err)
		return
	}
	if err := c.Rcpt(to); err != nil {
		log.Printf("notify: smtp rcpt failed: %v", err)
		return
	}
	w, err := c.Data()
	if err != nil {
		log.Printf("notify: smtp data failed: %v", err)
		return
	}
	msg := "From: " + config.SMTPFrom + "\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + subject + "\r\n" +
		"MIME-Version: 1.0\r\n" +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
		body + "\r\n"
	if _, err := w.Write([]byte(msg)); err != nil {
		log.Printf("notify: smtp write failed: %v", err)
		return
	}
	if err := w.Close(); err != nil {
		log.Printf("notify: smtp close failed: %v", err)
		return
	}
	if err := c.Quit(); err != nil {
		log.Printf("notify: smtp quit failed: %v", err)
		return
	}
	log.Printf("notify: email sent to %s: %s", to, subject)
}
