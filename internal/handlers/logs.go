package handlers

import (
	"net/http"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/execpath"
)

// journalRe parses journalctl short-iso lines:
// "2026-08-17T22:39:28+08:00 Lambs lambs-server[2868301]: <message>"
var journalRe = regexp.MustCompile(`^(\S+)\s+\S+\s+\S+\[\d+\]:\s(.*)$`)

// classifyLevel guesses a level for a log line. Lambs logs via the stdlib
// logger and carry no explicit level, so keywords are the best available
// signal. Order matters: error keywords first, then warn.
func classifyLevel(line string) string {
	lt := strings.ToLower(line)
	switch {
	case strings.Contains(lt, "panic"), strings.Contains(lt, "fatal"),
		strings.Contains(lt, "failed"), strings.Contains(lt, "error"):
		return "error"
	case strings.Contains(lt, "warn"), strings.Contains(lt, "down"),
		strings.Contains(lt, "deprecated"), strings.Contains(lt, "timeout"):
		return "warn"
	}
	return "info"
}

// parseJournalLine splits one journalctl line into its display fields.
// Lines without the journald prefix pass through untouched.
func parseJournalLine(line string) map[string]interface{} {
	if m := journalRe.FindStringSubmatch(line); m != nil {
		return map[string]interface{}{
			"time":    m[1],
			"level":   classifyLevel(m[2]),
			"message": m[2],
		}
	}
	return map[string]interface{}{
		"time":    "",
		"level":   classifyLevel(line),
		"message": line,
	}
}

// HandleSystemLogs serves the lambs-server's own journald log — the real
// system log, replacing the old audit+status aggregate. super_admin only:
// log lines can carry internal detail (paths, DSN fragments in errors).
func HandleSystemLogs(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Role") != "super_admin" {
		auth.JSONErr(w, 403, "仅管理员可查看系统日志")
		return
	}
	lines, _ := strconv.Atoi(r.URL.Query().Get("lines"))
	if lines < 1 || lines > 200 {
		lines = 30
	}
	out, err := exec.Command(execpath.Path("journalctl"), "-u", "lambs-server", "--no-pager", "-o", "short-iso", "-n", strconv.Itoa(lines)).Output()
	logs := []map[string]interface{}{}
	if err != nil {
		// Degrade to an empty list — the card shows 暂无日志 instead of dying.
		auth.JSONOK(w, logs)
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		logs = append(logs, parseJournalLine(line))
	}
	auth.JSONOK(w, logs)
}
