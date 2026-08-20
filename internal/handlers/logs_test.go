package handlers

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClassifyLevel(t *testing.T) {
	cases := map[string]string{
		"2026/08/17 22:39:28 procmgr.go:549: health: REST API真机测试 is down, restarting": "warn",
		"2026/08/17 10:00:00 main.go:1: failed to connect: dial tcp error":             "error",
		"2026/08/17 10:00:00 main.go:1: PANIC: nil pointer":                            "error",
		"2026/08/17 10:00:00 main.go:1: WARNING: deprecated flag":                      "warn",
		"2026/08/17 10:00:00 main.go:1: server listening on :3602":                     "info",
		"2026/08/17 10:00:00 main.go:1: 登录成功 user=admin":                               "info",
	}
	for in, want := range cases {
		if got := classifyLevel(in); got != want {
			t.Errorf("classifyLevel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseJournalLine(t *testing.T) {
	line := "2026-08-17T22:39:28+08:00 Lambs lambs-server[2868301]: 2026/08/17 22:39:28 procmgr.go:549: health: test is down"
	got := parseJournalLine(line)
	if got["time"] != "2026-08-17T22:39:28+08:00" {
		t.Errorf("time = %v", got["time"])
	}
	if got["level"] != "warn" {
		t.Errorf("level = %v, want warn", got["level"])
	}
	wantMsg := "2026/08/17 22:39:28 procmgr.go:549: health: test is down"
	if got["message"] != wantMsg {
		t.Errorf("message = %v, want %v", got["message"], wantMsg)
	}
}

func TestParseJournalLineNoPrefix(t *testing.T) {
	// Lines without the journald prefix (raw output) pass through untouched.
	got := parseJournalLine("just a plain log line")
	if got["message"] != "just a plain log line" {
		t.Errorf("message = %v", got["message"])
	}
	if got["level"] != "info" {
		t.Errorf("level = %v, want info", got["level"])
	}
}

// TestHandleSystemLogsGate — non-super_admin gets 403 before any command
// runs; on hosts without journalctl the handler degrades to an empty list
// instead of dying (cross-platform).
func TestHandleSystemLogsGate(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/logs/system", nil)
	r.Header.Set("X-User-ID", "v1")
	r.Header.Set("X-Role", "viewer")
	w := httptest.NewRecorder()
	HandleSystemLogs(w, r)
	if w.Code != 403 {
		t.Errorf("viewer = %d, want 403", w.Code)
	}

	sa := httptest.NewRequest("GET", "/api/logs/system?lines=10", nil)
	sa.Header.Set("X-User-ID", "admin")
	sa.Header.Set("X-Role", "super_admin")
	sw := httptest.NewRecorder()
	HandleSystemLogs(sw, sa)
	if sw.Code != 200 {
		t.Errorf("super_admin = %d, want 200 (body %s)", sw.Code, sw.Body.String())
	}
	// journalctl may be absent (Windows dev) — the contract is the JSONOK
	// envelope with a list payload, never a 500.
	b := sw.Body.String()
	if len(b) < 10 || !strings.Contains(b, "success") {
		t.Errorf("body unexpected: %q", b)
	}
}
