package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestClientIPBranches — the spoofing-defense matrix: XFF is honored only
// from trusted peers and takes the right-most entry; anything else falls
// back to the remote address (ratelimit.go was 65.0%).
func TestClientIPBranches(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		r := httptest.NewRequest("POST", "/api/auth/login", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"plain ip:port", "203.0.113.7:5555", "", "203.0.113.7"},
		{"trusted loopback takes right-most xff", "127.0.0.1:5555", "198.51.100.1, 198.51.100.2", "198.51.100.2"},
		{"untrusted peer ignores xff", "203.0.113.9:5555", "198.51.100.9", "203.0.113.9"},
		{"no port", "203.0.113.11", "", "203.0.113.11"},
		{"trusted v6 loopback", "[::1]:5555", "198.51.100.3", "198.51.100.3"},
		{"empty xff falls back", "127.0.0.1:5555", "", "127.0.0.1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clientIP(mk(c.remote, c.xff)); got != c.want {
				t.Errorf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRateLimitWindow — max requests pass, the next one is 429, and the
// window expiry lets a request through again.
func TestRateLimitWindow(t *testing.T) {
	loginLimiter.Lock()
	loginLimiter.hits = make(map[string][]time.Time)
	loginLimiter.Unlock()

	limit := RateLimit(2, 50*time.Millisecond)
	h := limit(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	call := func() int {
		r := httptest.NewRequest("POST", "/x", nil)
		r.RemoteAddr = "198.51.100.42:1"
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code
	}
	if c1 := call(); c1 != 200 {
		t.Fatalf("first request = %d, want 200", c1)
	}
	if c2 := call(); c2 != 200 {
		t.Fatalf("second request = %d, want 200", c2)
	}
	if call() != 429 {
		t.Fatal("third request must be 429")
	}
	time.Sleep(60 * time.Millisecond)
	if call() != 200 {
		t.Fatal("request after window expiry must pass")
	}
}
