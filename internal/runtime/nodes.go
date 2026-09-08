package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"

	"lambs-server-go/internal/db"
)

// NodeSnapshot is one polled machine's metric snapshot.
type NodeSnapshot struct {
	Name        string  `json:"name"`
	Online      bool    `json:"online"`
	CPU         float64 `json:"cpu_percent"`
	MemUsedMB   int     `json:"memory_used_mb"`
	MemTotalMB  int     `json:"memory_total_mb"`
	DiskUsedGB  float64 `json:"disk_used_gb"`
	DiskTotalGB float64 `json:"disk_total_gb"`
	Uptime      int     `json:"uptime_seconds"`
	FetchedAt   int64   `json:"fetched_at"`
}

var (
	woolMu    sync.RWMutex
	woolNode  NodeSnapshot
	agentMu   sync.RWMutex
	agentNode NodeSnapshot
)

// Default wool agent URL — override with WOOL_AGENT_URL.
func woolAgentURL() string {
	if u := os.Getenv("WOOL_AGENT_URL"); u != "" {
		return u
	}
	return "" // 未配置 WOOL_AGENT_URL = 节点监控停用（开源默认，不硬编码内网地址 R24）
}

// pollNode fetches one node's metrics. Any transport/parse failure marks
// the node offline — a monitor that lies is worse than none.
func pollNode(name, url string) NodeSnapshot {
	n := NodeSnapshot{Name: name}
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return n
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return n
	}
	var raw struct {
		Hostname    string  `json:"hostname"`
		CPU         float64 `json:"cpu_percent"`
		MemUsedMB   int     `json:"memory_used_mb"`
		MemTotalMB  int     `json:"memory_total_mb"`
		DiskUsedGB  float64 `json:"disk_used_gb"`
		DiskTotalGB float64 `json:"disk_total_gb"`
		Uptime      int     `json:"uptime_seconds"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return n
	}
	n.Online = true
	if raw.Hostname != "" {
		n.Name = raw.Hostname
	}
	n.CPU = raw.CPU
	n.MemUsedMB = raw.MemUsedMB
	n.MemTotalMB = raw.MemTotalMB
	n.DiskUsedGB = raw.DiskUsedGB
	n.DiskTotalGB = raw.DiskTotalGB
	n.Uptime = raw.Uptime
	n.FetchedAt = time.Now().Unix()
	return n
}

// nodePollOnce is one monitor pass — extracted so the poll/write cycle is
// testable without the never-returning monitor goroutine.
func nodePollOnce() {
	n := pollNode("wool", woolAgentURL())
	woolMu.Lock()
	woolNode = n
	woolMu.Unlock()
	a := pollNode("windows-agent", agentURL+"/health")
	agentMu.Lock()
	agentNode = a
	agentMu.Unlock()
}

// StartNodeMonitor polls wool and the Windows compute agent on a 30s
// ticker. Never returns.
func StartNodeMonitor() {
	nodePollOnce()
	go func() {
		for range time.Tick(30 * time.Second) {
			nodePollOnce()
		}
	}()
}

// RegistrySnapshot returns one snapshot per registered machine. Wool and the
// Windows agent keep live polled metrics; other machines fall back to the
// registry's static capacity with online status (B5c — replaces the old
// hardcoded two-node list).
func RegistrySnapshot() []NodeSnapshot {
	rows, err := db.DB.Query(`SELECT id, COALESCE(status,'online'), COALESCE(mem_gb,0), COALESCE(disk_gb,0) FROM machines ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []NodeSnapshot{}
	for rows.Next() {
		var id, status string
		var memGB, diskGB int
		if rows.Scan(&id, &status, &memGB, &diskGB) != nil {
			continue
		}
		var snap NodeSnapshot
		switch id {
		case "wool":
			woolMu.RLock()
			snap = woolNode
			woolMu.RUnlock()
		case "laptop":
			agentMu.RLock()
			snap = agentNode
			snap.Name = "laptop" // 注册表 id 为准（agent 上报的是主机名）
			agentMu.RUnlock()
		default:
			snap = NodeSnapshot{
				Name:       id,
				Online:     status == "online",
				MemTotalMB: memGB * 1024,
				DiskTotalGB: float64(diskGB),
			}
		}
		out = append(out, snap)
	}
	return out
}

// WoolSnapshot returns the last polled wool metrics. Read-only.
func WoolSnapshot() NodeSnapshot {
	woolMu.RLock()
	defer woolMu.RUnlock()
	return woolNode
}

// AgentSnapshot returns the last polled Windows agent metrics. Read-only.
func AgentSnapshot() NodeSnapshot {
	agentMu.RLock()
	defer agentMu.RUnlock()
	return agentNode
}
