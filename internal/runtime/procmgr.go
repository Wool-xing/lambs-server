package runtime

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"lambs-server-go/internal/db"
)

type procState struct {
	cmd     *exec.Cmd
	port    int
	started time.Time
	logFile *os.File
	done    chan struct{} // closed by the single Wait goroutine when the process exits
}

type cpuSample struct {
	ticks int64
	at    time.Time
}

// svcState is a shared service instance with reference counting.
// Multiple projects referencing the same service name share one instance:
// it starts on first attach and stops when the last project detaches.
type svcState struct {
	name     string
	startCmd string
	stopCmd  string
	refs     map[string]bool // projectID -> true (only online projects count)
	cmd      *exec.Cmd       // direct-run process (bash -c startCmd)
	logFile  *os.File
	done     chan struct{} // closed by the single Wait goroutine when the process exits
	starting bool          // launch in flight — guards against double start
}

type ProcManager struct {
	mu       sync.RWMutex
	procs    map[string]*procState
	services map[string]*svcState
	samples  map[string]cpuSample
}

var ProcMgr = &ProcManager{procs: make(map[string]*procState), services: make(map[string]*svcState), samples: make(map[string]cpuSample)}

// clockTicks is the kernel jiffies-per-second (usually 100), detected at startup.
var clockTicks = 100.0

func init() {
	if out, err := exec.Command("getconf", "CLK_TCK").Output(); err == nil {
		if v, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil && v > 0 {
			clockTicks = v
		}
	}
}

// minFreeMB returns the startup memory gate threshold in MB.
// Default 100; overridable via LAMBS_MIN_FREE_MB for tuning and testing.
func minFreeMB() int {
	if v := os.Getenv("LAMBS_MIN_FREE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return 100
}

// memAvailableMB reads MemAvailable from /proc/meminfo, or returns 0 on error.
func memAvailableMB() (int, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "MemAvailable:") {
			f := strings.Fields(line)
			if len(f) > 1 {
				kb, err := strconv.Atoi(f[1])
				if err != nil {
					return 0, err
				}
				return kb / 1024, nil
			}
		}
	}
	return 0, fmt.Errorf("MemAvailable not found")
}

