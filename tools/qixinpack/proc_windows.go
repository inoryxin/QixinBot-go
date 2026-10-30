//go:build windows

package main

import "os/exec"

// setProcGroup 在 Windows 上是空操作（用 taskkill /T 收树）。
func setProcGroup(cmd *exec.Cmd) {}
