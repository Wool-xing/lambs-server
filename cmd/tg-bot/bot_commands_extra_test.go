package main

import (
	"strings"
	"testing"
)

// TestCommandBranchesRemaining — the command surface not covered by
// TestCommandBranches: authz gate, every /restart target, /logs bounds,
// /dl validation, /storage parsing, /ssh, and the run/runSSH/svcStatus
// helpers. /stop is deliberately skipped: it calls setWebhook, which dials
// api.telegram.org with no injection point.
func TestCommandBranchesRemaining(t *testing.T) {
	var sent []string
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

	t.Run("unauthorized chat refused", func(t *testing.T) {
		sent = nil
		handleCommand(999, "/status")
		if len(sent) != 1 || !strings.Contains(sent[0], "未授权") {
			t.Errorf("unauthorized sent = %v", sent)
		}
	})

	t.Run("restart targets", func(t *testing.T) {
		for _, c := range []struct{ cmd, want string }{
			{"/restart pg", "PG:"},
			{"/restart postgresql", "PG:"},
			{"/restart lambs", "Lambs:"},
			{"/restart qa", "QA managed"},
			{"/restart redis", "Redis:"},
			{"/restart", "可用:"}, // no-arg → usage
		} {
			if out := run(c.cmd); !strings.Contains(out, c.want) {
				t.Errorf("%s = %q, want containing %q", c.cmd, out, c.want)
			}
		}
	})

	t.Run("logs bounds", func(t *testing.T) {
		if out := run("/logs"); !strings.Contains(out, "tail -n 10") {
			t.Errorf("/logs default = %q, want tail -n 10", out)
		}
		for _, bad := range []string{"/logs 0", "/logs -5", "/logs 999", "/logs abc"} {
			if out := run(bad); !strings.Contains(out, "tail -n 10") {
				t.Errorf("%s = %q, want fallback tail -n 10", bad, out)
			}
		}
	})

	t.Run("dl validation", func(t *testing.T) {
		if out := run("/dl"); !strings.Contains(out, "用法") {
			t.Errorf("/dl = %q, want usage", out)
		}
		for _, bad := range []string{"/dl short", "/dl abc!def", "/dl " + strings.Repeat("a", 129)} {
			if out := run(bad); !strings.Contains(out, "无效的文件ID") {
				t.Errorf("%s = %q, want invalid-file rejection", bad[:14], out)
			}
		}
		if out := run("/dl abcdefgh"); !strings.Contains(out, "RUN</opt/wool-tools/tg-upload.py -d abcdefgh") {
			t.Errorf("/dl valid = %q", out)
		}
	})

	t.Run("storage branches", func(t *testing.T) {
		// Unparseable raw degrades to an empty summary (the raw never reaches
		// the message — /storage sends the parsed result only).
		if out := run("/storage"); !strings.Contains(out, "total: 0 files") {
			t.Errorf("/storage unparseable = %q, want empty summary", out)
		}
		bot.run = func(cmd string) string { return "empty" }
		if out := run("/storage"); !strings.Contains(out, "暂无存储文件") {
			t.Errorf("/storage empty = %q", out)
		}
		bot.run = func(cmd string) string {
			return "{\"ch\":\"files\",\"name\":\"averyveryverylongbackupfilename.db\",\"size\":2048,\"fid\":\"abcdefgh\"}\n" +
				"not json\n" +
				"{\"ch\":\"files\",\"name\":\"b.db\",\"size\":1.5,\"fid\":\"ijklmnop\"}"
		}
		out := run("/storage")
		if !strings.Contains(out, "files (2)") || !strings.Contains(out, "total: 2 files") ||
			!strings.Contains(out, "abcdefgh") || !strings.Contains(out, "ijklmnop") {
			t.Errorf("/storage parsed = %q", out)
		}
		if !strings.Contains(out, "averyveryverylongbackupf") {
			t.Errorf("/storage name not truncated: %q", out)
		}
	})

	t.Run("ssh", func(t *testing.T) {
		// Restore the outer stub — the storage subtest replaced bot.run.
		bot.run = func(cmd string) string { return "RUN<" + cmd + ">" }
		if out := run("/ssh"); !strings.Contains(out, "App1: RUN<hostname>") || !strings.Contains(out, "App1→Web1 SSH: SSH<") {
			t.Errorf("/ssh = %q", out)
		}
	})
}

// TestRunAndRunSSH — real exec helpers: output trimmed, command failure
// with empty output degrades to ERR, empty host refuses.
func TestRunAndRunSSH(t *testing.T) {
	if out := run("echo  hi  "); out != "hi" {
		t.Errorf("run(echo) = %q, want trimmed %q", out, "hi")
	}
	if out := run("exit 1"); out != "ERR" {
		t.Errorf("run(exit 1) = %q, want ERR", out)
	}
	if out := runSSH("", "x"); !strings.Contains(out, "未配置 TG_WEB1_IP") {
		t.Errorf("runSSH empty host = %q", out)
	}
}

// TestSvcStatusIcons — active maps to ✅, anything else to ❌.
func TestSvcStatusIcons(t *testing.T) {
	old := bot
	bot = botOps{run: func(cmd string) string { return "active" }}
	defer func() { bot = old }()
	if out := svcStatus("", "x", "X"); !strings.Contains(out, "✅") {
		t.Errorf("active = %q", out)
	}
	bot.run = func(cmd string) string { return "inactive" }
	if out := svcStatus("", "x", "X"); !strings.Contains(out, "❌") {
		t.Errorf("inactive = %q", out)
	}
}
