//go:build windows

package kernel

import "os/exec"

func setSysProcAttr(cmd *exec.Cmd) {}

func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func processAlive(cmd *exec.Cmd) bool { return false }
