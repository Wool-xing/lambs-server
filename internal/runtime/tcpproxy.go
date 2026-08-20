package runtime

import (
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lambs-server-go/internal/db"
)

type TCPProxy struct {
	mu        sync.RWMutex
	listeners map[string]net.Listener
	actives   map[string]*int64 // active connection count per project
}

// allowedSources: TCP_PROXY_ALLOWED_IPS 逗号分隔的来源 IP 白名单。
// 空 = fail-closed：仅回环放行，任何非回环连接被拒 (QA 第 2 轮 HIGH)。
// 生产应设 wool 的公网 IP — 否则代理只对本机可用，不会裸奔公网。
var allowedSources = parseAllowedSources(os.Getenv("TCP_PROXY_ALLOWED_IPS"))

func init() {
	if len(allowedSources) == 0 {
		log.Printf("tcp-proxy: TCP_PROXY_ALLOWED_IPS unset — only loopback sources allowed (fail-closed)")
	}
}

func parseAllowedSources(s string) map[string]bool {
	m := map[string]bool{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			m[p] = true
		}
	}
	return m
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// selfLoopBackend reports whether backend would dial the proxy's own
// listener port on a loopback host (any loopback spelling).
func selfLoopBackend(backend, portStr string) bool {
	host, port, err := net.SplitHostPort(backend)
	if err != nil {
		return false
	}
	return port == portStr && isLoopback(host)
}

func sourceAllowed(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return false
	}
	if isLoopback(host) {
		return true
	}
	return allowedSources[host]
}

var TCPProxyMgr = &TCPProxy{
	listeners: make(map[string]net.Listener),
	actives:   make(map[string]*int64),
}

func (tp *TCPProxy) Start(projectID string) error {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if _, ok := tp.listeners[projectID]; ok {
		return nil
	}
	var portStr, backendPort, svcName, startCmd string
	db.DB.QueryRow("SELECT COALESCE(port,''), COALESCE(backend_url,''), COALESCE(service_name,''), COALESCE(startup_command,'') FROM projects WHERE id=$1", projectID).
		Scan(&portStr, &backendPort, &svcName, &startCmd)
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return fmt.Errorf("invalid port for project %s", projectID)
	}
	backend := backendPort
	if backend == "" {
		// No backend_url: the proxy would target its own listener — a pure
		// datasource project has nothing listening on its port, so skip it
		// instead of building a self-connecting loop.
		if svcName == "" && startCmd == "" {
			log.Printf("tcp-proxy: %s has no backend, skipping proxy", projectID)
			return nil
		}
		backend = fmt.Sprintf("127.0.0.1:%s", portStr)
	}
	backend = strings.TrimPrefix(backend, "http://")
	if !strings.Contains(backend, ":") {
		backend = "127.0.0.1:" + backend
	}
	// 自环守卫：backend 与本监听端口同端口 = 代理连自己 (R25 校准发现)。
	// host/port 拆分比较，覆盖 localhost / ::1 等变体，不只 127.0.0.1 字面量。
	if selfLoopBackend(backend, portStr) {
		log.Printf("tcp-proxy: %s backend equals listener port (%s) — self-loop, skipping", projectID, portStr)
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", port, err)
	}
	tp.listeners[projectID] = ln
	var counter int64
	tp.actives[projectID] = &counter
	go tp.serve(projectID, ln, backend, &counter)
	log.Printf("tcp-proxy: %s listening on :%d -> %s", projectID, port, backend)
	return nil
}

func (tp *TCPProxy) serve(projectID string, ln net.Listener, backend string, counter *int64) {
	defer func() {
		// Accept failed = this listener is dead. Leave it in the map and
		// Start() would report "already running" forever.
		tp.mu.Lock()
		if tp.listeners[projectID] == ln {
			delete(tp.listeners, projectID)
			delete(tp.actives, projectID)
		}
		tp.mu.Unlock()
		log.Printf("tcp-proxy: %s listener closed, removed from registry", projectID)
	}()
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		if !sourceAllowed(client.RemoteAddr().String()) {
			client.Close()
			log.Printf("tcp-proxy: %s rejected connection from %s (not in allowlist)", projectID, client.RemoteAddr())
			continue
		}
		go func() {
			defer client.Close()
			atomic.AddInt64(counter, 1)
			defer atomic.AddInt64(counter, -1)
			ProcMgr.Start(projectID)
			var backendConn net.Conn
			for i := 0; i < 60; i++ {
				conn, err := net.DialTimeout("tcp", backend, 500*time.Millisecond)
				if err == nil {
					backendConn = conn
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			if backendConn == nil {
				return
			}
			defer backendConn.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(backendConn, client); done <- struct{}{} }()
			go func() { io.Copy(client, backendConn); done <- struct{}{} }()
			<-done
		}()
	}
}

func (tp *TCPProxy) Stop(projectID string) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	if ln, ok := tp.listeners[projectID]; ok {
		ln.Close()
		delete(tp.listeners, projectID)
	}
	delete(tp.actives, projectID)
	log.Printf("tcp-proxy: %s stopped", projectID)
}

// IdleMonitor stops managed backends that have been idle for 5+ minutes.
func (tp *TCPProxy) IdleMonitor() {
	for {
		time.Sleep(1 * time.Minute)
		tp.mu.RLock()
		for projectID, counter := range tp.actives {
			if atomic.LoadInt64(counter) == 0 {
				// Check last activity via health — if ProcManager started it but no traffic, consider idle
				st := ProcMgr.Status(projectID)
				if running, _ := st["running"].(bool); running {
					if uptime, ok := st["uptime_sec"].(int); ok && uptime > 300 {
						var svcName string
						if err := db.DB.QueryRow("SELECT COALESCE(service_name,'') FROM projects WHERE id=$1", projectID).Scan(&svcName); err != nil {
							// 查询失败 = 不知道是否 systemd 管 — 宁可不杀 (QA 第 2 轮校准)。
							log.Printf("tcp-proxy: %s svcName lookup failed, skipping idle stop: %v", projectID, err)
							continue
						}
						// Only auto-stop if NOT a systemd unit (systemd handles its own lifecycle).
						// svcName was queried but never used — the stop fired for
						// managed services too (R12 code review).
						if svcName != "" {
							continue
						}
						if _, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%v", st["port"]), 2*time.Second); err != nil {
							log.Printf("tcp-proxy: %s backend unreachable, stopping idle process", projectID)
							ProcMgr.Stop(projectID)
						}
					}
				}
			}
		}
		tp.mu.RUnlock()
	}
}
