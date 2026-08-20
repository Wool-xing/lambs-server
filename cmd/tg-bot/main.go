// TG Manager Bot — Go rewrite (replaces tg-bot.py)
// Memory: ~5MB vs Python 30MB. Same behavior, same commands.
//
// Build: GOOS=linux GOARCH=amd64 go build -o tg-bot .
// Deploy: scp tg-bot → App1:/usr/local/bin/tg-bot
//          sudo systemctl restart tg-bot
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Server IPs — deployment-specific, must come from the environment.
// No hardcoded defaults: a wrong default would run commands on the wrong host.
var (
	app1          = os.Getenv("TG_APP1_IP")
	app2          = os.Getenv("TG_WEB1_IP")
	token         string
	webhookSecret string
	webhookURL    = os.Getenv("TG_WEBHOOK_URL")
)

// adminChats is the allowlist of Telegram chat IDs permitted to run commands
// (comma-separated TG_ADMIN_CHAT_IDS). Empty = deny all (secure default).
var adminChats = map[int64]bool{}

func init() {
	// 开源默认不硬编码部署域名 — webhook 走 TG_WEBHOOK_URL 配置。
	for _, s := range strings.Split(os.Getenv("TG_ADMIN_CHAT_IDS"), ",") {
		if id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil && id > 0 {
			adminChats[id] = true
		}
	}
}

