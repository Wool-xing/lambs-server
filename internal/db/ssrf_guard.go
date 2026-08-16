package db

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// CheckDSNHost rejects datasource targets that point at non-public networks
// (SSRF guard). Tailscale CGNAT (100.64/10) is allowed — managed datasources
// legitimately live there. Returns an error suitable for a 400 response.
func CheckDSNHost(dsn string) error {
	// Scheme matching is case-insensitive (parseScheme lowercases too); a
	// mixed-case "Mongo://" must not skip host extraction.
	dsn = strings.ToLower(dsn)
	if dsn == "" || dsn == "—" || strings.HasPrefix(dsn, "sqlite") {
		return nil // local file or nothing to judge
	}
	host := ""
	if strings.HasPrefix(dsn, "http") || strings.HasPrefix(dsn, "mongo") ||
		strings.HasPrefix(dsn, "mysql") || strings.HasPrefix(dsn, "redis") ||
		strings.HasPrefix(dsn, "postgres") || strings.HasPrefix(dsn, "qdrant") {
		if u, err := url.Parse(dsn); err == nil && u.Hostname() != "" {
			host = u.Hostname()
			// lib/pq convertURL promotes query params to keyword args —
			// ?hostaddr=10.0.0.5 overrides the URL host at dial time (R6).
			for _, k := range []string{"host", "hostaddr"} {
				if q := u.Query().Get(k); q != "" {
					if err := checkHostPublic(q); err != nil {
						return err
					}
				}
			}
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
		// lib/pq keyword form: "host=10.0.0.5 port=5432 ...". Collect EVERY
		// host=/hostaddr= occurrence — lib/pq parses duplicate keys with
		// last-wins, and hostaddr takes dial precedence over host, so a
		// single checked entry is not enough (R6). Values may be quoted and
		// '=' may be surrounded by spaces.
		for _, m := range keywordHostRe.FindAllStringSubmatch(dsn, -1) {
			v := strings.Trim(m[2], `'"`)
			for _, h := range strings.Split(v, ",") {
				h = strings.TrimSpace(h)
				if h == "" {
					continue // trailing comma / empty entry — nothing to check
				}
				if err := checkHostPublic(h); err != nil {
					return err
				}
			}
		}
		return nil // keyword form fully judged above (fail-closed per entry)
	}
	if host == "" {
		return nil // unknown form — leave to the dialer
	}
	return checkHostPublic(host)
}

// keywordHostRe matches host=/hostaddr= keyword assignments in lib/pq DSNs
// (dsn is lowercased before matching). lib/pq trims spaces around '=' and
// accepts quoted values.
var keywordHostRe = regexp.MustCompile(`(?:^|\s)(host|hostaddr)\s*=\s*('[^']*'|"[^"]*"|\S+)`)

// resolvePublicHosts validates a hostname and returns its resolved IPs.
// Loopback is allowed (managed datasources legitimately run on the server
// itself); the guard's job is to stop probes into OTHER hosts' private
// networks (RFC1918, link-local, unspecified, ULA fc00::/7).
func resolvePublicHosts(host string) ([]net.IP, error) {
	host = strings.Trim(host, "[]")
	if host == "localhost" {
		// Deterministic pin (no resolver variance). Note: services bound only
		// to ::1 (IPv6 loopback) are not reachable via this rewrite.
		host = "127.0.0.1"
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("禁止访问无法解析的地址: %s", host)
	}
	for _, ip := range ips {
		if ip.IsLoopback() {
			continue
		}
		if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return nil, fmt.Errorf("禁止访问内网地址: %s", host)
		}
		if ip.IsPrivate() {
			return nil, fmt.Errorf("禁止访问内网地址: %s", host)
		}
		if ip.To4() == nil && (ip[0]&0xFE) == 0xFC { // ULA fc00::/7
			return nil, fmt.Errorf("禁止访问内网地址: %s", host)
		}
	}
	return ips, nil
}

func checkHostPublic(host string) error {
	_, err := resolvePublicHosts(host)
	return err
}

// pinHostToIP rewrites the host of URL-form DSNs (http, qdrant, redis) to a
// validated IP from the guard's resolution, closing the DNS-rebinding window
// between CheckDSNHost and the later dial (R3-3). postgres/mysql/mongo/https
// are left untouched: their drivers dial independently, and https certificate
// verification binds to the hostname (residual risk, documented).
func pinHostToIP(dsn string) (string, error) {
	lower := strings.ToLower(dsn)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "qdrant") &&
		!strings.HasPrefix(lower, "redis") {
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Hostname() == "" {
		return dsn, nil
	}
	ips, err := resolvePublicHosts(u.Hostname())
	if err != nil {
		return dsn, err
	}
	ip := pickIPv4(ips)
	if ip == nil || ip.String() == u.Hostname() {
		return dsn, nil // already an IP literal or no IPv4 to pin
	}
	host := ip.String()
	if u.Port() != "" {
		host = net.JoinHostPort(host, u.Port())
	}
	u.Host = host
	return u.String(), nil
}

// pickIPv4 prefers an IPv4 address; an IPv6-only host returns nil so the
// hostname stays untouched (a bare IPv6 in the URL would break url.Parse).
func pickIPv4(ips []net.IP) net.IP {
	for _, ip := range ips {
		if ip.To4() != nil {
			return ip
		}
	}
	return nil
}
