package plugin

import (
	"context"
	"fmt"
	"sync"
)

// capLimiter 限制"同时有多少个调用打向同一个能力的同一个提供者"。
//
// 键是 `provider:capability` 而不是只有 provider——
// 因为 max_concurrent 是按 [[capability]] 声明的，
// 同一个服务的两个能力可以有不同的上限。
//
// 为什么需要：Python SDK 是同步逐条处理的，一个慢请求会把后面全排住；
// Node 虽然能并发，但提供者自己的状态不一定安全。
// 与其让调用方干等 15 秒超时，不如在这里排队。
//
// 只对**声明了 max_concurrent 的提供者**生效，没声明的照旧不限。
type capLimiter struct {
	mu   sync.Mutex
	sems map[string]chan struct{} // "provider:capability" → 信号量
}

func newCapLimiter() *capLimiter {
	return &capLimiter{sems: make(map[string]chan struct{})}
}

// Acquire 拿一个并发名额。返回释放函数。
//
// 名额满时**阻塞等待**（受 ctx 控制）；等不到就返回错误，
// 让调用方知道"是提供者忙不过来"，而不是"提供者坏了"。
func (l *capLimiter) Acquire(ctx context.Context, providerID, capability string, limit int) (func(), error) {
	if limit <= 0 {
		return func() {}, nil // 没限制
	}
	key := providerID + ":" + capability

	l.mu.Lock()
	sem, ok := l.sems[key]
	if !ok || cap(sem) != limit {
		// 限制变了就重建（插件重载后配置可能改）
		sem = make(chan struct{}, limit)
		l.sems[key] = sem
	}
	l.mu.Unlock()

	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("能力 %s 的提供者 %s 并发已满（上限 %d），等待超时",
			capability, providerID, limit)
	}
}

// Forget 清掉某个提供者的所有信号量（插件卸载时）。
//
// 键是 provider:capability，所以要按前缀清。
func (l *capLimiter) Forget(providerID string) {
	prefix := providerID + ":"
	l.mu.Lock()
	for k := range l.sems {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(l.sems, k)
		}
	}
	l.mu.Unlock()
}

// InFlight 返回某个能力当前占用数（监控/测试用）。
func (l *capLimiter) InFlight(providerID, capability string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if sem, ok := l.sems[providerID+":"+capability]; ok {
		return len(sem)
	}
	return 0
}
