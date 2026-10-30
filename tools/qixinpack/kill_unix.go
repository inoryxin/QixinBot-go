//go:build !windows

package main

import (
	"os/exec"
	"syscall"
	"time"
)

// killTree 在 Unix 上杀整个进程组。
//
// 启动子进程时设了 Setpgid（见 setProcGroup），所以 kill(-pid) 一次收干净。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()

	waited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
	}
}

// setProcGroup 让子进程自成一组。
func setProcGroup(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}
