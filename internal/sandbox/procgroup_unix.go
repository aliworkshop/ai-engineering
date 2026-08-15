//go:build unix

package sandbox

import (
	"os/exec"
	"syscall"
)

// isolateProcessGroup puts the child in a process group of its own. Without
// this, killing the child leaves anything it spawned still running — and the
// first thing a runaway script does is spawn something.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killGroup signals the whole group. The negative pid is the Unix spelling of
// "this group, not just this process".
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill() // group gone already; fall back to the child
	}
	return nil
}