func (pm *ProcManager) Start(projectID string) error {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	if s, ok := pm.procs[projectID]; ok && s.cmd != nil && s.cmd.Process != nil {
		return nil
	}
	// Memory gate: refuse to start another process when the box is nearly
	// out of memory — a new process under pressure would OOM the whole host.
	if avail, err := memAvailableMB(); err == nil && avail < minFreeMB() {
		return fmt.Errorf("可用内存不足（%dMB），拒绝启动以避免拖垮整机", avail)
	}
	var svcName, portStr, startCmd string
	db.DB.QueryRow("SELECT COALESCE(service_name,''), COALESCE(port,''), COALESCE(startup_command,'') FROM projects WHERE id=$1",
		projectID).Scan(&svcName, &portStr, &startCmd)
	var cmd *exec.Cmd
	port, _ := strconv.Atoi(portStr)

	// Priority: startup_command (Lambs managed) > systemd unit > binary path
	if startCmd != "" {
		// Parse "cd /dir && command args..." pattern for working directory
		cmdStr := startCmd
		var workDir string
		if strings.HasPrefix(cmdStr, "cd ") {
			if idx := strings.Index(cmdStr, " && "); idx > 0 {
				workDir = strings.TrimSpace(cmdStr[3:idx])
				cmdStr = strings.TrimSpace(cmdStr[idx+4:])
			}
		}
		parts := strings.Fields(cmdStr)
		if len(parts) == 0 {
			return fmt.Errorf("empty startup_command for %s", projectID)
		}
		cmd = exec.Command(parts[0], parts[1:]...)
		if workDir != "" {
			cmd.Dir = workDir
		}
	} else if svcName != "" {
		// Fallback 1: try systemctl for existing systemd unit
		unitName := svcName + ".service"
		if _, err := os.Stat("/etc/systemd/system/" + unitName); err == nil {
			c := exec.Command("sudo", "systemctl", "start", unitName)
			if out, err := c.CombinedOutput(); err != nil {
				return fmt.Errorf("systemctl start %s: %s", unitName, string(out))
			}
			log.Printf("runtime: started %s via systemctl", unitName)
			return nil
		}
		// Fallback 2: binary at /home/ubuntu/apps/<name>/<name>
		binPath := fmt.Sprintf("/home/ubuntu/apps/%s/%s", svcName, svcName)
		if _, err := os.Stat(binPath); os.IsNotExist(err) {
			return fmt.Errorf("binary not found: %s", binPath)
		}
		cmd = exec.Command(binPath)
	} else {
		return fmt.Errorf("project %s has no service_name or startup_command", projectID)
	}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "DATABASE_URL=") && !strings.HasPrefix(e, "JWT_SECRET=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	if startCmd == "" {
		// Only force PORT for legacy binary-path mode — startup_command handles its own port
		cmd.Env = append(cmd.Env, fmt.Sprintf("PORT=%s", portStr))
	}
	// Runtime-managed processes log to a per-project file so the
	// logs endpoint can show real output (journalctl only covers systemd units).
	logDir := "/home/ubuntu/apps/lambs-server/logs"
	os.MkdirAll(logDir, 0755)
	lf, lfErr := os.OpenFile(logDir+"/"+projectID+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if lfErr == nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		if lfErr == nil {
			lf.Close()
		}
		return fmt.Errorf("start %s: %w", svcName, err)
	}
	done := make(chan struct{})
	pm.procs[projectID] = &procState{cmd: cmd, port: port, started: time.Now(), logFile: lf, done: done}
	go func() {
		cmd.Wait()
		pm.mu.Lock()
		if s, ok := pm.procs[projectID]; ok && s.logFile != nil {
			s.logFile.Close()
		}
		delete(pm.procs, projectID)
		pm.mu.Unlock()
		close(done)
	}()
	log.Printf("runtime: started %s (pid=%d, port=%d)", svcName, cmd.Process.Pid, port)
	return nil
}

func (pm *ProcManager) Stop(projectID string) error {
	pm.mu.Lock()
	s, ok := pm.procs[projectID]
	if !ok || s.cmd == nil || s.cmd.Process == nil {
		pm.mu.Unlock()
		// Try systemctl stop if not a tracked process
		var svcName string
		db.DB.QueryRow("SELECT COALESCE(service_name,'') FROM projects WHERE id=$1", projectID).Scan(&svcName)
		if svcName != "" {
			unitName := svcName + ".service"
			if _, err := os.Stat("/etc/systemd/system/" + unitName); err == nil {
				exec.Command("sudo", "systemctl", "stop", unitName).Run()
				log.Printf("runtime: stopped %s via systemctl", unitName)
			}
		}
		return nil
	}
	pid := s.cmd.Process.Pid
	done := s.done
	pm.mu.Unlock()
	// logFile is closed by the Wait goroutine — closing it here too races on
	// the fd (it may already be reused by another open).
	killGroup(pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		killGroup(pid, syscall.SIGKILL)
		<-done
	}
	pm.mu.Lock()
	delete(pm.procs, projectID)
	pm.mu.Unlock()
	log.Printf("runtime: stopped project %s (pid=%d)", projectID, pid)
	return nil
}

func (pm *ProcManager) Restart(projectID string) error {
	pm.Stop(projectID)
	time.Sleep(500 * time.Millisecond)
	return pm.Start(projectID)
}

