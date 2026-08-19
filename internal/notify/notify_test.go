package notify

import (
	"strings"
	"testing"

	"lambs-server-go/internal/models"
)

func TestSendMailConfigMatrix(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *models.Config
		to      string
		wantSub string
	}{
		{"nil config", nil, "a@b.c", "smtp not configured"},
		{"missing host", &models.Config{SMTPFrom: "x@y.z"}, "a@b.c", "smtp not configured"},
		{"missing from", &models.Config{SMTPHost: "smtp.example"}, "a@b.c", "smtp not configured"},
		{"missing recipient", &models.Config{SMTPHost: "smtp.example", SMTPFrom: "x@y.z"}, "", "smtp not configured"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			config = c.cfg
			err := SendMail(c.to, "subj", "body")
			if err == nil || !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("SendMail err = %v, want contains %q", err, c.wantSub)
			}
		})
	}
}

// TestSendMailForgetR8 — a configured-but-unset SMTP must error out so the
// caller never reports "验证码已发送" (R8: silent-swallow burned codes).
func TestSendMailForgetR8(t *testing.T) {
	config = nil
	err := SendMailForget("a@b.c", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "邮件服务未配置") {
		t.Errorf("SendMailForget nil config err = %v, want 邮件服务未配置", err)
	}
	config = &models.Config{} // SMTPHost empty
	err = SendMailForget("a@b.c", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "邮件服务未配置") {
		t.Errorf("SendMailForget unset host err = %v, want 邮件服务未配置", err)
	}
}

// TestSendMailDialFailure — a dead SMTP endpoint surfaces as an error.
func TestSendMailDialFailure(t *testing.T) {
	config = &models.Config{SMTPHost: "127.0.0.1", SMTPPort: "1", SMTPFrom: "x@y.z"}
	err := SendMail("a@b.c", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "smtp dial") {
		t.Errorf("SendMail dial err = %v, want smtp dial", err)
	}
}

// TestNotifyAdminSilentPaths — no panic, no send when unconfigured.
func TestNotifyAdminSilentPaths(t *testing.T) {
	config = nil
	NotifyAdmin("s", "b") // must not panic
	config = &models.Config{AdminEmail: ""}
	NotifyAdmin("s", "b") // no admin email → no send
}
