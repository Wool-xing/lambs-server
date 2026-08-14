//go:build linux

package runtime

import (
	"os/exec"
	"syscall"
)

// setProcGroup puts the child in its own process group so that
// killGroup can terminate the whole tree.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the process group headed by pid.
func killGroup(pid int, sig syscall.Signal) {
	syscall.Kill(-pid, sig)
}
