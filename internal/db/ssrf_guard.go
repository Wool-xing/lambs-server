package db

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// CheckDSNHost rejects datasource targets that point at non-public networks
// (SSRF guard). Tailscale CGNAT (100.64/10) is allowed — managed datasources
// legitimately live there. Returns an error suitable for a 400 response.
func CheckDSNHost(dsn string) error {
	if dsn == "" || dsn == "—" || strings.HasPrefix(dsn, "sqlite") {
		return nil // local file or nothing to judge
	}
	host := ""
	if strings.HasPrefix(dsn, "http") || strings.HasPrefix(dsn, "mongodb") ||
		strings.HasPrefix(dsn, "mysql") || strings.HasPrefix(dsn, "redis") ||
		strings.HasPrefix(dsn, "postgres") {
		if u, err := url.Parse(dsn); err == nil && u.Hostname() != "" {
			host = u.Hostname()
		}
	}
	if host == "" {
		// mysql form: user:pass@tcp(host:port)/db
		if i := strings.Index(dsn, "tcp("); i >= 0 {
			rest := dsn[i+4:]
			if j := strings.Index(rest, ")"); j > 0 {
				host = strings.Split(rest[:j], ":")[0]
			}
		}
	}
	if host == "" {
		if h, _, err := net.SplitHostPort(dsn); err == nil {
			host = h
		}
	}
	if host == "" {
		return nil // unknown form — leave to the dialer
	}
	return checkHostPublic(host)
}

func checkHostPublic(host string) error {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		return nil // same-host datasources are legitimate (redis on 127.0.0.1 etc.)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("禁止访问无法解析的地址: %s", host)
	}
	for _, ip := range ips {
		// Loopback is allowed: several managed datasources legitimately run
		// on the server itself. The guard's job is to stop probes into
		// OTHER hosts' private networks.
		if ip.IsLoopback() {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return fmt.Errorf("禁止访问内网地址: %s", host)
		}
		if ip.IsPrivate() {
			return fmt.Errorf("禁止访问内网地址: %s", host)
		}
		if ip.To4() == nil && (ip[0]&0xFE) == 0xFC { // ULA fc00::/7
			return fmt.Errorf("禁止访问内网地址: %s", host)
		}
	}
	return nil
}
