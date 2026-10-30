package plugin

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// askRegistry 管理"等待用户下一条消息"的请求。
//
// 典型用法（绑定账号、分步设置、小游戏）：
//
//	const city = await host.ask(chatOf(session), '请输入城市名（60 秒内回复）')
//	if (!city) return { handled: true }   // 用户没回或超时
//
// 宿主收到消息时先查这里：命中就投给等待者，不再走插件分发。
type askRegistry struct {
	mu      sync.Mutex
	pending map[string]*askWaiter // key = chatType:chatID:userID
}

type askWaiter struct {
	plugin string
	ch     chan string
	// cancelled 关闭表示这次等待被用户/插件主动取消：
	// Ask 收到后立刻返回，而不是一直挂到超时。
	cancelled chan struct{}
}

func newAskRegistry() *askRegistry {
	return &askRegistry{pending: make(map[string]*askWaiter)}
}

// askKey 用 账号+会话+用户 做键：同一个群里，不同人各自等各自的，
// 不同账号（同一群号）也互不串——群号在两个账号下是两个独立会话。
func askKey(account, chatType string, chatID, userID int64) string {
	return fmt.Sprintf("%s|%s:%d:%d", account, chatType, chatID, userID)
}

// Register 登记一个等待，返回接收通道和取消函数。
func (r *askRegistry) Register(plugin, account, chatType string, chatID, userID int64) (<-chan string, <-chan struct{}, func()) {
	key := askKey(account, chatType, chatID, userID)
	w := &askWaiter{plugin: plugin, ch: make(chan string, 1), cancelled: make(chan struct{})}

	r.mu.Lock()
	if old, ok := r.pending[key]; ok {
		// 同一用户注册新的等待：旧的那次必须立刻结束，不能"失去登记还继续阻塞"
		close(old.cancelled)
	}
	r.pending[key] = w
	r.mu.Unlock()

	return w.ch, w.cancelled, func() {
		r.mu.Lock()
		if cur, ok := r.pending[key]; ok && cur == w {
			delete(r.pending, key)
			close(w.cancelled)
		}
		r.mu.Unlock()
	}
}

// Deliver 尝试把消息投给等待者。返回 true 表示被消费了（不要再分发）。
func (r *askRegistry) Deliver(account, chatType string, chatID, userID int64, text string) bool {
	key := askKey(account, chatType, chatID, userID)

	r.mu.Lock()
	w, ok := r.pending[key]
	if ok {
		delete(r.pending, key) // 一次只能答一次
	}
	r.mu.Unlock()

	if !ok {
		return false
	}
	select {
	case w.ch <- text:
	default:
	}
	return true
}

// CancelOne 取消某个人当前的等待。返回是否真的取消了。
//
// /取消 和 /cancel 都认——用户在交互流程里想退出时用。
func (r *askRegistry) CancelOne(account, chatType string, chatID, userID int64) bool {
	key := askKey(account, chatType, chatID, userID)
	r.mu.Lock()
	w, ok := r.pending[key]
	if ok {
		delete(r.pending, key)
		close(w.cancelled)
	}
	r.mu.Unlock()
	return ok
}

// IsCancelWord 判断这句话是不是"取消"。
func IsCancelWord(text string) bool {
	switch text {
	case "取消", "cancel", "退出", "算了", "不弄了":
		return true
	}
	return false
}

// CancelPlugin 清掉某个插件所有等待（插件卸载时用）。
func (r *askRegistry) CancelPlugin(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for k, w := range r.pending {
		if w.plugin == id {
			delete(r.pending, k)
			close(w.cancelled)
			n++
		}
	}
	return n
}

// Count 返回当前等待数。
func (r *askRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

// RegisterAsk 只登记等待，不发消息——让调用方自己控制"先登记再发提示"的时序。
//
// 这是 host.ask 的正确用法：先登记，再发提示语，
// 否则用户在空隙里秒回的消息会走正常分发，等待者收不到。
func (m *Manager) RegisterAsk(pluginID, account, chatType string, chatID, userID int64) (<-chan string, <-chan struct{}, func()) {
	if m.asks == nil {
		ch := make(chan string)
		close(ch)
		closed := make(chan struct{})
		close(closed)
		return ch, closed, func() {}
	}
	return m.asks.Register(pluginID, account, chatType, chatID, userID)
}

// Ask 是 host.ask 的旧实现：发提示语，然后等用户下一条消息。
//
// Deprecated: 用 RegisterAsk + 自己发提示，避免"先发后等"漏掉秒回。
//
// 超时返回空字符串（不报错），插件按"用户没回"处理就行。
func (m *Manager) Ask(ctx context.Context, pluginID, account, chatType string, chatID, userID int64, timeout time.Duration) (string, error) {
	if m.asks == nil {
		return "", fmt.Errorf("宿主未启用交互等待")
	}
	ch, cancelled, cancel := m.asks.Register(pluginID, account, chatType, chatID, userID)
	defer cancel()

	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case text := <-ch:
		return text, nil
	case <-cancelled:
		return "", nil // 用户按了取消：立刻返回，不挂到超时
	case <-timer.C:
		return "", nil // 超时不算错误，插件当"没回答"处理
	case <-ctx.Done():
		return "", ctx.Err()
	}
}
