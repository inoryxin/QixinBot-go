package plugin

import (
	"sync"
	"testing"
	"time"

	"github.com/xin/onebot/internal/session"
)

// 并发语义是插件作者最关心的问题之一：
//   - 不同插件之间绝不互相等
//   - 同一插件默认串行（作者不用处理并发）
//   - 配了 concurrency = "session" 的话，不同会话并行

func sessOf(chatID, userID int64) *session.Session {
	return &session.Session{
		Account: "main", ChatType: "group", ChatID: chatID, UserID: userID,
	}
}

// fakeHost 造一个带锁的 Host，够测 lockFor。
func fakeHost(concurrency string) *Host {
	h := &Host{callMu: sync.Mutex{}}
	h.Manifest = &Manifest{ID: "test", Concurrency: concurrency}
	return h
}

// 带超时的抢锁：拿到返回 true。
func tryLock(fn func() func(), timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		unlock := fn()
		unlock()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

func TestLockForDefaultIsSerial(t *testing.T) {
	m := &Manager{}
	h := fakeHost("") // 默认串行

	unlock := m.lockFor(h, sessOf(100, 1))
	defer unlock()

	// 另一个群：同一插件默认串行 → 抢不到
	if tryLock(func() func() { return m.lockFor(h, sessOf(200, 2)) }, 50*time.Millisecond) {
		t.Fatal("默认模式下同一插件的另一个会话不该并行")
	}
}

func TestLockForSessionModeParallel(t *testing.T) {
	m := &Manager{}
	h := fakeHost("session")

	unlock := m.lockFor(h, sessOf(100, 1))
	defer unlock()

	// 不同群：session 模式应该能并行
	if !tryLock(func() func() { return m.lockFor(h, sessOf(200, 2)) }, 200*time.Millisecond) {
		t.Fatal("session 模式下不同会话应该并行")
	}
}

func TestLockForSessionModeSameChatSerial(t *testing.T) {
	m := &Manager{}
	h := fakeHost("session")

	unlock := m.lockFor(h, sessOf(100, 1))
	defer unlock()

	// 同一个群：还是要排队
	if tryLock(func() func() { return m.lockFor(h, sessOf(100, 2)) }, 50*time.Millisecond) {
		t.Fatal("同一个群内应该串行")
	}
}

func TestDifferentPluginsDoNotBlock(t *testing.T) {
	m := &Manager{}
	h1 := fakeHost("")
	h2 := fakeHost("")

	unlock1 := m.lockFor(h1, sessOf(100, 1))
	defer unlock1()

	// 另一个插件完全独立
	if !tryLock(func() func() { return m.lockFor(h2, sessOf(100, 1)) }, 200*time.Millisecond) {
		t.Fatal("不同插件之间不该互相阻塞")
	}
}

func TestLockForNilSessionFallsBackToPluginLock(t *testing.T) {
	// 没有会话上下文（比如定时任务）时按插件串行，避免并发改状态
	m := &Manager{}
	h := fakeHost("session")

	unlock := m.lockFor(h, nil)
	defer unlock()

	if tryLock(func() func() { return m.lockFor(h, nil) }, 50*time.Millisecond) {
		t.Fatal("没有会话时应该退回插件级串行")
	}
}
