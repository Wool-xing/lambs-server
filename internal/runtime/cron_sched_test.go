package runtime

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRunApp1CommandOK(t *testing.T) {
	ok, out, status := runApp1Command("echo hello-lambs", 30*time.Second)
	if !ok || status != "success" {
		t.Fatalf("ok=%v status=%s out=%q, want success", ok, status, out)
	}
	if !strings.Contains(out, "hello-lambs") {
		t.Errorf("out = %q, want to contain hello-lambs", out)
	}
}

func TestRunApp1CommandFail(t *testing.T) {
	ok, _, status := runApp1Command("exit 3", 30*time.Second)
	if ok || status != "failed" {
		t.Fatalf("ok=%v status=%s, want failed", ok, status)
	}
}

func TestRunApp1CommandTimeout(t *testing.T) {
	ok, _, status := runApp1Command("sleep 5", 1*time.Second)
	if ok || status != "timeout" {
		t.Fatalf("ok=%v status=%s, want timeout", ok, status)
	}
}

func TestTailLog(t *testing.T) {
	long := strings.Repeat("x", 20000)
	got := tailLog(long)
	if len(got) != logTailBytes {
		t.Fatalf("len = %d, want %d", len(got), logTailBytes)
	}
	if !strings.HasSuffix(got, "x") {
		t.Error("tail must keep the END of the log")
	}
	short := "abc"
	if tailLog(short) != short {
		t.Error("short log must pass through")
	}
}

func TestRunWindowsCommandOK(t *testing.T) {
	var gotToken, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cmd" {
			http.NotFound(w, r)
			return
		}
		gotToken = r.Header.Get("Authorization")
		var body map[string]interface{}
		json.NewDecoder(r.Body).Decode(&body)
		b, _ := json.Marshal(body)
		gotBody = string(b)
		w.Write([]byte(`{"ok":true,"code":0,"stdout":"scan done","stderr":"","elapsed":1.2}`))
	}))
	defer srv.Close()

	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = srv.URL, "test-secret"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	ok, out, status := runWindowsCommand("python main.py --target 127.0.0.1", 60*time.Second)
	if !ok || status != "success" {
		t.Fatalf("ok=%v status=%s out=%q", ok, status, out)
	}
	if !strings.Contains(out, "scan done") {
		t.Errorf("out = %q", out)
	}
	if gotToken != "Bearer test-secret" {
		t.Errorf("token = %q", gotToken)
	}
	if !strings.Contains(gotBody, `"cmd"`) || !strings.Contains(gotBody, "python main.py") {
		t.Errorf("body = %q", gotBody)
	}
}

func TestRunWindowsCommandPrependsAgentVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"hostname":"LAPTOP","status":"ok","version":"2.0.1"}`))
			return
		}
		w.Write([]byte(`{"ok":true,"code":0,"stdout":"scan done","stderr":"","elapsed":1.2}`))
	}))
	defer srv.Close()

	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = srv.URL, "t"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	ok, out, status := runWindowsCommand("python main.py", 60*time.Second)
	if !ok || status != "success" {
		t.Fatalf("ok=%v status=%s", ok, status)
	}
	if !strings.Contains(out, "[agent v2.0.1]") {
		t.Errorf("out missing agent version prefix: %q", out)
	}
	if !strings.Contains(out, "scan done") {
		t.Errorf("out missing command output: %q", out)
	}
}

func TestRunWindowsCommandAgentError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":false,"code":1,"stdout":"","stderr":"module not found","elapsed":0.4}`))
	}))
	defer srv.Close()

	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = srv.URL, "t"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	ok, out, status := runWindowsCommand("bad command", 60*time.Second)
	if ok || status != "failed" {
		t.Fatalf("ok=%v status=%s", ok, status)
	}
	if !strings.Contains(out, "module not found") {
		t.Errorf("out = %q", out)
	}
}

func TestRunWindowsCommandUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	oldURL, oldTok := agentURL, agentToken
	agentURL, agentToken = url, "t"
	defer func() { agentURL, agentToken = oldURL, oldTok }()

	ok, _, status := runWindowsCommand("cmd", 5*time.Second)
	if ok || status != "failed" {
		t.Fatalf("ok=%v status=%s, want failed for unreachable agent", ok, status)
	}
}
