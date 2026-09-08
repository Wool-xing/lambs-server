// Package deploy runs remote deployment commands on registered machines over
// the dedicated lambs-deploy SSH key. Local deployments (host lambs) run
// directly without SSH.
package deploy

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"lambs-server-go/internal/db"
	"lambs-server-go/internal/execpath"
	"lambs-server-go/internal/models"
)

// sshRun executes one remote command via the deploy key; tests swap it out.
var sshRun = func(user, host, key, remoteCmd string, stdin []byte) ([]byte, error) {
	args := []string{"-i", key, "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new", "-o", "ConnectTimeout=8",
		user + "@" + host, remoteCmd}
	cmd := exec.Command(execpath.Path("ssh"), args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), err
}

func deployKey() string {
	if k := os.Getenv("LAMBS_DEPLOY_KEY"); k != "" {
		return k
	}
	return "/home/ubuntu/.ssh/lambs-deploy"
}

// MachineConn resolves a machine's ssh user and tailscale IP from the registry.
func MachineConn(hostID string) (user, host string, err error) {
	if hostID == "" || hostID == "lambs" {
		return "", "", fmt.Errorf("local machine: %s", hostID)
	}
	var sshUser, tsIP string
	err = db.DB.QueryRow(`SELECT COALESCE(ssh_user,''), COALESCE(ts_ip,'') FROM machines WHERE id=$1`, hostID).Scan(&sshUser, &tsIP)
	if err != nil {
		return "", "", fmt.Errorf("machine %s not in registry: %v", hostID, err)
	}
	if tsIP == "" {
		return "", "", fmt.Errorf("machine %s has no ts_ip", hostID)
	}
	if sshUser == "" {
		sshUser = strings.ToLower(hostID)
	}
	return sshUser, tsIP, nil
}

// Run executes cmd on the given machine. Local machine (lambs) runs directly;
// anything else goes through the deploy key SSH channel.
func Run(hostID, cmd string, stdin []byte) ([]byte, error) {
	if hostID == "" || hostID == "lambs" {
		c := exec.Command("/bin/bash", "-c", cmd)
		if stdin != nil {
			c.Stdin = bytes.NewReader(stdin)
		}
		var out bytes.Buffer
		c.Stdout = &out
		c.Stderr = &out
		return out.Bytes(), c.Run()
	}
	user, host, err := MachineConn(hostID)
	if err != nil {
		return nil, err
	}
	return sshRun(user, host, deployKey(), cmd, stdin)
}

