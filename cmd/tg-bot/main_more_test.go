package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// roundTripFunc adapts a function to http.RoundTripper so tests can intercept
// http.DefaultClient without touching production code.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// interceptTG swaps http.DefaultClient for one that records the outgoing
// request (path+query and form body) and answers with canned JSON. The TG API
// URL is hardcoded in production code, so the default client is the only
// injection point — no real api.telegram.org traffic.
func interceptTG(t *testing.T, canned string) func() (string, string) {
	t.Helper()
	old := http.DefaultClient
	var lastPath, lastBody string
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var b []byte
		if r.Body != nil { // GET requests (setWebhook/deleteWebhook) have no body
			b, _ = io.ReadAll(r.Body)
		}
		lastPath, lastBody = r.URL.Path+"?"+r.URL.RawQuery, string(b)
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(canned))}, nil
	})}
	t.Cleanup(func() { http.DefaultClient = old })
	return func() (string, string) { return lastPath, lastBody }
}

// TestTGAPIBranches — success, ok:false, garbage body, and transport failure.
func TestTGAPIBranches(t *testing.T) {
	oldToken := token
	token = "test-token"
	t.Cleanup(func() { token = oldToken })

	t.Run("success", func(t *testing.T) {
		record := interceptTG(t, `{"ok":true,"result":{"update_id":42}}`)
		res, err := tgAPI("getUpdates", url.Values{"offset": {"-1"}})
		if err != nil {
			t.Fatalf("tgAPI: %v", err)
		}
		if res["update_id"] != float64(42) {
			t.Errorf("result = %v, want update_id 42", res)
		}
		p, _ := record()
		if !strings.Contains(p, "/bottest-token/getUpdates") {
			t.Errorf("path = %q, want /bottest-token/getUpdates", p)
		}
	})

	t.Run("ok false", func(t *testing.T) {
		interceptTG(t, `{"ok":false}`)
		if _, err := tgAPI("getUpdates", url.Values{}); err == nil {
			t.Error("want error when ok=false")
		}
	})

	t.Run("garbage body", func(t *testing.T) {
		interceptTG(t, `not json`)
		if _, err := tgAPI("getUpdates", url.Values{}); err == nil {
			t.Error("want error on undecodable body")
		}
	})

	t.Run("dial error", func(t *testing.T) {
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("dial refused")
		})}
		t.Cleanup(func() { http.DefaultClient = old })
		if _, err := tgAPI("getUpdates", url.Values{}); err == nil {
			t.Error("want error when the transport fails")
		}
	})
}

// TestSendMessageBranches — trim, 4000-char truncation, and the request shape.
func TestSendMessageBranches(t *testing.T) {
	oldToken := token
	token = "test-token"
	t.Cleanup(func() { token = oldToken })
	record := interceptTG(t, `{"ok":true}`)

	sendMessage(1, "  hi  ")
	p, b := record()
	if !strings.Contains(p, "/bottest-token/sendMessage") {
		t.Errorf("path = %q", p)
	}
	if !strings.Contains(b, "chat_id=1") || !strings.Contains(b, "text=hi") {
		t.Errorf("body = %q, want trimmed text=hi", b)
	}

	sendMessage(1, strings.Repeat("x", 5000))
	_, b = record()
	if !strings.Contains(b, "text="+strings.Repeat("x", 4000)) {
		t.Error("long text not truncated to 4000 chars")
	}
}

// TestSetAndDeleteWebhook — both fire the expected TG API calls with the
// configured URL and secret (previously untestable: they dial the hardcoded
// api.telegram.org).
func TestSetAndDeleteWebhook(t *testing.T) {
	oldToken, oldURL, oldSecret := token, webhookURL, webhookSecret
	token, webhookURL, webhookSecret = "test-token", "https://example.com/hook", "secret123"
	t.Cleanup(func() { token, webhookURL, webhookSecret = oldToken, oldURL, oldSecret })
	record := interceptTG(t, `{"ok":true}`)

	setWebhook()
	p, _ := record()
	if !strings.Contains(p, "/bottest-token/setWebhook") ||
		!strings.Contains(p, "url=https://example.com/hook") ||
		!strings.Contains(p, "secret_token=secret123") {
		t.Errorf("setWebhook = %q", p)
	}

	deleteWebhook()
	p, _ = record()
	if !strings.Contains(p, "/bottest-token/deleteWebhook") {
		t.Errorf("deleteWebhook = %q", p)
	}
}

// TestRunSSHWithHost — the non-empty-host branch of runSSH (empty host is
// covered by TestRunAndRunSSH). Connects to a closed local port: ssh exits
// immediately with connection refused; if ssh is absent from PATH, run()
// degrades to ERR. Either way no real server is touched.
func TestRunSSHWithHost(t *testing.T) {
	out := runSSH("127.0.0.1:65530", "true")
	if out == "" {
		t.Error("runSSH with host returned empty output")
	}
}

// TestSvcStatusRemoteBranch — svcStatus with a host routes through bot.runSSH
// (TestSvcStatusIcons covers the local branch).
func TestSvcStatusRemoteBranch(t *testing.T) {
	old := bot
	bot = botOps{run: func(cmd string) string { return "" }, runSSH: func(host, cmd string) string { return "active" }}
	t.Cleanup(func() { bot = old })
	if out := svcStatus("10.0.0.2", "nginx", "Nginx"); !strings.Contains(out, "✅") {
		t.Errorf("remote active = %q", out)
	}
}

