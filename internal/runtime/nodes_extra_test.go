package runtime

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPollNodeNon200 — a 500 from the agent marks the node offline
// (nodes_test covers 200/unreachable/garbage; this closes the status branch).
func TestPollNodeNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer ts.Close()
	if n := pollNode("wool", ts.URL); n.Online {
		t.Errorf("online = true on 500, want offline")
	}
}

// TestSnapshotsReadBack — the mutex-guarded accessors return what was
// written under the same locks (woolNode/agentNode had zero coverage).
func TestSnapshotsReadBack(t *testing.T) {
	woolMu.Lock()
	woolNode = NodeSnapshot{Name: "wool", Online: true, CPU: 1.5}
	woolMu.Unlock()
	t.Cleanup(func() {
		woolMu.Lock()
		woolNode = NodeSnapshot{}
		woolMu.Unlock()
	})
	agentMu.Lock()
	agentNode = NodeSnapshot{Name: "agent", MemUsedMB: 128}
	agentMu.Unlock()
	t.Cleanup(func() {
		agentMu.Lock()
		agentNode = NodeSnapshot{}
		agentMu.Unlock()
	})

	if w := WoolSnapshot(); !w.Online || w.CPU != 1.5 {
		t.Errorf("WoolSnapshot = %+v", w)
	}
	if a := AgentSnapshot(); a.MemUsedMB != 128 {
		t.Errorf("AgentSnapshot = %+v", a)
	}
}