// DeployLinuxService deploys one component on a linux machine: clone (when a
// git url is set), write a systemd unit, enable and start it. An empty
// startCmd means "code only" — the unit is skipped.
func DeployLinuxService(hostID, projectID, svcName, appDir, gitURL, startCmd string) error {
	if !regexp.MustCompile(`^[a-zA-Z0-9._-]+$`).MatchString(svcName) {
		return fmt.Errorf("service name %q contains unsafe characters", svcName)
	}
	user, _, err := MachineConn(hostID)
	if err != nil {
		return err
	}
	clone := fmt.Sprintf("mkdir -p %s", appDir)
	if gitURL != "" {
		if strings.ContainsAny(gitURL, "'\n") {
			return fmt.Errorf("git_url contains unsafe characters")
		}
		clone = fmt.Sprintf("if [ -d %s/.git ]; then cd %s && git pull --ff-only; else git clone --depth 1 '%s' %s; fi",
			appDir, appDir, gitURL, appDir)
	}
	if out, err := Run(hostID, clone, nil); err != nil {
		return fmt.Errorf("clone: %v: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(startCmd) == "" {
		return nil // code landed; no start command supplied (manual start)
	}
	// ExecStart runs through bash so shell syntax (&&, env vars) behaves the
	// same as the local procmgr path.
	unit := fmt.Sprintf("[Unit]\nDescription=Lambs managed: %s/%s\nAfter=network.target\n\n[Service]\nUser=%s\nWorkingDirectory=%s\nExecStart=/bin/bash -lc '%s'\nRestart=always\nRestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n",
		projectID, svcName, user, appDir, strings.ReplaceAll(startCmd, "'", "'\\''"))
	unitName := "lambs-" + projectID + "-" + svcName + ".service"
	if out, err := Run(hostID, "sudo tee /etc/systemd/system/"+unitName+" > /dev/null", []byte(unit)); err != nil {
		return fmt.Errorf("write unit: %v: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := Run(hostID, "sudo systemctl daemon-reload && sudo systemctl enable --now "+unitName, nil)
	if err != nil {
		return fmt.Errorf("start: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// DeployWindowsService starts a component on the windows compute-agent. Code
// must already be in place locally; Lambs only triggers the start command.
func DeployWindowsService(startCmd string) error {
	url := os.Getenv("COMPUTE_AGENT_URL")
	token := os.Getenv("COMPUTE_AGENT_TOKEN")
	if url == "" || token == "" {
		return fmt.Errorf("COMPUTE_AGENT_URL/TOKEN unset")
	}
	payload, _ := json.Marshal(map[string]interface{}{"cmd": startCmd, "timeout": 120})
	req, err := http.NewRequest("POST", strings.TrimSuffix(url, "/")+"/cmd", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("compute-agent: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("compute-agent %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// DeployProject deploys a project's main component to its registered machine.
func DeployProject(p models.Project) error {
	user, _, err := MachineConn(p.Host)
	if err != nil {
		return err
	}
	return DeployLinuxService(p.Host, p.ID, "app", fmt.Sprintf("/home/%s/apps/%s", user, p.ID), p.GitURL, p.StartupCommand)
}

// DeployServices deploys every component of a multi-component project. Each
// service may override host and git url; windows-svc goes through the
// compute-agent channel, everything else through linux systemd units.
func DeployServices(p models.Project, svcs []models.ServiceComponent) error {
	for _, svc := range svcs {
		if svc.Name == "" || svc.StartCmd == "" {
			continue
		}
		host := svc.Host
		if host == "" {
			host = p.Host
		}
		var err error
		if svc.Type == "windows-svc" {
			err = DeployWindowsService(svc.StartCmd)
		} else {
			user, _, cerr := MachineConn(host)
			if cerr != nil {
				err = cerr
			} else {
				err = DeployLinuxService(host, p.ID, svc.Name,
					fmt.Sprintf("/home/%s/apps/%s/%s", user, p.ID, svc.Name),
					svc.GitURL, svc.StartCmd)
			}
		}
		if err != nil {
			return fmt.Errorf("service %s: %v", svc.Name, err)
		}
	}
	return nil
}

// UpdateRemote pulls the latest code for a remote project, restarts its unit,
// checks the health URL, and rolls back to the previous commit on failure.
// Returns (changed, error). Health checks run from lambs over tailscale.
func UpdateRemote(p models.Project) (bool, error) {
	user, _, err := MachineConn(p.Host)
	if err != nil {
		return false, err
	}
	appDir := fmt.Sprintf("/home/%s/apps/%s", user, p.ID)
	gitCmd := fmt.Sprintf("cd %s && git fetch origin && git rev-parse HEAD && git rev-parse @{u} 2>/dev/null", appDir)
	out, err := Run(p.Host, gitCmd, nil)
	if err != nil {
		return false, fmt.Errorf("fetch: %v: %s", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Fields(string(out))
	if len(lines) < 2 || lines[len(lines)-2] == lines[len(lines)-1] {
		return false, nil // no change
	}
	oldSHA := lines[len(lines)-2]
	unitName := "lambs-" + p.ID + "-app.service"
	if out, err := Run(p.Host, fmt.Sprintf("cd %s && git merge --ff-only origin && sudo systemctl restart %s", appDir, unitName), nil); err != nil {
		return false, fmt.Errorf("update: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Health gate: GET ts_ip:port + health_url from lambs, with retries —
	// systemd restart returning does not mean the port is bound yet.
	if p.HealthURL != "" && p.Port != "" {
		var tsIP string
		if err := db.DB.QueryRow(`SELECT COALESCE(ts_ip,'') FROM machines WHERE id=$1`, p.Host).Scan(&tsIP); err == nil && tsIP != "" {
			url := "http://" + tsIP + ":" + p.Port + p.HealthURL
			client := &http.Client{Timeout: 8 * time.Second}
			healthy := false
			var lastErr error
			for attempt := 0; attempt < 3; attempt++ {
				if attempt > 0 {
					time.Sleep(2 * time.Second)
				}
				resp, err := client.Get(url)
				if err == nil {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode < 400 {
						healthy = true
						break
					}
					lastErr = fmt.Errorf("status %d", resp.StatusCode)
				} else {
					lastErr = err
				}
			}
			if !healthy {
				// Roll back to the previous commit.
				if out2, err2 := Run(p.Host, fmt.Sprintf("cd %s && git reset --hard %s && sudo systemctl restart %s", appDir, oldSHA, unitName), nil); err2 != nil {
					return false, fmt.Errorf("health check failed (%v) and rollback failed: %v: %s", lastErr, err2, strings.TrimSpace(string(out2)))
				}
				return false, fmt.Errorf("health check failed (%v), rolled back to %s", lastErr, oldSHA[:8])
			}
		}
	}
	return true, nil
}

// StartAutoUpdater polls projects with auto_update enabled every 10 minutes,
// pulling changes through the same health-gated path as manual updates.
func StartAutoUpdater() {
	for {
		rows, err := db.DB.Query(`SELECT id, COALESCE(host,''), COALESCE(port,''), COALESCE(health_url,'') FROM projects WHERE auto_update = true AND status = 'online'`)
		if err == nil {
			for rows.Next() {
				var p models.Project
				if rows.Scan(&p.ID, &p.Host, &p.Port, &p.HealthURL) == nil {
					if p.Host == "" || p.Host == "lambs" {
						continue
					}
					if changed, err := UpdateRemote(p); err != nil {
						println("autoupdate", p.ID, "err:", err.Error())
					} else if changed {
						println("autoupdate", p.ID, "updated")
					}
				}
			}
			rows.Close()
		}
		time.Sleep(10 * time.Minute)
	}
}

// randomHex returns n random bytes as hex (database passwords).
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// CreateDatabase provisions an isolated database and user for a project on
// the lambs postgres instance (superuser-capable lambs_admin runs the DDL).
// Returns the DSN the project should store; the host is lambs' tailscale IP
// from the machines registry so remote projects can reach it.
func CreateDatabase(projectID string) (string, error) {
	raw := os.Getenv("DATABASE_URL")
	if raw == "" {
		return "", fmt.Errorf("DATABASE_URL unset")
	}
	u, err := url.Parse(strings.Replace(raw, "postgresql+asyncpg", "postgresql", 1))
	if err != nil {
		return "", err
	}
	pass, _ := u.User.Password()
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	dbName := `"app_` + projectID + `"`
	user := `"u_` + projectID + `"`
	password := randomHex(12)
	if password == "" {
		return "", fmt.Errorf("random password generation failed")
	}
	// psql runs each -c in its own transaction; CREATE DATABASE cannot share
	// a transaction, so separate invocations.
	base := []string{"-h", host, "-p", port, "-U", "lambs_admin", "-d", "postgres", "-v", "ON_ERROR_STOP=1"}
	env := append(os.Environ(), "PGPASSWORD="+pass)
	cmd1 := exec.Command(execpath.Path("psql"), append(base, "-c",
		fmt.Sprintf("CREATE USER %s WITH PASSWORD '%s'", user, password))...)
	cmd1.Env = env
	if out, err := cmd1.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "already exists") {
			// Re-set the password so a re-run stays consistent with the DSN
			// returned to the caller.
			cmd1b := exec.Command(execpath.Path("psql"), append(base, "-c",
				fmt.Sprintf("ALTER USER %s WITH PASSWORD '%s'", user, password))...)
			cmd1b.Env = env
			if out2, err2 := cmd1b.CombinedOutput(); err2 != nil {
				return "", fmt.Errorf("alter user: %v: %s", err2, strings.TrimSpace(string(out2)))
			}
		} else {
			return "", fmt.Errorf("create user: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	cmd2 := exec.Command(execpath.Path("psql"), append(base, "-c",
		fmt.Sprintf("CREATE DATABASE %s", dbName))...)
	cmd2.Env = env
	if out, err := cmd2.CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "already exists") {
			return "", fmt.Errorf("create database: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	// Non-superusers cannot hand ownership to another role (SET ROLE
	// restriction), so the database stays lambs_admin-owned and the project
	// user gets full access on the public schema instead.
	baseDB := []string{"-h", host, "-p", port, "-U", "lambs_admin", "-d", strings.Trim(dbName, `"`), "-v", "ON_ERROR_STOP=1"}
	cmd3 := exec.Command(execpath.Path("psql"), append(baseDB, "-c",
		fmt.Sprintf("GRANT ALL ON SCHEMA public TO %s", user))...)
	cmd3.Env = env
	if out, err := cmd3.CombinedOutput(); err != nil {
		return "", fmt.Errorf("grant schema: %v: %s", err, strings.TrimSpace(string(out)))
	}
	// Reuse the existing user's password on conflict so repeat runs stay
	// consistent with what callers already stored.
	var tsIP string
	if err := db.DB.QueryRow(`SELECT COALESCE(ts_ip,'') FROM machines WHERE id='lambs'`).Scan(&tsIP); err != nil || tsIP == "" {
		tsIP = "127.0.0.1"
	}
	dsn := fmt.Sprintf("postgresql://u_%s:%s@%s:%s/app_%s", projectID, password, tsIP, port, projectID)
	return dsn, nil
}
