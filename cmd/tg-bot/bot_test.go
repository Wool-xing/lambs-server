package main

import "testing"

// cmd/tg-bot 0% 破冰（QA 第 5 轮想法 2）：先测纯函数，命令处理抽离
// 是后续工作。

func TestMemLine(t *testing.T) {
	// free 输出形状：total used free shared buff/cache available
	raw := "Mem: 16383900 8234000 5123000 123000 3028900 7156000"
	got := memLine(raw)
	want := "Mem: 8234000/16383900  avail 7156000"
	if got != want {
		t.Errorf("memLine = %q, want %q", got, want)
	}
	if memLine("short") != "short" {
		t.Error("memLine should pass through short lines")
	}
}

func TestRedact(t *testing.T) {
	app1, app2 = "10.0.0.1", "10.0.0.2" // package vars used by redact
	token = "secret-token"
	in := "token secret-token on 10.0.0.1 and 10.0.0.2"
	got := redact(in)
	if got != "token [TOKEN] on [IP] and [IP]" {
		t.Errorf("redact = %q", got)
	}
}

func TestFmtSize(t *testing.T) {
	cases := map[int64]string{
		0:           "0B",
		1023:        "1023B",
		2048:        "2.0KB",
		5 * 1024 * 1024: "5.0MB",
	}
	for in, want := range cases {
		if got := fmtSize(in); got != want {
			t.Errorf("fmtSize(%d) = %q, want %q", in, got, want)
		}
	}
}
