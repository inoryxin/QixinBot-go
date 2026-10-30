//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"time"
)

// killTree 在 Windows 上用 taskkill /T 连子进程一起收。
//
// **必须有超时**：taskkill /T 会等整棵进程树退出，
// 如果有子进程不响应，它会卡很久（验证跑完不退出就是这个原因）。
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := fmt.Sprint(cmd.Process.Pid)

	done := make(chan struct{})
	go func() {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// taskkill 卡住了：直接杀主进程，别让整个 verify 挂着
		_ = cmd.Process.Kill()
	}

	// 收尸：再给 5 秒，拿不到就放弃（进程已经杀了，只是 Wait 没返回）
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
