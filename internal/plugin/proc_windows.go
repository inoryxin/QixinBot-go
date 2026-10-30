//go:build windows

package plugin

import (
	"fmt"
	"os/exec"
)

// killProcessTree 杀掉插件进程及其所有子进程。
//
// 插件可能起常驻 sidecar（ffmpeg、worker、模型服务），
// 只杀主进程会留下孤儿。taskkill /T 会连整棵树一起收。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// 先试 taskkill /T（收整棵树），失败再退回单进程杀
	if err := exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprint(cmd.Process.Pid)).Run(); err != nil {
		_ = cmd.Process.Kill()
	}
	// 不在这里 Wait：退出状态由 spawn 时的 cmd.Wait() goroutine 统一收，
	// 两处同时等同一个子进程会拿到错误状态。
}

// setProcGroup 在 Windows 上是空操作（用 taskkill /T 收树）。
func setProcGroup(cmd *exec.Cmd) {}
