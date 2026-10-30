package plugin

import (
	"github.com/shirou/gopsutil/v4/process"
)

// processRSSMB 查询进程常驻内存（MB）。失败或进程已退出返回 0。
// gopsutil 跨平台：Windows 走 psapi，Linux 走 /proc——monitor.go 统计用的是同一套。
func processRSSMB(pid int) float64 {
	if pid <= 0 {
		return 0
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0
	}
	mi, err := p.MemoryInfo()
	if err != nil {
		return 0
	}
	return float64(mi.RSS) / 1024 / 1024
}
