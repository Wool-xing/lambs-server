package handlers

import "testing"

func TestValidateTaskInput(t *testing.T) {
	// valid
	if err := validateTaskInput("每日扫描", "0 2 * * *", "echo hi", "app1"); err != nil {
		t.Errorf("valid input rejected: %v", err)
	}
	if err := validateTaskInput("win", "*/5 * * * *", "python main.py", "windows"); err != nil {
		t.Errorf("valid windows input rejected: %v", err)
	}
	// invalid
	cases := []struct{ name, cron, command, host string }{
		{"", "0 2 * * *", "echo", "app1"},
		{"ok", "bad cron", "echo", "app1"},
		{"ok", "60 * * * *", "echo", "app1"},
		{"ok", "0 2 * * *", "", "app1"},
		{"ok", "0 2 * * *", "echo", "mac"},
		{"ok", "0 2 * * * *", "echo", "app1"},
	}
	for _, c := range cases {
		if err := validateTaskInput(c.name, c.cron, c.command, c.host); err == nil {
			t.Errorf("validateTaskInput(%q,%q,%q,%q) = nil, want error", c.name, c.cron, c.command, c.host)
		}
	}
}