// TestCommandBranchesMore — the handleCommand branches not covered by
// TestCommandBranches(Remaining): output ceilings, the /dl charset gate, the
// /storage empty/limit paths, and /stop (which calls setWebhook — possible
// now via the intercepted default client).
func TestCommandBranchesMore(t *testing.T) {
	var sent []string
	oldBot := bot
	bot = botOps{
		run:    func(cmd string) string { return "RUN<" + cmd + ">" },
		runSSH: func(host, cmd string) string { return "SSH<" + host + ":" + cmd + ">" },
		send:   func(chatID int64, text string) { sent = append(sent, text) },
	}
	t.Cleanup(func() { bot = oldBot })
	adminChats[1] = true
	t.Cleanup(func() { delete(adminChats, 1) })
	record := interceptTG(t, `{"ok":true}`) // /stop calls setWebhook
	oldToken, oldURL, oldSecret := token, webhookURL, webhookSecret
	token, webhookURL, webhookSecret = "test-token", "https://example.com/hook", "secret123"
	t.Cleanup(func() { token, webhookURL, webhookSecret = oldToken, oldURL, oldSecret })

	run := func(text string) string {
		sent = nil
		handleCommand(1, text)
		if len(sent) == 0 {
			return ""
		}
		return sent[len(sent)-1]
	}

	t.Run("backup truncates at 3800", func(t *testing.T) {
		bot.run = func(cmd string) string { return strings.Repeat("b", 5000) }
		if out := run("/backup"); len(out) != 3800 {
			t.Errorf("/backup len = %d, want 3800", len(out))
		}
	})

	t.Run("logs truncates at 3500", func(t *testing.T) {
		bot.runSSH = func(host, cmd string) string { return strings.Repeat("l", 5000) }
		if out := run("/logs 50"); len(out) != 3500 {
			t.Errorf("/logs len = %d, want 3500", len(out))
		}
	})

	t.Run("dl rejects bad char after length gate", func(t *testing.T) {
		// len 9 passes the 8..128 gate, then the charset check rejects '!'.
		if out := run("/dl abcdefgh!"); !strings.Contains(out, "无效的文件ID") {
			t.Errorf("/dl bad-char = %q", out)
		}
	})

	t.Run("storage empty raw", func(t *testing.T) {
		bot.run = func(cmd string) string { return "empty" }
		if out := run("/storage"); !strings.Contains(out, "暂无存储文件") {
			t.Errorf("/storage empty = %q", out)
		}
	})

	t.Run("storage limit window", func(t *testing.T) {
		var buf strings.Builder
		for i := 0; i < 12; i++ {
			fmt.Fprintf(&buf, `{"ch":"files","name":"f%02d.db","size":100,"fid":"abcdefghij"}`+"\n", i)
		}
		bot.run = func(cmd string) string { return buf.String() }
		out := run("/storage")
		if !strings.Contains(out, "files (12)") || !strings.Contains(out, "f09.db") {
			t.Errorf("/storage limit = %q", out)
		}
		if strings.Contains(out, "f00.db") {
			t.Error("first file should be outside the last-10 window")
		}
	})

	t.Run("storage count arg and 4000 ceiling", func(t *testing.T) {
		// 100 items with 25-char names: the /storage N limit parse (N=100) and
		// the 4000-char output ceiling both execute.
		var buf strings.Builder
		for i := 0; i < 100; i++ {
			fmt.Fprintf(&buf, `{"ch":"files","name":"f%02d-%021s","size":100,"fid":"abcdefghij"}`+"\n", i, "")
		}
		bot.run = func(cmd string) string { return buf.String() }
		out := run("/storage 100")
		if len(out) != 4000 {
			t.Errorf("/storage 100 len = %d, want 4000 (truncated)", len(out))
		}
		if !strings.Contains(out, "f10-") {
			t.Errorf("/storage 100 = %q, want early items present", out)
		}
	})

	t.Run("stop sleeps and re-enables webhook", func(t *testing.T) {
		if out := run("/stop"); !strings.Contains(out, "已休眠") {
			t.Errorf("/stop = %q", out)
		}
		p, _ := record()
		if !strings.Contains(p, "setWebhook") {
			t.Errorf("/stop did not call setWebhook, last request = %q", p)
		}
	})
}

// TestProcessUpdatesMore — updates whose message lacks a chat, or whose chat
// lacks an id, are skipped without dispatching (TestProcessUpdates covers the
// dispatch path).
func TestProcessUpdatesMore(t *testing.T) {
	adminChats = map[int64]bool{}
	updates := []interface{}{
		map[string]interface{}{"update_id": float64(201), "message": map[string]interface{}{"text": "/status"}},
		map[string]interface{}{"update_id": float64(202), "message": map[string]interface{}{"chat": map[string]interface{}{}, "text": "/status"}},
		map[string]interface{}{"update_id": float64(203), "message": map[string]interface{}{"chat": map[string]interface{}{"id": float64(7)}, "text": ""}},
	}
	hasMsg, latest := processUpdates(updates, 200)
	if hasMsg {
		t.Error("hasMsg = true, want false (nothing dispatchable)")
	}
	if latest != 203 {
		t.Errorf("latest = %d, want 203", latest)
	}
}
