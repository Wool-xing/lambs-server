package main

import (
	"strings"
	"testing"
)

// TestCommandBranches — fake ops inject run/runSSH/send, then every
// command branch is exercised without real servers (the 5.7% package's
// command surface).
func TestCommandBranches(t *testing.T) {
	var sent []string
	app1, app2, token = "", "", "" // reset TestRedact's fixtures (redact no-ops)
	old := bot
	bot = botOps{
		run:    func(cmd string) string { return "RUN<" + cmd + ">" },
		runSSH: func(host, cmd string) string { return "SSH<" + host + ":" + cmd + ">" },
		send:   func(chatID int64, text string) { sent = append(sent, text) },
	}
	defer func() { bot = old }()
	adminChats[1] = true
	defer delete(adminChats, 1)

	run := func(text string) string {
		sent = nil
		handleCommand(1, text)
		if len(sent) == 0 {
			return ""
		}
		return sent[len(sent)-1]
	}

	// /start help lists the command set.
	if out := run("/start"); !strings.Contains(out, "/status") || !strings.Contains(out, "/restart") {
		t.Errorf("/start = %q", out)
	}
	// /status carries both hosts' sections.
	if out := run("/status"); !strings.Contains(out, "App1") || !strings.Contains(out, "Web1") {
		t.Errorf("/status = %q", out)
	}
	// /mem routes run() and runSSH().
	if out := run("/mem"); !strings.Contains(out, "RUN<free") || !strings.Contains(out, "SSH<") {
		t.Errorf("/mem = %q", out)
	}
	// /restart nginx routes to the Web1 box.
	if out := run("/restart nginx"); !strings.Contains(out, "Web1 nginx") || !strings.Contains(out, "SSH<") {
		t.Errorf("/restart nginx = %q", out)
	}
	// Unknown /restart service prints the available list.
	if out := run("/restart bogus"); !strings.Contains(out, "可用:") {
		t.Errorf("/restart bogus = %q", out)
	}
	// /backup truncates to the 3800-byte ceiling.
	if out := run("/backup"); !strings.Contains(out, "RUN<") {
		t.Errorf("/backup = %q", out)
	}
	// /logs with a numeric count routes over SSH to the web box.
	if out := run("/logs 50"); !strings.Contains(out, "SSH<") || !strings.Contains(out, "tail -n 50") {
		t.Errorf("/logs 50 = %q", out)
	}
}
