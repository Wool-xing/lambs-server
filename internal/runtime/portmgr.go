package runtime

import (
	"fmt"
	"net"
	"strconv"
	"sync"

	"lambs-server-go/internal/db"
)

type PortManager struct {
	mu        sync.Mutex
	StartPort int
	EndPort   int
}

var PortMgr = &PortManager{StartPort: 3510, EndPort: 3599}

func (pm *PortManager) Allocate(projectID string) (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	var existing string
	db.DB.QueryRow("SELECT COALESCE(port,'') FROM projects WHERE id=$1", projectID).Scan(&existing)
	if existing != "" {
		if p, err := strconv.Atoi(existing); err == nil && p >= pm.StartPort && p <= pm.EndPort {
			return p, nil
		}
	}
	for port := pm.StartPort; port <= pm.EndPort; port++ {
		var count int
		db.DB.QueryRow("SELECT COUNT(*) FROM projects WHERE port=$1 AND id!=$2", fmt.Sprintf("%d", port), projectID).Scan(&count)
		if count > 0 {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			continue
		}
		ln.Close()
		db.DB.Exec("UPDATE projects SET port=$1 WHERE id=$2", fmt.Sprintf("%d", port), projectID)
		return port, nil
	}
	return 0, fmt.Errorf("no free ports in range %d-%d", pm.StartPort, pm.EndPort)
}

func (pm *PortManager) Free(projectID string) {
	db.DB.Exec("UPDATE projects SET port='' WHERE id=$1", projectID)
}