// readProcStats returns CPU ticks (utime+stime), RSS pages and starttime jiffies for a pid.
func readProcStats(pid int) (ticks int64, rssPages int64, startTicks int64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0, 0
	}
	// stat format: pid (comm) state ppid ... — fields after comm start at index 3
	// Split on ')' to skip comm which may contain spaces
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 >= len(s) {
		return 0, 0, 0
	}
	fields := strings.Fields(s[idx+2:])
	// fields[0]=state(3), utime=14, stime=15, starttime=22 → index 11,12,19 in this slice
	if len(fields) < 20 {
		return 0, 0, 0
	}
	utime, _ := strconv.ParseInt(fields[11], 10, 64)
	stime, _ := strconv.ParseInt(fields[12], 10, 64)
	rss, _ := strconv.ParseInt(fields[21], 10, 64)
	start, _ := strconv.ParseInt(fields[19], 10, 64)
	return utime + stime, rss, start
}

// systemdPID returns the MainPID of a systemd unit, or 0.
func systemdPID(svcName string) int {
	out, err := exec.Command("systemctl", "show", svcName+".service", "-p", "MainPID", "--value").Output()
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return pid
}

// procUptimeSec derives process uptime from /proc/uptime and starttime jiffies.
func procUptimeSec(startTicks int64) int {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return 0
	}
	bootSec, _ := strconv.ParseFloat(fields[0], 64)
	return int(bootSec - float64(startTicks)/clockTicks)
}

func (pm *ProcManager) Status(projectID string) map[string]interface{} {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	pid := 0
	port := 0
	var started time.Time
	if s, ok := pm.procs[projectID]; ok && s.cmd != nil && s.cmd.Process != nil {
		pid = s.cmd.Process.Pid
		port = s.port
		started = s.started
	} else {
		// Fallback: systemd-managed service
		var svcName string
		db.DB.QueryRow("SELECT COALESCE(service_name,'') FROM projects WHERE id=$1", projectID).Scan(&svcName)
		if svcName != "" {
			pid = systemdPID(svcName)
		}
	}
	if pid <= 0 {
		return map[string]interface{}{"running": false, "project_id": projectID}
	}

	ticks, rssPages, startTicks := readProcStats(pid)
	now := time.Now()
	cpuPct := 0.0
	if prev, ok := pm.samples[projectID]; ok && !prev.at.IsZero() && ticks >= prev.ticks {
		elapsedTicks := now.Sub(prev.at).Seconds() * clockTicks
		if elapsedTicks > 0 {
			cpuPct = float64(ticks-prev.ticks) / elapsedTicks * 100
		}
	}
	pm.samples[projectID] = cpuSample{ticks: ticks, at: now}

	uptime := int(now.Sub(started).Seconds())
	if started.IsZero() {
		uptime = procUptimeSec(startTicks)
	}
	return map[string]interface{}{
		"running": true, "project_id": projectID,
		"pid": pid, "port": port,
		"uptime_sec": uptime,
		"cpu_percent": cpuPct,
		"rss_mb":     int(rssPages * 4 / 1024),
		"starting":   !started.IsZero() && time.Since(started) < 30*time.Second,
	}
}

