// Package metrics 记录框架运行指标的 24 小时时间序列。
//
// 为什么需要它：出问题时只有"现在的快照"看不出"几点开始坏的"。
// 按 5 分钟一个窗口滚动 24 小时（288 个窗口），消息速率、插件延迟、
// 内存这些在概览页画成曲线，故障时间点一眼可见。
//
// 全在内存里，重启清零——这是有意为之：它用于排障，不用于对账。
package metrics

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
)

// WindowSec 是每个窗口的时长。
const WindowSec = 300

// windows 是 24 小时（288 × 5min）。
const windows = 288

// Window 是一个已完成窗口的聚合值。
type Window struct {
	Start int64 `json:"start"` // 窗口起始 unix 秒
	MsgIn int   `json:"msg_in"`
	Sent  int   `json:"sent"`

	Calls int   `json:"calls"`            // 插件调用次数（消息/事件分发的 handle_message）
	Errs  int   `json:"errs"`             // 失败次数
	AvgMS int64 `json:"avg_ms,omitempty"` // 平均耗时
	MaxMS int64 `json:"max_ms,omitempty"` // 最慢一次
	MemMB int64 `json:"mem_mb,omitempty"` // 窗口内自驻内存峰值
	Depth int   `json:"depth,omitempty"`  // 发送队列峰值
}

type accum struct {
	start time.Time
	msgIn int
	sent  int
	calls int
	errs  int
	msSum int64
	msMax int64
	memMB float64
	depth int
}

// Collector 聚合当前窗口并滚动历史。
type Collector struct {
	mu       sync.Mutex
	ring     [windows]Window
	cur      accum
	selfProc *process.Process

	depth func() int // 发送队列深度采样（由 main 注入，避免反向依赖 send 包）
}

// New 创建采集器。depth 采样函数由调用方注入（可为 nil）。
func New(depth func() int) *Collector {
	c := &Collector{depth: depth}
	c.cur.start = time.Now() // 不设的话首窗 Start=0，Snapshot 会把前 5 分钟的数据滤掉
	if p, err := process.NewProcess(int32(os.Getpid())); err == nil {
		c.selfProc = p
	}
	return c
}

// Start 起滚动与采样循环，ctx 取消时退出。
func (c *Collector) Start(ctx context.Context) {
	go func() {
		roll := time.NewTicker(WindowSec * time.Second)
		defer roll.Stop()
		sample := time.NewTicker(30 * time.Second)
		defer sample.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-roll.C:
				c.roll()
			case <-sample.C:
				c.sample()
			}
		}
	}()
}

// MsgIn 记一条收到并进入处理的消息。
func (c *Collector) MsgIn() {
	c.mu.Lock()
	c.cur.msgIn++
	c.mu.Unlock()
}

// Sent 记一条成功发出的消息。
func (c *Collector) Sent() {
	c.mu.Lock()
	c.cur.sent++
	c.mu.Unlock()
}

// PluginCall 记一次插件调用（handle_message）。ms 毫秒，failed 是否失败。
func (c *Collector) PluginCall(ms int64, failed bool) {
	c.mu.Lock()
	c.cur.calls++
	if failed {
		c.cur.errs++
	}
	c.cur.msSum += ms
	if ms > c.cur.msMax {
		c.cur.msMax = ms
	}
	c.mu.Unlock()
}

// roll 把当前窗口压进环形数组，开新窗口。
func (c *Collector) roll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := Window{Start: c.cur.start.Unix(), MsgIn: c.cur.msgIn, Sent: c.cur.sent,
		Calls: c.cur.calls, Errs: c.cur.errs, MaxMS: c.cur.msMax,
		MemMB: int64(c.cur.memMB + 0.5), Depth: c.cur.depth}
	if c.cur.calls > 0 {
		w.AvgMS = c.cur.msSum / int64(c.cur.calls)
	}
	if w.MsgIn > 0 || w.Sent > 0 || w.Calls > 0 {
		// 有数据的窗口才进环形数组：全空窗口（半夜）不占历史
		copy(c.ring[:], c.ring[1:])
		c.ring[windows-1] = w
	}
	c.cur = accum{start: time.Now()}
}

// sample 采当前窗口的内存/队列深度峰值。
func (c *Collector) sample() {
	var mb float64
	if c.selfProc != nil {
		if mem, err := c.selfProc.MemoryInfo(); err == nil && mem != nil {
			mb = float64(mem.RSS) / 1024 / 1024
		}
	}
	c.mu.Lock()
	if mb > c.cur.memMB {
		c.cur.memMB = mb
	}
	if c.depth != nil {
		if d := c.depth(); d > c.cur.depth {
			c.cur.depth = d
		}
	}
	c.mu.Unlock()
}

// Snapshot 返回有数据的窗口（时间正序），给 /api/metrics。
func (c *Collector) Snapshot() []Window {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Window, 0, windows)
	for _, w := range c.ring {
		if w.Start != 0 {
			out = append(out, w)
		}
	}
	return out
}
