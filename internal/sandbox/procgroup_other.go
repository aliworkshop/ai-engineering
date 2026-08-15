//go:build !unix

package sandbox

import "os/exec"

// On platforms without process groups the timeout still fires and the direct
// child is still killed — we just cannot promise its descendants die with it.
// Saying so here is better than a silently weaker sandbox.
func isolateProcessGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