func (pm *ProcManager) List() []map[string]interface{} {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	var out []map[string]interface{}
	for id, s := range pm.procs {
		if s.cmd == nil || s.cmd.Process == nil {
			continue
		}
		out = append(out, map[string]interface{}{
			"project_id": id, "pid": s.cmd.Process.Pid,
			"port": s.port, "uptime_sec": int(time.Since(s.started).Seconds()),
		})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	return out
}

// readServices returns the project's declared shared services.
func readServices(projectID string) []map[string]interface{} {
	var raw string
	db.DB.QueryRow("SELECT COALESCE(services::text,'[]') FROM projects WHERE id=$1", projectID).Scan(&raw)
	var arr []map[string]interface{}
	json.Unmarshal([]byte(raw), &arr)
	return arr
}

// AttachServices increments refcounts for the project's shared services and
// starts any that transition 0→1. Call on project going online.
func (pm *ProcManager) AttachServices(projectID string) {
	for _, s := range readServices(projectID) {
		name, _ := s["name"].(string)
		startCmd, _ := s["start_cmd"].(string)
		stopCmd, _ := s["stop_cmd"].(string)
		if name == "" || startCmd == "" {
			continue
		}
		pm.mu.Lock()
		st, ok := pm.services[name]
		preLen := 0
		if ok {
			preLen = len(st.refs)
		}
		if !ok {
			st = &svcState{name: name, startCmd: startCmd, stopCmd: stopCmd, refs: make(map[string]bool)}
			pm.services[name] = st
		}
		st.refs[projectID] = true
		first := preLen == 0
		pm.mu.Unlock()
		log.Printf("runtime: attach service %s project=%s pre=%d post=%d first=%v", name, projectID, preLen, len(st.refs), first)
		if first {
			if err := pm.startShared(st); err != nil {
				log.Printf("runtime: shared service %s start failed: %v", name, err)
				db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
					fmt.Sprintf("n%d", time.Now().UnixNano()), projectID, "alert", "共享服务启动失败", fmt.Sprintf("「%s」启动失败: %v", name, err))
			}
		}
	}
}

// DetachServices decrements refcounts and stops services that reach 0.
// Call on project going offline/maintenance or being deleted.
func (pm *ProcManager) DetachServices(projectID string) {
	for _, s := range readServices(projectID) {
		name, _ := s["name"].(string)
		if name == "" {
			continue
		}
		pm.mu.Lock()
		st, ok := pm.services[name]
		if !ok {
			pm.mu.Unlock()
			continue
		}
		delete(st.refs, projectID)
		last := len(st.refs) == 0
		if last {
			delete(pm.services, name)
		}
		pm.mu.Unlock()
		log.Printf("runtime: detach service %s project=%s refs=%d last=%v", name, projectID, len(st.refs), last)
		if last {
			pm.stopShared(st)
		}
	}
}

// startShared launches the service command (bash -c), gated on free memory.
func (pm *ProcManager) startShared(st *svcState) error {
	pm.mu.Lock()
	if st.starting || (st.cmd != nil && st.cmd.Process != nil) {
		pm.mu.Unlock()
		return nil // launch already in flight or running
	}
	st.starting = true
	pm.mu.Unlock()
	if avail, err := memAvailableMB(); err == nil && avail < minFreeMB() {
		pm.mu.Lock()
		st.starting = false
		pm.mu.Unlock()
		return fmt.Errorf("可用内存不足（%dMB），拒绝启动共享服务", avail)
	}
	logDir := "/home/ubuntu/apps/lambs-server/logs"
	os.MkdirAll(logDir, 0755)
	lf, err := os.OpenFile(logDir+"/svc-"+st.name+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		st.logFile = lf
	}
	cmd := exec.Command("bash", "-c", st.startCmd)
	if lf != nil {
		cmd.Stdout = lf
		cmd.Stderr = lf
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		if lf != nil {
			lf.Close()
		}
		// Reset the launch guard — a permanent "starting" flag would block
		// every future retry and make HealthMonitor believe a restart succeeded.
		pm.mu.Lock()
		st.starting = false
		pm.mu.Unlock()
		return err
	}
	st.cmd = cmd
	done := make(chan struct{})
	st.done = done
	go func() {
		cmd.Wait()
		pm.mu.Lock()
		st.cmd = nil
		st.starting = false
		if lf != nil {
			lf.Close()
		}
		pm.mu.Unlock()
		close(done)
	}()
	log.Printf("runtime: shared service %s started (%s)", st.name, st.startCmd)
	return nil
}

