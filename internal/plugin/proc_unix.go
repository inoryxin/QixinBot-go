//go:build !windows

package plugin

import (
	"os/exec"
	"syscall"
)

// killProcessTree 杀掉插件进程及其所有子进程。
//
// 因为 Start 时设了 Setpgid，插件和它的子进程在同一个进程组里，
// kill(-pid) 能一次收干净。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		_ = cmd.Process.Kill()
	}
	// 不在这里 Wait：退出状态由 spawn 时的 cmd.Wait() goroutine 统一收，
	// 两处同时等同一个子进程会拿到错误状态。
}

// setProcGroup 让插件进程自成一组，方便整组回收。
func setProcGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
