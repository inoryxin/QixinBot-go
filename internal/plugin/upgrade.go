package plugin

// 升级窗口：替换文件期间阻塞拉起与等待。

import (
	"context"
	"fmt"
	"time"
)

// beginUpgrade 标记插件进入升级窗口（安装器替换文件期间）。
func (m *Manager) beginUpgrade(id string) {
	m.mu.Lock()
	if _, ok := m.upgrading[id]; !ok {
		m.upgrading[id] = make(chan struct{})
	}
	m.mu.Unlock()
}

// finishUpgrade 结束升级窗口，放行等待中的拉起。

// finishUpgrade 结束升级窗口，放行等待中的拉起。
func (m *Manager) finishUpgrade(id string) {
	m.mu.Lock()
	if ch, ok := m.upgrading[id]; ok {
		close(ch)
		delete(m.upgrading, id)
	}
	m.mu.Unlock()
}

// AbortUpgrade 升级失败时也要解除等待标记，否则拉起会一直卡着。

// AbortUpgrade 升级失败时也要解除等待标记，否则拉起会一直卡着。
func (m *Manager) AbortUpgrade(id string) { m.finishUpgrade(id) }

// MarkUpgraded 供安装器/重载流程标记"刚升级成功"，用于监视器去重。

// MarkUpgraded 供安装器/重载流程标记"刚升级成功"，用于监视器去重。
func (m *Manager) MarkUpgraded(id string) { m.markUpgraded(id) }

// markUpgraded 记录升级时间（供运行区监视器做去重）。

// markUpgraded 记录升级时间（供运行区监视器做去重）。
func (m *Manager) markUpgraded(id string) {
	m.mu.Lock()
	m.upgradedAt[id] = time.Now()
	m.mu.Unlock()
}

// upgradingNow 判断插件是否正处于升级替换窗口。

// upgradingNow 判断插件是否正处于升级替换窗口。
func (m *Manager) upgradingNow(id string) bool {
	m.mu.RLock()
	_, ok := m.upgrading[id]
	m.mu.RUnlock()
	return ok
}

// recentlyUpgraded 判断插件是不是刚升级过（5 秒内）。

// recentlyUpgraded 判断插件是不是刚升级过（5 秒内）。
func (m *Manager) recentlyUpgraded(id string) bool {
	m.mu.RLock()
	t := m.upgradedAt[id]
	m.mu.RUnlock()
	return !t.IsZero() && time.Since(t) < 5*time.Second
}

// waitUpgrade 如果插件正在升级，就等到替换完成（最多 60 秒）。
// 超时返回错误：不能放行启动一个可能还在被替换的目录。

// waitUpgrade 如果插件正在升级，就等到替换完成（最多 60 秒）。
// 超时返回错误：不能放行启动一个可能还在被替换的目录。
func (m *Manager) waitUpgrade(ctx context.Context, id string) error {
	m.mu.RLock()
	ch := m.upgrading[id]
	m.mu.RUnlock()
	if ch == nil {
		return nil
	}
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-timer.C:
		return fmt.Errorf("插件 %s 正在升级，等待超时", id)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ensureRunning 确保插件在跑：没在运行就拉起（崩溃恢复、禁用后重新启用）。
// acquireHost 的第一步；进程活着时是一次廉价查询。
