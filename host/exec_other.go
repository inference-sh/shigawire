//go:build !unix

package host

import (
	"os/exec"
	"time"
)

// killGroup: no process groups here; a cancel kills the command itself.
func killGroup(cmd *exec.Cmd, grace time.Duration) { cmd.WaitDelay = grace }
