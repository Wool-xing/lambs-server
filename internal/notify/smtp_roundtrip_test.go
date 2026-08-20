package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"lambs-server-go/internal/models"
)

// selfSignedCert returns an ephemeral self-signed cert so the fake server
// can speak STARTTLS. Generated per run — nothing committed.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("gen key: %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{"127.0.0.1"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// fakeSMTP logs every line both ways and records the DATA payload.
type fakeSMTP struct {
	ln   net.Listener
	logs []string
	msg  string
	done chan struct{}
}

// serve handles exactly one connection: EHLO, STARTTLS, AUTH, MAIL, RCPT,
// DATA, QUIT. Post-TLS there is NO re-EHLO from Go's smtp client (didHello
// stays true) — the server goes straight back to the command loop.
func (s *fakeSMTP) serve(cert tls.Certificate) {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	r := bufio.NewReader(conn)
	w := conn
	write := func(line string) {
		s.logs = append(s.logs, "S> "+line)
		fmt.Fprintf(w, "%s\r\n", line)
	}
	// read returns ("", err) only on real read failure — a blank line in
	// the DATA payload is legit data, not EOF (that conflation was the
	// deadlock: server closed the conn on the empty header separator line).
	read := func() (string, error) {
		line, err := r.ReadString('\n')
		if err != nil {
			s.logs = append(s.logs, fmt.Sprintf("C> (read err: %v)", err))
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		s.logs = append(s.logs, "C< "+line)
		return line, nil
	}
	write("220 fake ESMTP ready")
	var data strings.Builder
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
				s.logs = append(s.logs, fmt.Sprintf("S> (tls handshake err: %v)", err))
				return
			}
			conn, w, r = tconn, tconn, bufio.NewReader(tconn)
		case strings.HasPrefix(up, "AUTH"):
			write("235 2.7.0 ok")
		case strings.HasPrefix(up, "MAIL"), strings.HasPrefix(up, "RCPT"):
			write("250 ok")
		case strings.HasPrefix(up, "DATA"):
			write("354 end with .")
			for {
				d, err := read()
				if err != nil {
					return
				}
				if d == "." {
					break
				}
				data.WriteString(d)
				data.WriteString("\n")
			}
			s.msg = data.String()
			write("250 queued")
		case strings.HasPrefix(up, "QUIT"):
			write("221 bye")
			close(s.done)
			return
		default:
			write("500 unknown")
		}
	}
}

// runSendMail calls SendMail with a hang guard; on timeout the full line log
// is dumped so the stall point is visible.
func runSendMail(t *testing.T, s *fakeSMTP, to string) error {
	t.Helper()
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() { ch <- result{SendMail(to, "测试主题", "line1\nline2")} }()
	select {
	case r := <-ch:
		return r.err
	case <-time.After(15 * time.Second):
		t.Fatalf("SendMail hung. Server log:\n%s", strings.Join(s.logs, "\n"))
		return nil
	}
}

// TestSendMailRoundTrip — full STARTTLS+AUTH+message round trip against a
// fake server, verifying the received payload and the mail-side log.
func TestSendMailRoundTrip(t *testing.T) {
	t.Setenv("LAMBS_SMTP_INSECURE", "1")
	cert := selfSignedCert(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	s := &fakeSMTP{ln: ln, done: make(chan struct{})}
	go s.serve(cert)

	SetConfig(&models.Config{
		SMTPHost:     "127.0.0.1",
		SMTPPort:     fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port),
		SMTPUser:     "testuser",
		SMTPPassword: "testpass",
		SMTPFrom:     "from@example.com",
	})
	if err := runSendMail(t, s, "to@example.com"); err != nil {
		t.Fatalf("SendMail: %v\nServer log:\n%s", err, strings.Join(s.logs, "\n"))
	}
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Fatalf("server never finished QUIT\nlog:\n%s", strings.Join(s.logs, "\n"))
	}
	if !strings.Contains(s.msg, "测试主题") || !strings.Contains(s.msg, "line2") {
		t.Errorf("received message missing content: %q", s.msg)
	}
}

// TestSendMailRejectsNoSTARTTLS — a server without STARTTLS must be refused:
// credentials never cross in plaintext (R8 contract).
func TestSendMailRejectsNoSTARTTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		fmt.Fprintf(conn, "220 fake\r\n")
		r.ReadString('\n') // EHLO
		fmt.Fprintf(conn, "250 fake\r\n")
	}()

	SetConfig(&models.Config{
		SMTPHost: "127.0.0.1",
		SMTPPort: fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port),
		SMTPUser: "u", SMTPPassword: "p",
		SMTPFrom: "from@example.com",
	})
	err = SendMail("to@example.com", "s", "b")
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("err = %v, want STARTTLS refusal", err)
	}
}
