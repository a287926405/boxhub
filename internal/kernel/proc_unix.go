//go:build !windows

package kernel

import (
	"os"
	"os/exec"
	"syscall"
)

// setSysProcAttr 让内核运行在独立进程组里,停止时可以整组清理。
func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcess 强杀整个进程组。
func killProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

// processAlive 判断进程是否还活着。
func processAlive(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	// Signal(0) 只做权限/存在性检查,不会真的发信号
	err := cmd.Process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	// 进程已退出时返回 ESRCH;其它错误(如权限)按存活处理
	return !os.IsTimeout(err) && err != syscall.ESRCH && err != nil && err.Error() != "os: process already finished"
}
