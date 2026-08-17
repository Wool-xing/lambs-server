package runtime

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"time"
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
	woolMu   sync.RWMutex
	woolNode NodeSnapshot
)

// Default wool agent URL — override with WOOL_AGENT_URL.
func woolAgentURL() string {
	if u := os.Getenv("WOOL_AGENT_URL"); u != "" {
		return u
	}
	return "http://100.126.18.126:3901/health"
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
	n.CPU = raw.CPU
	n.MemUsedMB = raw.MemUsedMB
	n.MemTotalMB = raw.MemTotalMB
	n.DiskUsedGB = raw.DiskUsedGB
	n.DiskTotalGB = raw.DiskTotalGB
	n.Uptime = raw.Uptime
	n.FetchedAt = time.Now().Unix()
	return n
}

// StartNodeMonitor polls wool on a 30s ticker. Never returns.
func StartNodeMonitor() {
	poll := func() {
		n := pollNode("wool", woolAgentURL())
		woolMu.Lock()
		woolNode = n
		woolMu.Unlock()
	}
	poll()
	go func() {
		for range time.Tick(30 * time.Second) {
			poll()
		}
	}()
}

// WoolSnapshot returns the last polled wool metrics. Read-only.
func WoolSnapshot() NodeSnapshot {
	woolMu.RLock()
	defer woolMu.RUnlock()
	return woolNode
}
