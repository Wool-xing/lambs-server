package handlers

import "testing"

func TestClassifyLevel(t *testing.T) {
	cases := map[string]string{
		"2026/08/17 22:39:28 procmgr.go:549: health: REST API真机测试 is down, restarting": "warn",
		"2026/08/17 10:00:00 main.go:1: failed to connect: dial tcp error":               "error",
		"2026/08/17 10:00:00 main.go:1: PANIC: nil pointer":                               "error",
		"2026/08/17 10:00:00 main.go:1: WARNING: deprecated flag":                         "warn",
		"2026/08/17 10:00:00 main.go:1: server listening on :3602":                        "info",
		"2026/08/17 10:00:00 main.go:1: 登录成功 user=admin":                                "info",
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
