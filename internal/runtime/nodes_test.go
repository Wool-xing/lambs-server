package runtime

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestPollNodeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"hostname":"wool","cpu_percent":3.5,"memory_used_mb":800,"memory_total_mb":3900,"disk_used_gb":12.3,"disk_total_gb":40.0,"uptime_seconds":12345}`))
	}))
	defer srv.Close()

	n := pollNode("caller-supplied-name", srv.URL+"/health")
	if !n.Online {
		t.Fatal("expected online")
	}
	if n.CPU != 3.5 || n.MemUsedMB != 800 || n.MemTotalMB != 3900 || n.DiskUsedGB != 12.3 || n.DiskTotalGB != 40.0 {
		t.Errorf("fields wrong: %+v", n)
	}
	if n.Uptime != 12345 {
		t.Errorf("uptime = %d, want 12345", n.Uptime)
	}
	// Hostname from /health overrides the caller-supplied name — the
	// Windows agent reports its COMPUTERNAME and must show up as itself.
	if n.Name != "wool" {
		t.Errorf("name = %q, want hostname override", n.Name)
	}
}

func TestPollNodeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now unreachable

	n := pollNode("wool", url+"/health")
	if n.Online {
		t.Fatal("expected offline")
	}
}

func TestPollNodeBadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	if n := pollNode("wool", srv.URL+"/health"); n.Online {
		t.Fatal("expected offline for garbage body")
	}
}

// TestAgentURLDefaults — open-source default is EMPTY (no hardcoded
// internal nets, R24); env overrides flow through.
func TestAgentURLDefaults(t *testing.T) {
	os.Unsetenv("COMPUTE_AGENT_URL")
	if defaultAgentURL() != "" {
		t.Error("defaultAgentURL should be empty without env")
	}
	os.Setenv("COMPUTE_AGENT_URL", "http://agent:19527")
	defer os.Unsetenv("COMPUTE_AGENT_URL")
	if defaultAgentURL() != "http://agent:19527" {
		t.Errorf("defaultAgentURL = %q", defaultAgentURL())
	}

	os.Unsetenv("WOOL_AGENT_URL")
	if woolAgentURL() != "" {
		t.Error("woolAgentURL should be empty without env")
	}
	os.Setenv("WOOL_AGENT_URL", "http://node:19528")
	defer os.Unsetenv("WOOL_AGENT_URL")
	if woolAgentURL() != "http://node:19528" {
		t.Errorf("woolAgentURL = %q", woolAgentURL())
	}
}
