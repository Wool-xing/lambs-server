package auth

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// loginLimiter is a per-IP sliding-window limiter for credential endpoints.
// nginx already rate-limits /lambs/api/auth/login — this is the second layer
// for direct :3602 access.
var loginLimiter = struct {
	sync.Mutex
	hits map[string][]time.Time
}{hits: make(map[string][]time.Time)}

// clientIP prefers the X-Forwarded-For set by nginx over the socket address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return xff
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RateLimit allows max requests per window per IP; beyond that it returns 429.
func RateLimit(max int, window time.Duration) func(http.HandlerFunc) http.HandlerFunc {
	return func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			ip := clientIP(r)
			now := time.Now()
			loginLimiter.Lock()
			// Rotating spoofed XFF headers would grow the map without bound —
			// sweep stale keys once past a threshold.
			if len(loginLimiter.hits) > 10000 {
				for k, times := range loginLimiter.hits {
					if len(times) == 0 || now.Sub(times[len(times)-1]) > window {
						delete(loginLimiter.hits, k)
					}
				}
			}
			kept := loginLimiter.hits[ip][:0]
			for _, t := range loginLimiter.hits[ip] {
				if now.Sub(t) < window {
					kept = append(kept, t)
				}
			}
			if len(kept) >= max {
				loginLimiter.hits[ip] = kept
				loginLimiter.Unlock()
				JSONErr(w, 429, "请求过于频繁，请稍后再试")
				return
			}
			loginLimiter.hits[ip] = append(kept, now)
			loginLimiter.Unlock()
			next(w, r)
		}
	}
}
