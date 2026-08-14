//go:build windows

package runtime

import (
	"os"
	"os/exec"
	"syscall"
)

// setProcGroup is a no-op on Windows (process groups are POSIX-only).
func setProcGroup(cmd *exec.Cmd) {}

// killGroup terminates the single process (no group semantics on Windows).
func killGroup(pid int, sig syscall.Signal) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}
