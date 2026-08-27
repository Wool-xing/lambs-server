package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestDetectStartupCandidates — every detection branch: requirements.txt
// (three main.py layouts), package.json, go.mod, Cargo.toml, and the
// repo-named entry. /home/ubuntu/apps resolves to C:\home\ubuntu\apps on
// Windows (creatable without root), same path CI sees on Linux.
func TestDetectStartupCandidates(t *testing.T) {
	probe := "/home/ubuntu/apps/detect-matrix"
	if err := os.MkdirAll(probe, 0755); err != nil {
		t.Skipf("cannot create /home/ubuntu/apps: %v", err)
	}
	defer os.RemoveAll(probe)

	post := func(body string) (int, string) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			handleDetectStartup(w, r)
		}))
		defer ts.Close()
		resp, err := http.Post(ts.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw := make([]byte, 8192)
		n, _ := resp.Body.Read(raw)
		return resp.StatusCode, string(raw[:n])
	}

	touch := func(rel string) {
		p := probe + "/" + rel
		if err := os.MkdirAll(p[:strings.LastIndex(p, "/")], 0755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte("x"), 0644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	// requirements.txt + app/main.py → uvicorn (app dir)
	touch("requirements.txt")
	touch("app/main.py")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "uvicorn") {
		t.Errorf("app/main.py detect = %d %s", c, b)
	}
	// requirements.txt + bare main.py → uvicorn (repo root)
	os.RemoveAll(probe + "/app")
	touch("main.py")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "uvicorn") {
		t.Errorf("main.py detect = %d %s", c, b)
	}
	// requirements.txt + app/app/main.py → uvicorn (nested app dir)
	os.Remove(probe + "/main.py")
	touch("app/app/main.py")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "uvicorn") {
		t.Errorf("app/app/main.py detect = %d %s", c, b)
	}
	// package.json → npm start
	os.RemoveAll(probe + "/app")
	os.Remove(probe + "/requirements.txt")
	touch("package.json")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "npm start") {
		t.Errorf("package.json detect = %d %s", c, b)
	}
	// go.mod → go run .
	os.Remove(probe + "/package.json")
	touch("go.mod")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "go run .") {
		t.Errorf("go.mod detect = %d %s", c, b)
	}
	// Cargo.toml → cargo run --release
	os.Remove(probe + "/go.mod")
	touch("Cargo.toml")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "cargo run --release") {
		t.Errorf("Cargo.toml detect = %d %s", c, b)
	}
	// repo-named entry under the app dir
	touch("detect-matrix")
	if c, b := post(`{"repo":"detect-matrix"}`); c != 200 || !strings.Contains(b, "detect-matrix/detect-matrix") {
		t.Errorf("repo-named entry detect = %d %s", c, b)
	}
}
