package notify

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/models"
)

// failSMTP walks the same STARTTLS+AUTH flow as fakeSMTP but fails one
// named stage so each SendMail error branch is exercised.
type failSMTP struct {
	ln     net.Listener
	failAt string // auth | mail | rcpt | data
}

func (s *failSMTP) run(cert tls.Certificate) {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	r := bufio.NewReader(conn)
	w := conn
	write := func(line string) { fmt.Fprintf(w, "%s\r\n", line) }
	read := func() (string, error) {
		line, err := r.ReadString('\n')
		if err != nil {
			return "", err
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	write("220 fake ESMTP ready")
	for {
		line, err := read()
		if err != nil {
			return
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			write("250-fake")
			write("250-STARTTLS")
			write("250 AUTH PLAIN")
		case strings.HasPrefix(up, "STARTTLS"):
			write("220 go ahead")
			tconn := tls.Server(conn, tlsCfg)
			if err := tconn.Handshake(); err != nil {
				return
			}
			conn, w, r = tconn, tconn, bufio.NewReader(tconn)
		case strings.HasPrefix(up, "AUTH"):
			if s.failAt == "auth" {
				write("535 auth rejected")
				return
			}
			write("235 2.7.0 ok")
		case strings.HasPrefix(up, "MAIL"):
			if s.failAt == "mail" {
				write("550 sender rejected")
				return
			}
			write("250 ok")
		case strings.HasPrefix(up, "RCPT"):
			if s.failAt == "rcpt" {
				write("550 recipient rejected")
				return
			}
			write("250 ok")
		case strings.HasPrefix(up, "DATA"):
			if s.failAt == "data" {
				write("554 no")
				return
			}
			write("354 end with .")
			for {
				d, err := read()
				if err != nil {
					return
				}
				if d == "." {
					break
				}
			}
			write("250 queued")
		case strings.HasPrefix(up, "QUIT"):
			write("221 bye")
			return
		default:
			write("500 unknown")
		}
	}
}

func runFailSMTP(t *testing.T, failAt, wantErr string) {
	t.Helper()
	t.Setenv("LAMBS_SMTP_INSECURE", "1")
	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	s := &failSMTP{ln: ln, failAt: failAt}
	go s.run(cert)

	oldCfg := config
	t.Cleanup(func() { config = oldCfg })
	SetConfig(&models.Config{
		SMTPHost: "127.0.0.1",
		SMTPPort: fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port),
		SMTPUser: "u", SMTPPassword: "p",
		SMTPFrom: "from@example.com",
	})
	done := make(chan error, 1)
	go func() { done <- SendMail("to@example.com", "s", "b") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("SendMail err = %v, want containing %q", err, wantErr)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("SendMail hung at stage %s", failAt)
	}
}

// TestSendMailStageFailures — auth/mail/rcpt/data rejections surface as
// distinct errors (SendMail was 76.9%: the mid-flight failure branches had
// zero coverage).
func TestSendMailStageFailures(t *testing.T) {
	for _, c := range []struct{ failAt, wantErr string }{
		{"auth", "smtp auth"},
		{"mail", "smtp mail"},
		{"rcpt", "smtp rcpt"},
		{"data", "smtp data"},
	} {
		t.Run(c.failAt, func(t *testing.T) {
			runFailSMTP(t, c.failAt, c.wantErr)
		})
	}
}

// TestNotifyAdminLogsFailure — a configured admin email with a failing
// SMTP is logged, never a panic (NotifyAdmin was 50.0%; the dial-failure
// branch of SendMail is already covered by notify_test's dial test).
func TestNotifyAdminLogsFailure(t *testing.T) {
	oldCfg := config
	t.Cleanup(func() { config = oldCfg })
	SetConfig(&models.Config{
		AdminEmail: "admin@example.com",
		SMTPHost:   "127.0.0.1",
		SMTPPort:   "1",
		SMTPFrom:   "from@example.com",
	})
	NotifyAdmin("备份失败", "详情")
	// Nothing to assert beyond no panic — the log line is the contract.
}