// stopShared runs the stop command and kills the direct process if tracked.
func (pm *ProcManager) stopShared(st *svcState) {
	if st.stopCmd != "" {
		c := exec.Command("bash", "-c", st.stopCmd)
		c.CombinedOutput()
	}
	if st.cmd != nil && st.cmd.Process != nil {
		pid := st.cmd.Process.Pid
		done := st.done
		killGroup(pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			killGroup(pid, syscall.SIGKILL)
			<-done
		}
	}
	log.Printf("runtime: shared service %s stopped", st.name)
}

// serviceRunning checks whether a shared service is actually up.
// systemctl-style commands are checked via is-active; direct runs via pid.
func (pm *ProcManager) serviceRunning(st *svcState) bool {
	if strings.Contains(st.startCmd, "systemctl") {
		fields := strings.Fields(st.startCmd)
		for i, f := range fields {
			if f == "start" && i+1 < len(fields) {
				out, err := exec.Command("systemctl", "is-active", fields[i+1]).Output()
				if err == nil && strings.TrimSpace(string(out)) == "active" {
					return true
				}
			}
		}
		return false
	}
	return st.cmd != nil && st.cmd.Process != nil
}

// HealthMonitor checks managed processes every 30s.
func (pm *ProcManager) HealthMonitor(enabled func() bool) {
	lastAlert := map[string]time.Time{} // per-project alert cooldown
	for {
		time.Sleep(30 * time.Second)
		if !enabled() {
			continue
		}
		rows, err := db.DB.Query("SELECT id, name FROM projects WHERE status='online'")
		if err != nil {
			continue
		}
		type entry struct{ id, name string }
		var projects []entry
		for rows.Next() {
			var e entry
			rows.Scan(&e.id, &e.name)
			projects = append(projects, e)
		}
		rows.Close()
		for _, p := range projects {
			st := pm.Status(p.id)
			if running, _ := st["running"].(bool); running {
				continue
			}
			if starting, _ := st["starting"].(bool); starting {
				continue // still initializing, skip this cycle
			}
			log.Printf("health: %s (%s) is down, restarting", p.name, p.id)
			if err := pm.Start(p.id); err != nil {
				// Projects with no startup_command/service_name are pure
				// datasources — nothing to restart, no alarm needed.
				if strings.Contains(err.Error(), "no service_name") {
					continue
				}
				log.Printf("health: %s restart failed: %v", p.name, err)
				// Alert cooldown: a crash-looping process must not insert a
				// notification every 30s (R12 perf review).
				if time.Since(lastAlert[p.id]) > 10*time.Minute {
					lastAlert[p.id] = time.Now()
					nid := fmt.Sprintf("n%d", time.Now().UnixNano())
					db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())", nid, p.id, "alert", "进程异常", fmt.Sprintf("「%s」进程意外退出，自动重启失败: %v", p.name, err))
				}
				continue
			}
			nid := fmt.Sprintf("n%d", time.Now().UnixNano())
			db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())", nid, p.id, "info", "进程恢复", fmt.Sprintf("「%s」进程已自动重启", p.name))
		}
		// Shared services: referenced (refs>0) but not running → restart.
		pm.mu.RLock()
		shared := make([]*svcState, 0, len(pm.services))
		for _, st := range pm.services {
			if len(st.refs) > 0 {
				shared = append(shared, st)
			}
		}
		pm.mu.RUnlock()
		for _, st := range shared {
			if pm.serviceRunning(st) {
				continue
			}
			log.Printf("health: shared service %s down, restarting", st.name)
			if err := pm.startShared(st); err != nil {
				db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
					fmt.Sprintf("n%d", time.Now().UnixNano()), "", "alert", "共享服务异常", fmt.Sprintf("「%s」重启失败: %v", st.name, err))
			} else {
				db.DB.Exec("INSERT INTO notifications (id, project_id, type, title, content, is_read, created_at) VALUES ($1,$2,$3,$4,$5,false,NOW())",
					fmt.Sprintf("n%d", time.Now().UnixNano()), "", "info", "共享服务恢复", fmt.Sprintf("「%s」已自动重启", st.name))
			}
		}
	}
}
