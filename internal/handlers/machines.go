package handlers

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/models"
	"lambs-server-go/internal/runtime"
)

// HandleHeartbeat accepts metric pushes from res-monitor on each machine
// (internal tailnet path, token-gated).
func HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	expect := os.Getenv("MACHINE_HEARTBEAT_TOKEN")
	if expect == "" || r.Header.Get("Authorization") != "Bearer "+expect {
		auth.JSONErr(w, 401, "invalid token")
		return
	}
	var body struct {
		ID         string  `json:"id"`
		CPU        float64 `json:"cpu_percent"`
		MemUsedMB  int     `json:"memory_used_mb"`
		DiskUsedGB float64 `json:"disk_used_gb"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		auth.JSONErr(w, 400, "invalid body")
		return
	}
	// hostname 大小写与注册表 id 归一（Lambs 机 hostname 是大写 L）
	id := strings.ToLower(body.ID)
	runtime.StoreLive(id, runtime.NodeLive{CPU: body.CPU, MemUsedMB: body.MemUsedMB, DiskUsedGB: body.DiskUsedGB, TS: time.Now().Unix()})
	auth.JSONOK(w, map[string]string{"id": body.ID})
}

// ListMachines returns all registered machines from the machines registry.
func ListMachines(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query(`SELECT id, role, COALESCE(ts_ip,''), COALESCE(lan_ip,''), COALESCE(os,''),
		COALESCE(arch,''), COALESCE(cpu_cores,0), COALESCE(mem_gb,0), COALESCE(disk_gb,0),
		COALESCE(tags::text,'[]'), COALESCE(status,'online'), COALESCE(override::text,'[]'),
		COALESCE(notes,''), COALESCE(created_at::text,''), COALESCE(updated_at::text,''),
		COALESCE(last_check_at::text,''), COALESCE(ssh_user,'') FROM machines ORDER BY id`)
	if err != nil {
		auth.JSONErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	machines := []models.Machine{}
	for rows.Next() {
		var m models.Machine
		var tags, override string
		if err := rows.Scan(&m.ID, &m.Role, &m.TSIP, &m.LanIP, &m.OS, &m.Arch, &m.CPU,
			&m.MemGB, &m.DiskGB, &tags, &m.Status, &override, &m.Notes,
			&m.CreatedAt, &m.UpdatedAt, &m.LastCheckAt, &m.SSHUser); err != nil {
			auth.JSONErr(w, 500, err.Error())
			return
		}
		m.Tags = json.RawMessage(tags)
		m.Override = json.RawMessage(override)
		// Live metrics: heartbeat for linux machines, agent poll for laptop
		if live, ok := runtime.SnapshotLive(m.ID); ok {
			m.CpuPercent = live.CPU
			m.MemUsedMB = live.MemUsedMB
			m.DiskUsedGB = live.DiskUsedGB
		} else if m.ID == "laptop" {
			if snap := runtime.AgentSnapshot(); snap.Name != "" {
				m.CpuPercent = snap.CPU
				m.MemUsedMB = snap.MemUsedMB
				m.DiskUsedGB = snap.DiskUsedGB
			}
		}
		machines = append(machines, m)
	}
	auth.JSONOK(w, map[string]interface{}{"machines": machines, "total": len(machines)})
}

// UpsertMachine registers a new machine or updates an existing one (super_admin only).
func UpsertMachine(w http.ResponseWriter, r *http.Request) {
	var m models.Machine
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		auth.JSONErr(w, 400, "invalid body: "+err.Error())
		return
	}
	if m.ID == "" {
		auth.JSONErr(w, 400, "id (hostname) is required")
		return
	}
	tags := "[]"
	if m.Tags != nil {
		tags = string(mustJSON(m.Tags))
	}
	override := "[]"
	if m.Override != nil {
		override = string(mustJSON(m.Override))
	}
	_, err := db.DB.Exec(`INSERT INTO machines (id, role, ts_ip, lan_ip, os, arch, cpu_cores, mem_gb, disk_gb, tags, status, override, notes, ssh_user, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,now())
		ON CONFLICT (id) DO UPDATE SET role=$2, ts_ip=$3, lan_ip=$4, os=$5, arch=$6, cpu_cores=$7,
		mem_gb=$8, disk_gb=$9, tags=$10, status=$11, override=$12, notes=$13, ssh_user=$14, updated_at=now()`,
		m.ID, m.Role, m.TSIP, m.LanIP, m.OS, m.Arch, m.CPU, m.MemGB, m.DiskGB,
		tags, m.Status, override, m.Notes, m.SSHUser)
	if err != nil {
		auth.JSONErr(w, 500, err.Error())
		return
	}
	auth.JSONOK(w, map[string]string{"id": m.ID})
}

// PatchMachineStatus updates a machine's status field only.
func PatchMachineStatus(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		auth.JSONErr(w, 400, "invalid body: "+err.Error())
		return
	}
	if body.Status == "" {
		auth.JSONErr(w, 400, "status is required")
		return
	}
	if _, err := db.DB.Exec(`UPDATE machines SET status=$1, updated_at=now() WHERE id=$2`, body.Status, id); err != nil {
		auth.JSONErr(w, 500, err.Error())
		return
	}
	auth.JSONOK(w, map[string]string{"id": id, "status": body.Status})
}

// ReconcileMachines probes each machine's reachability and syncs status (super_admin only).
// Linux machines are probed on SSH (22); Windows on compute-agent (19527).
func ReconcileMachines(w http.ResponseWriter, r *http.Request) {
	rows, err := db.DB.Query(`SELECT id, COALESCE(ts_ip,''), COALESCE(os,'') FROM machines`)
	if err != nil {
		auth.JSONErr(w, 500, err.Error())
		return
	}
	type res struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		Err    string `json:"error,omitempty"`
	}
	results := []res{}
	type rec struct {
		id, ip, os string
	}
	var recs []rec
	for rows.Next() {
		var rr rec
		rows.Scan(&rr.id, &rr.ip, &rr.os)
		recs = append(recs, rr)
	}
	rows.Close()

	for _, rr := range recs {
		port := "22"
		if rr.os == "windows" {
			port = "19527"
		}
		status := "online"
		errMsg := ""
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(rr.ip, port), 3*time.Second)
		if err != nil {
			status = "offline"
			errMsg = err.Error()
		} else {
			conn.Close()
		}
		if _, err := db.DB.Exec(`UPDATE machines SET status=$1, last_check_at=now() WHERE id=$2`, status, rr.id); err != nil {
			auth.JSONErr(w, 500, err.Error())
			return
		}
		results = append(results, res{ID: rr.id, Status: status, Err: errMsg})
	}
	auth.JSONOK(w, map[string]interface{}{"results": results})
}

// DeleteMachine removes a machine from the registry (super_admin only).
func DeleteMachine(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := db.DB.Exec(`DELETE FROM machines WHERE id=$1`, id); err != nil {
		auth.JSONErr(w, 500, err.Error())
		return
	}
	auth.JSONOK(w, map[string]string{"id": id})
}

// allocHost picks a deployment target for a new project. An explicit machine
// id is honored when registered and online; otherwise the largest online
// linux compute machine wins. Falls back to lambs (the legacy local path).
func allocHost(preferred string) (string, error) {
	if preferred != "" && preferred != "auto" {
		var cnt int
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM machines WHERE id=$1 AND status='online'`, preferred).Scan(&cnt); err != nil || cnt == 0 {
			return "", fmt.Errorf("指定机器 %s 不存在或离线", preferred)
		}
		return preferred, nil
	}
	var id string
	err := db.DB.QueryRow(`SELECT id FROM machines WHERE status='online' AND os='ubuntu' AND role LIKE '%compute%'
		ORDER BY mem_gb DESC, disk_gb DESC, cpu_cores DESC LIMIT 1`).Scan(&id)
	if err != nil {
		return "lambs", nil // no compute machine available — legacy local path
	}
	return id, nil
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
