package runtime

import (
	"fmt"
	"io"
	"log"
	"net"
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
	var portStr, backendPort string
	db.DB.QueryRow("SELECT COALESCE(port,''), COALESCE(backend_url,'') FROM projects WHERE id=$1", projectID).Scan(&portStr, &backendPort)
	port, err := strconv.Atoi(portStr)
	if err != nil || port == 0 {
		return fmt.Errorf("invalid port for project %s", projectID)
	}
	backend := backendPort
	if backend == "" {
		backend = fmt.Sprintf("127.0.0.1:%s", portStr)
	}
	backend = strings.TrimPrefix(backend, "http://")
	if !strings.Contains(backend, ":") {
		backend = "127.0.0.1:" + backend
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
	for {
		client, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer client.Close()
			atomic.AddInt64(counter, 1)
			defer atomic.AddInt64(counter, -1)
			ProcMgr.Start(projectID)
				var backendConn net.Conn
				for i := 0; i < 60; i++ {
					conn, err := net.DialTimeout("tcp", backend, 500*time.Millisecond)
					if err == nil { backendConn = conn; break }
					time.Sleep(500 * time.Millisecond)
				}
				if backendConn == nil { return }
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
	if _, ok := tp.actives[projectID]; ok {
		delete(tp.actives, projectID)
	}
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
						db.DB.QueryRow("SELECT COALESCE(service_name,'') FROM projects WHERE id=$1", projectID).Scan(&svcName)
						// Only auto-stop if NOT a systemd unit (systemd handles its own lifecycle)
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
