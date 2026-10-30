package plugin

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Stats 是单个插件的运行统计。
//
// 插件作者最常问的三个问题，这里都能回答：
//   - 我的插件被调用了吗？      → Calls
//   - 为什么慢？               → AvgMS / MaxMS / LastMS
//   - 是不是崩过？             → Crashes / LastError
type Stats struct {
	// 消息分发
	Calls    int64 `json:"calls"`    // 被调用次数
	Handled  int64 `json:"handled"`  // 认领次数（返回 handled:true）
	Timeouts int64 `json:"timeouts"` // 超时次数（插件太慢）
	Cancels  int64 `json:"cancels"`  // 被取消次数（框架关闭等，不算插件问题）
	Failures int64 `json:"failures"` // 报错次数

	// 耗时（毫秒）
	LastMS  int64 `json:"last_ms"`
	MaxMS   int64 `json:"max_ms"`
	TotalMS int64 `json:"-"` // 用于算平均

	// 生命周期
	Crashes     int64     `json:"crashes"`  // 崩溃重启次数
	Restarts    int64     `json:"restarts"` // 主动重载次数
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	LastCallAt  time.Time `json:"last_call_at,omitempty"`
}

// AvgMS 返回平均耗时。
func (s Stats) AvgMS() int64 {
	if s.Calls == 0 {
		return 0
	}
	return s.TotalMS / s.Calls
}

// statsBox 是带锁的统计表。
type statsBox struct {
	mu sync.Mutex
	m  map[string]*Stats
}

func newStatsBox() *statsBox {
	return &statsBox{m: make(map[string]*Stats)}
}

// recordCall 记一次调用。
func (b *statsBox) recordCall(id string, took time.Duration, handled bool, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[id]
	if !ok {
		s = &Stats{}
		b.m[id] = s
	}
	ms := took.Milliseconds()
	s.Calls++
	s.LastMS = ms
	s.TotalMS += ms
	if ms > s.MaxMS {
		s.MaxMS = ms
	}
	s.LastCallAt = time.Now()
	if handled {
		s.Handled++
	}
	if err != nil {
		s.Failures++
		// 超时和取消分开记：
		//   DeadlineExceeded = 插件处理太慢（要优化或调超时）
		//   Canceled        = 调用方主动取消（比如框架关闭），不算插件的问题
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			s.Timeouts++
		case errors.Is(err, context.Canceled):
			s.Cancels++
		}
		s.LastError = err.Error()
		s.LastErrorAt = time.Now()
	}
}

// recordCrash 记一次崩溃。
func (b *statsBox) recordCrash(id string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[id]
	if !ok {
		s = &Stats{}
		b.m[id] = s
	}
	s.Crashes++
	if err != nil {
		s.LastError = err.Error()
		s.LastErrorAt = time.Now()
	}
}

// recordRestart 记一次主动重载。
func (b *statsBox) recordRestart(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[id]
	if !ok {
		s = &Stats{}
		b.m[id] = s
	}
	s.Restarts++
}

// snapshot 返回所有插件的统计副本（给 WebUI 用）。
func (b *statsBox) snapshot() map[string]Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]Stats, len(b.m))
	for id, s := range b.m {
		out[id] = *s
	}
	return out
}

// Stats 返回某个插件的统计。
func (m *Manager) Stats(id string) Stats {
	if m.stats == nil {
		return Stats{}
	}
	m.stats.mu.Lock()
	defer m.stats.mu.Unlock()
	if s, ok := m.stats.m[id]; ok {
		return *s
	}
	return Stats{}
}

// AllStats 返回所有插件的统计。
func (m *Manager) AllStats() map[string]Stats {
	if m.stats == nil {
		return nil
	}
	return m.stats.snapshot()
}
