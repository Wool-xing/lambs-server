package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"lambs-server-go/internal/db"
)

const logTailBytes = 8192

// Package-level so tests can point them at httptest servers.
var (
	agentURL   = defaultAgentURL()
	agentToken = os.Getenv("COMPUTE_AGENT_TOKEN")
)

func defaultAgentURL() string {
	if u := os.Getenv("COMPUTE_AGENT_URL"); u != "" {
		return u
	}
	return "" // 未配置 COMPUTE_AGENT_URL = windows 通道不可用（开源默认，不硬编码内网地址 R24）
}

// EnsureCronSchema creates the scheduled_tasks table (idempotent).
func EnsureCronSchema() {
	db.DB.Exec(`CREATE TABLE IF NOT EXISTS scheduled_tasks (
		id TEXT PRIMARY KEY,
		project_id TEXT NOT NULL,
		name TEXT NOT NULL,
		cron TEXT NOT NULL,
		command TEXT NOT NULL,
		host TEXT NOT NULL DEFAULT 'app1',
		enabled BOOLEAN NOT NULL DEFAULT true,
		created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		last_run_at TIMESTAMPTZ,
		last_status TEXT NOT NULL DEFAULT '',
		last_log TEXT NOT NULL DEFAULT ''
	)`)
}

// tailLog keeps the last logTailBytes bytes — full logs are not worth the
// storage; a task running every minute would fill the table fast.
func tailLog(s string) string {
	if len(s) <= logTailBytes {
		return s
	}
	return s[len(s)-logTailBytes:]
}

// runApp1Command executes a shell command on the Lambs host (bash -c).
func runApp1Command(cmd string, timeout time.Duration) (ok bool, out, status string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var buf bytes.Buffer
	c := exec.CommandContext(ctx, "bash", "-c", cmd)
	c.Stdout, c.Stderr = &buf, &buf
	err := c.Run()
	out = tailLog(buf.String())
	if ctx.Err() == context.DeadlineExceeded {
		return false, out, "timeout"
	}
	if err != nil {
		return false, out, "failed"
	}
	return true, out, "success"
}

// agentVersion fetches /health once and returns the agent version string.
// Best-effort: empty when unreachable or the field is absent — the version
// prefix must never cost a task run.
func agentVersion() string {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(agentURL + "/health")
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var h struct {
		Version string `json:"version"`
	}
	if json.NewDecoder(resp.Body).Decode(&h) != nil {
		return ""
	}
	return h.Version
}

// runWindowsCommand pushes the command to the Windows compute-agent
// (POST /cmd) and returns its output. Agent unreachable or command
// non-zero = failed. One retry on transport errors — Tailscale link
// blips are routine and must not burn a task run. The dialer gets a 10s
// timeout of its own: without it an unreachable agent holds the whole
// task for minutes in OS-level SYN retries.
func runWindowsCommand(cmd string, timeout time.Duration) (bool, string, string) {
	body, _ := json.Marshal(map[string]interface{}{"cmd": cmd, "timeout": int(timeout.Seconds())})
	client := http.Client{
		Timeout: timeout + 10*time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		},
	}
	var lastErr error
	ver := agentVersion() // best-effort, once per run
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest("POST", agentURL+"/cmd", bytes.NewReader(body))
		if err != nil {
			return false, "", "failed"
		}
		req.Header.Set("Content-Type", "application/json")
		if agentToken != "" {
			req.Header.Set("Authorization", "Bearer "+agentToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		defer resp.Body.Close()
		var res struct {
			OK     bool   `json:"ok"`
			Code   int    `json:"code"`
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
			Error  string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			return false, "bad agent response", "failed"
		}
		out := tailLog(res.Stdout + res.Stderr)
		if res.Error != "" {
			out = tailLog(out + " " + res.Error)
		}
		if ver != "" {
			out = "[agent v" + ver + "] " + out
		}
		if res.OK {
			return true, out, "success"
		}
		return false, out, "failed"
	}
	return false, "agent unreachable: " + lastErr.Error(), "failed"
}

// executeTask runs one scheduled task now and persists the outcome.
func executeTask(id, projectID, name, command, host string) {
	var ok bool
	var out, status string
	if host == "windows" {
		ok, out, status = runWindowsCommand(command, 10*time.Minute)
	} else {
		ok, out, status = runApp1Command(command, 10*time.Minute)
	}
	if _, err := db.DB.Exec("UPDATE scheduled_tasks SET last_run_at=NOW(), last_status=$1, last_log=$2 WHERE id=$3", status, out, id); err != nil {
		log.Printf("cron: %s update failed: %v", name, err)
	}
	log.Printf("cron: task %s (%s) finished: status=%s outlen=%d", name, host, status, len(out))
	if !ok {
		nid := fmt.Sprintf("n%d", time.Now().UnixNano())
		db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
			nid, projectID, "alert", "计划任务失败", fmt.Sprintf("「%s」执行%s。\n%s", name, map[string]string{"timeout": "超时", "failed": "失败"}[status], out))
	}
}

// StartTaskRun loads a task and runs it immediately (async).
func StartTaskRun(id string) error {
	var project, name, command, host string
	if err := db.DB.QueryRow("SELECT project_id, name, command, host FROM scheduled_tasks WHERE id=$1", id).Scan(&project, &name, &command, &host); err != nil {
		return fmt.Errorf("任务不存在")
	}
	go executeTask(id, project, name, command, host)
	return nil
}

// cronTickOnce is one scheduler pass — extracted so the tick logic is
// testable without the never-returning scheduler goroutine. lastFired is the
// per-task "already fired this minute" guard, owned by the caller.
func cronTickOnce(lastFired map[string]time.Time) {
	rows, err := db.DB.Query("SELECT id, project_id, name, command, host, cron FROM scheduled_tasks WHERE enabled")
	if err != nil {
		return
	}
	type t struct{ id, project, name, command, host, cron string }
	var due []t
	for rows.Next() {
		var e t
		rows.Scan(&e.id, &e.project, &e.name, &e.command, &e.host, &e.cron)
		due = append(due, e)
	}
	rows.Close()
	nowMin := time.Now().Truncate(time.Minute)
	for _, e := range due {
		spec, err := parseCron(e.cron)
		if err != nil || !spec.matches(nowMin) {
			continue
		}
		if lastFired[e.id] == nowMin {
			continue
		}
		lastFired[e.id] = nowMin
		go executeTask(e.id, e.project, e.name, e.command, e.host)
	}
}

// StartCronScheduler fires due tasks every 30s. A task fires when its cron
// matches the current minute and it has not fired in that minute yet.
func StartCronScheduler() {
	lastFired := map[string]time.Time{}
	go func() {
		for range time.Tick(30 * time.Second) {
			cronTickOnce(lastFired)
		}
	}()
}
