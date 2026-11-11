//go:build unix

package host

import (
	"os/exec"
	"syscall"
	"time"
)

// killGroup starts cmd as the leader of a process group of its own, and
// makes its cancel end the whole group: SIGTERM, then SIGKILL after grace.
func killGroup(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		pgid := cmd.Process.Pid
		time.AfterFunc(grace, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		return syscall.Kill(-pgid, syscall.SIGTERM)
	}
	// Past the SIGKILL: Wait returns, though something still holds the
	// output open.
	cmd.WaitDelay = grace + time.Second
}