func loadSecrets() {
	data, err := os.ReadFile("/opt/wool-tools/.tg-secrets")
	if err != nil {
		log.Fatal("cannot read secrets:", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		kv := strings.SplitN(line, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "TG_BOT_TOKEN":
			token = kv[1]
		case "TG_WEBHOOK_SECRET":
			webhookSecret = kv[1]
		}
	}
	if token == "" {
		log.Fatal("TG_BOT_TOKEN not set")
	}
	if webhookSecret == "" {
		log.Fatal("TG_WEBHOOK_SECRET not set")
	}
}

// ── TG API helpers ─────────────────────────────────────

func tgAPI(method string, params url.Values) (map[string]interface{}, error) {
	u := fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method)
	resp, err := http.PostForm(u, params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result struct {
		OK     bool                   `json:"ok"`
		Result map[string]interface{} `json:"result"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.OK {
		return nil, fmt.Errorf("TG API: not ok")
	}
	return result.Result, nil
}

// botOps is the side-effect surface of the command handlers — injected in
// tests so every /command branch is testable without real servers.
type botOps struct {
	run    func(cmd string) string
	runSSH func(host, cmd string) string
	send   func(chatID int64, text string)
}

var bot = botOps{run: run, runSSH: runSSH, send: sendMessage}

func sendMessage(chatID int64, text string) {
	text = strings.TrimSpace(text)
	if len(text) > 4000 {
		text = text[:4000]
	}
	tgAPI("sendMessage", url.Values{
		"chat_id": {fmt.Sprintf("%d", chatID)},
		"text":    {text},
	})
}

func setWebhook() {
	http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook?url=%s&secret_token=%s",
		token, webhookURL, webhookSecret))
}

func deleteWebhook() {
	http.Get(fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", token))
}

// ── Shell helpers ──────────────────────────────────────

func run(cmd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", cmd)
	out, err := c.CombinedOutput()
	if err != nil && len(out) == 0 {
		return "ERR"
	}
	return strings.TrimSpace(string(out))
}

func runSSH(host, cmd string) string {
	if host == "" {
		return "未配置 TG_WEB1_IP"
	}
	return run(fmt.Sprintf("ssh -o StrictHostKeyChecking=no -o ConnectTimeout=5 %s %s", host, cmd))
}

func memLine(raw string) string {
	f := strings.Fields(raw)
	if len(f) > 6 {
		return fmt.Sprintf("Mem: %s/%s  avail %s", f[2], f[1], f[6])
	}
	return raw
}

func svcStatus(host, name, label string) string {
	var s string
	if host == "" {
		s = bot.run(fmt.Sprintf("systemctl is-active %s 2>/dev/null || echo inactive", name))
	} else {
		s = bot.runSSH(host, fmt.Sprintf("systemctl is-active %s 2>/dev/null || echo inactive", name))
	}
	icon := "❌"
	if s == "active" {
		icon = "✅"
	}
	return fmt.Sprintf("  %-14s %s", label, icon)
}

func redact(s string) string {
	// 空配置跳过替换 — 空串 ReplaceAll 会在每个字符间插入标记 (QA 第 7 批)。
	if token != "" {
		s = strings.ReplaceAll(s, token, "[TOKEN]")
	}
	for _, ip := range []string{app1, app2} {
		if ip != "" {
			s = strings.ReplaceAll(s, ip, "[IP]")
		}
	}
	return s
}

func fmtSize(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%dB", b)
	case b < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(b)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(b)/1024/1024)
	}
}

// ── Command handlers ────────────────────────────────────

func handleCommand(chatID int64, text string) {
	// Authorization gate: only allowlisted chat IDs may run anything. Without
	// this, anyone who finds the bot could restart services or pull files.
	if !adminChats[chatID] {
		bot.send(chatID, "未授权的会话。请在 TG_ADMIN_CHAT_IDS 中配置此 chat_id 后重试。")
		return
	}
	switch {
	case text == "/start" || text == "/help":
		bot.send(chatID, "🐑 Lambs 服务器管理\n\n"+
			"/status    所有服务状态\n"+
			"/mem       内存占用 Top5\n"+
			"/backup    执行备份\n"+
			"/ssh       测试内网连通\n"+
			"/logs N    Nginx日志 (默认10)\n"+
			"/restart   nginx|pg|redis|lambs|qa\n"+
			"/stop      休眠 (发消息唤醒)")

	case text == "/status":
		// App1 (local) — Lambs server
		wm := memLine(bot.run("free -h | grep Mem"))
		ws := strings.Join([]string{
			svcStatus("", "lambs-server", "Lambs"),
			svcStatus("", "tg-bot", "TG Bot"),
			svcStatus("", "postgresql@16-main", "PostgreSQL"),
			svcStatus("", "redis-server", "Redis"),
			svcStatus("", "fail2ban", "Fail2ban"),
		}, "\n")
		// Web1 (remote) — Nginx
		am := memLine(bot.runSSH(app2, "free -h | grep Mem"))
		as := strings.Join([]string{
			svcStatus(app2, "nginx", "Nginx"),
			svcStatus(app2, "nginx", "Nginx"),
			svcStatus(app2, "fail2ban", "Fail2ban"),
		}, "\n")
		bot.send(chatID, fmt.Sprintf("App1 Lambs\n%s\n%s\n\nWeb1 Wool\n%s\n%s", wm, ws, am, as))

	case text == "/backup":
		out := bot.run("/opt/wool-tools/backup-lambs.sh 2>&1")
		if len(out) > 3800 {
			out = out[:3800]
		}
		bot.send(chatID, out)

	case text == "/mem":
		wm := memLine(bot.run("free -h | grep Mem"))
		wp := bot.run("ps aux --sort=-%mem | head -6")
		am := memLine(bot.runSSH(app2, "free -h | grep Mem"))
		ap := bot.runSSH(app2, "ps aux --sort=-%mem | head -6")
		bot.send(chatID, fmt.Sprintf("App1 Lambs\n%s\n%s\n\nWeb1 Wool\n%s\n%s", wm, wp, am, ap))

	case text == "/ssh":
		r1 := bot.run("hostname")
		r2 := bot.runSSH(app2, "hostname")
		bot.send(chatID, fmt.Sprintf("App1: %s\nApp1→Web1 SSH: %s", r1, r2))

	case strings.HasPrefix(text, "/restart"):
		parts := strings.Fields(text)
		svc := ""
		if len(parts) > 1 {
			svc = parts[1]
		}
		switch svc {
		case "nginx":
			r := bot.runSSH(app2, "sudo systemctl restart nginx; systemctl is-active nginx")
			bot.send(chatID, "Web1 nginx: "+r)
		case "pg", "postgresql":
			r := bot.run("sudo systemctl restart postgresql@16-main 2>&1; systemctl is-active postgresql@16-main")
			bot.send(chatID, "PG: "+r)
		case "lambs":
			r := bot.run("sudo systemctl restart lambs-server 2>&1; systemctl is-active lambs-server")
			bot.send(chatID, "Lambs: "+r)
		case "qa":
			r := bot.run("sudo systemctl restart lambs-server 2>&1; systemctl is-active lambs-server")
			bot.send(chatID, "Lambs (QA managed): "+r)
		case "redis":
			r := bot.run("sudo systemctl restart redis-server 2>&1; systemctl is-active redis-server")
			bot.send(chatID, "Redis: "+r)
		default:
			bot.send(chatID, "可用: nginx(Web1) | pg lambs redis(App1)")
		}

	case strings.HasPrefix(text, "/logs"):
		parts := strings.Fields(text)
		count := 10
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil && n > 0 && n <= 500 {
				count = n
			}
		}
		logOut := redact(bot.runSSH(app2,
			fmt.Sprintf("tail -n %d /var/log/nginx/error.log 2>/dev/null || echo no errors", count)))
		if len(logOut) > 3500 {
			logOut = logOut[:3500]
		}
		bot.send(chatID, logOut)

	case strings.HasPrefix(text, "/dl"):
		parts := strings.Fields(text)
		if len(parts) > 1 {
			fid := parts[1]
			// Validate: TG file IDs are alphanumeric/underscore/dash only
			if len(fid) < 8 || len(fid) > 128 {
				bot.send(chatID, "无效的文件ID")
			} else if strings.IndexFunc(fid, func(r rune) bool { return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') }) >= 0 {
				bot.send(chatID, "无效的文件ID")
			} else {
				bot.send(chatID, bot.run(fmt.Sprintf("/opt/wool-tools/tg-upload.py -d %s -o /tmp/dl-%s 2>&1", fid, fid)))
			}
		} else {
			bot.send(chatID, "用法: /dl <文件ID>")
		}

	case strings.HasPrefix(text, "/storage"):
		raw := bot.run("cat /opt/wool-tools/upload-log.jsonl 2>/dev/null || echo empty")
		if raw == "empty" {
			bot.send(chatID, "暂无存储文件")
			return
		}
		parts := strings.Fields(text)
		limit := 10
		if len(parts) > 1 {
			if n, err := strconv.Atoi(parts[1]); err == nil {
				limit = n
			}
		}
		result := map[string][]map[string]interface{}{}
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var d map[string]interface{}
			if json.Unmarshal([]byte(line), &d) != nil {
				continue
			}
			ch := "?"
			if v, ok := d["ch"]; ok {
				ch = fmt.Sprintf("%v", v)
			}
			result[ch] = append(result[ch], d)
		}
		out := ""
		totalF, totalS := 0, int64(0)
		for _, ch := range []string{"files", "backup", "logs", "cold"} {
			items := result[ch]
			if len(items) == 0 {
				continue
			}
			totalF += len(items)
			for _, item := range items {
				if sz, ok := item["size"].(float64); ok {
					totalS += int64(sz)
				}
			}
			out += fmt.Sprintf("\n%s (%d)\n", ch, len(items))
			start := 0
			if len(items) > limit {
				start = len(items) - limit
			}
			for _, item := range items[start:] {
				nm := ""
				if v, ok := item["name"]; ok {
					nm = fmt.Sprintf("%v", v)
				}
				if len(nm) > 25 {
					nm = nm[:25]
				}
				var sz int64
				if v, ok := item["size"].(float64); ok {
					sz = int64(v)
				}
				fid := ""
				if v, ok := item["fid"]; ok {
					fid = fmt.Sprintf("%v", v)
				}
				out += fmt.Sprintf("  %-25s %7s  /dl %s\n", nm, fmtSize(sz), fid)
			}
		}
		out += fmt.Sprintf("\ntotal: %d files %s", totalF, fmtSize(totalS))
		bot.send(chatID, out[:4000])

	case text == "/stop":
		bot.send(chatID, "已休眠，发消息自动唤醒")
		setWebhook()
	}
}


// wakeCh signals the bot to resume polling from sleep mode.
var wakeCh = make(chan struct{}, 1)

// webhookServer starts an HTTP server on :3601. When TG sends
// a webhook update (via nginx proxy), it wakes the bot.
func webhookServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		received := r.Header.Get("X-Telegram-Bot-Api-Secret-Token")
		// Constant-time compare (secret missing is fatal at startup, so a
		// length mismatch alone already fails the comparison).
		if subtle.ConstantTimeCompare([]byte(received), []byte(webhookSecret)) != 1 {
			log.Printf("webhook: 403 bad secret from %s", r.RemoteAddr)
			w.WriteHeader(403)
			w.Write([]byte(`{"ok":false}`))
			return
		}
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
		select {
		case wakeCh <- struct{}{}:
		default:
		}
	})
	log.Println("webhook: listening on :3601 (sleep mode)")
	// Port-in-use must not leave wake-ups silently dead (R5 C6).
	if err := http.ListenAndServe(":3601", mux); err != nil {
		log.Fatal("webhook server failed:", err)
	}
}
// ── Polling loop ────────────────────────────────────────

func main() {
	log.SetFlags(0)
	loadSecrets()

	// Graceful exit → re-enable webhook
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		setWebhook()
		os.Exit(0)
	}()

	// Always-on webhook server (handles wake-ups during sleep mode)
	go webhookServer()

	// Start in polling mode (delete webhook to force polling)
	deleteWebhook()

	// Get latest update_id to skip old messages
	params := url.Values{"offset": {"-1"}, "limit": {"1"}}
	result, err := tgAPI("getUpdates", params)
	lastUpdateID := 0
	if err == nil {
		if updates, ok := result["result"].([]interface{}); ok && len(updates) > 0 {
			if u, ok := updates[len(updates)-1].(map[string]interface{}); ok {
				if id, ok := u["update_id"].(float64); ok {
					lastUpdateID = int(id)
				}
			}
		}
	}
	log.Printf("Bot started. Last update_id: %d", lastUpdateID)

	const idleTimeout = 600 // 10 minutes
	lastMsgTime := time.Now()

	for {
		params := url.Values{
			"offset":  {fmt.Sprintf("%d", lastUpdateID+1)},
			"timeout": {"30"},
		}
		result, err := tgAPI("getUpdates", params)
		if err != nil {
			time.Sleep(5 * time.Second)
			continue
		}

		updates, ok := result["result"].([]interface{})
		if !ok || len(updates) == 0 {
			// Idle timeout → switch to webhook mode
			if time.Since(lastMsgTime) > idleTimeout*time.Second {
				log.Println("Idle timeout, entering sleep mode")
				setWebhook()
					<-wakeCh
					log.Println("Woke up, resuming polling")
					deleteWebhook()
					lastMsgTime = time.Now()
					continue
				}
				time.Sleep(500 * time.Millisecond)
				continue
			}

			hasMsg := false
		for _, raw := range updates {
			u, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if id, ok := u["update_id"].(float64); ok {
				lastUpdateID = int(id)
			}
			msg, ok := u["message"].(map[string]interface{})
			if !ok {
				continue
			}
			chat, ok := msg["chat"].(map[string]interface{})
			if !ok {
				continue
			}
			chatID, ok := chat["id"].(float64)
			if !ok {
				continue
			}
			text, _ := msg["text"].(string)
			if text != "" {
				handleCommand(int64(chatID), text)
				hasMsg = true
			}
		}
		if hasMsg {
			lastMsgTime = time.Now()
		}
		time.Sleep(500 * time.Millisecond)
	}
}
