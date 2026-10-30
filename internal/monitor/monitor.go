// Package monitor 采集宿主机的资源占用，给控制台展示。
//
// 为什么要看这个：插件是独立进程，跑飞了宿主不一定知道，
// 但 CPU 和内存会先报警。先看见，再决定停哪个插件。
package monitor

import (
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// Snapshot 是一次采样结果。
type Snapshot struct {
	// CPU
	CPUPercent float64 `json:"cpu_percent"` // 0-100
	CPUCores   int     `json:"cpu_cores"`

	// 内存（字节）
	MemTotal   uint64  `json:"mem_total"`
	MemUsed    uint64  `json:"mem_used"`
	MemPercent float64 `json:"mem_percent"`

	// 磁盘（框架所在分区）
	DiskTotal   uint64  `json:"disk_total"`
	DiskUsed    uint64  `json:"disk_used"`
	DiskPercent float64 `json:"disk_percent"`
	DiskPath    string  `json:"disk_path"`

	// 框架进程自己
	SelfRSS    uint64 `json:"self_rss"` // 常驻内存
	GoRoutines int    `json:"goroutines"`

	// 插件子进程（每个插件一个进程，内存要单独算）
	PluginRSS   uint64 `json:"plugin_rss"`
	PluginProcs int    `json:"plugin_procs"`

	// 元信息
	OS   string `json:"os"`
	Arch string `json:"arch"`
	At   int64  `json:"at"`
}

// Sampler 负责定期采样。
//
// CPU 百分比需要两次采样求差，所以第一次调用会返回 0，
// 之后 caches 才有意义。
type Sampler struct {
	mu       sync.RWMutex
	last     Snapshot
	diskPath string
	hasPrev  bool
}

// New 创建采样器。diskPath 是要监控的分区（一般用数据目录）。
func New(diskPath string) *Sampler {
	return &Sampler{diskPath: diskPath}
}

// Sample 采一次。CPU 用非阻塞的 cpu.Percent(0)。
func (s *Sampler) Sample() Snapshot {
	out := Snapshot{
		At:   time.Now().Unix(),
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
	}

	if p, err := cpu.Percent(0, false); err == nil && len(p) > 0 {
		out.CPUPercent = round1(p[0])
	}
	out.CPUCores = runtime.NumCPU()

	if vm, err := mem.VirtualMemory(); err == nil {
		out.MemTotal = vm.Total
		out.MemUsed = vm.Used
		out.MemPercent = round1(vm.UsedPercent)
	}

	if du, err := disk.Usage(s.diskPath); err == nil {
		out.DiskTotal = du.Total
		out.DiskUsed = du.Used
		out.DiskPercent = round1(du.UsedPercent)
		out.DiskPath = s.diskPath
	}

	// 框架自己的常驻内存（跨平台，gopsutil 内部处理了各系统差异）
	if p, err := process.NewProcess(int32(os.Getpid())); err == nil {
		if mi, err := p.MemoryInfo(); err == nil {
			out.SelfRSS = mi.RSS
		}
	}
	out.GoRoutines = runtime.NumGoroutine()

	// 插件是独立子进程，不算进 SelfRSS；按父进程 pid 汇总
	if procs, err := process.Processes(); err == nil {
		self := int32(os.Getpid())
		for _, pr := range procs {
			ppid, err := pr.Ppid()
			if err != nil || ppid != self {
				continue
			}
			mi, err := pr.MemoryInfo()
			if err != nil {
				continue
			}
			out.PluginRSS += mi.RSS
			out.PluginProcs++
		}
	}

	s.mu.Lock()
	s.last = out
	s.hasPrev = true
	s.mu.Unlock()
	return out
}

// Last 返回最近一次采样（不触发新的采集）。
func (s *Sampler) Last() (Snapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.last, s.hasPrev
}

// Run 定期采样，直到 ctx 结束。
func (s *Sampler) Run(stop <-chan struct{}, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	s.Sample() // 先采一次，避免页面首次打开是空的
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.Sample()
		}
	}
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}
