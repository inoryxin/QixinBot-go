package plugin

import (
	"fmt"
	"sync"
	"time"

	"github.com/xin/onebot/internal/session"
)

// cooldown 是宿主级的命令冷却。
//
// 插件自己写冷却有两个问题：每个插件写法不同、用户能靠换插件绕过。
// 放在宿主统一做，配置一处、行为一致。
//
// 三个维度可以叠加：
//   - 同一用户对同一插件：per_user（防刷屏）
//   - 同一群对同一插件：per_group（防一个群刷爆）
//   - 同一插件全局：per_plugin（防某个插件太费资源）
//
// 冷却期内不报错，只是**静默跳过**（不回复），避免刷屏时每条都回一句"慢点"。
type cooldown struct {
	mu   sync.Mutex
	last map[string]time.Time // key = 维度:插件:主体
}

func newCooldown() *cooldown {
	return &cooldown{last: make(map[string]time.Time)}
}

// Allow 检查并记账。返回 (是否放行, 还要等多久)。
func (c *cooldown) Allow(pluginID string, s *session.Session, cfg CooldownConfig) (bool, time.Duration) {
	if cfg.PerUser <= 0 && cfg.PerGroup <= 0 && cfg.PerPlugin <= 0 {
		return true, 0
	}
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 依次检查三个维度，任何一个没到时间就拒绝
	type check struct {
		key  string
		wait time.Duration
	}
	var checks []check
	if cfg.PerUser > 0 && s != nil {
		checks = append(checks, check{
			key:  fmt.Sprintf("u:%s:%d", pluginID, s.UserID),
			wait: cfg.PerUser,
		})
	}
	if cfg.PerGroup > 0 && s != nil && s.IsGroup() {
		checks = append(checks, check{
			key:  fmt.Sprintf("g:%s:%d", pluginID, s.ChatID),
			wait: cfg.PerGroup,
		})
	}
	if cfg.PerPlugin > 0 {
		checks = append(checks, check{
			key:  "p:" + pluginID,
			wait: cfg.PerPlugin,
		})
	}

	// 先全部检查一遍，任何一个还在冷却就整体拒绝（不记账）
	for _, ck := range checks {
		if last, ok := c.last[ck.key]; ok {
			if remain := ck.wait - now.Sub(last); remain > 0 {
				return false, remain
			}
		}
	}
	// 全部放行，统一记账
	for _, ck := range checks {
		c.last[ck.key] = now
	}
	// 顺手清一下过期条目，避免 map 无限长
	if len(c.last) > 4096 {
		for k, t := range c.last {
			if now.Sub(t) > time.Hour {
				delete(c.last, k)
			}
		}
	}
	return true, 0
}

// AllowFor 是 Manager 层的入口，用插件的配置。
func (m *Manager) allowCooldown(h *Host, s *session.Session) (bool, time.Duration) {
	if m.cool == nil || h == nil {
		return true, 0
	}
	return m.cool.Allow(h.Manifest.ID, s, h.Manifest.Cooldown.toConfig())
}

// CooldownConfig 从 manifest 的 [cooldown] 读。
type CooldownConfig struct {
	// PerUser 同一用户对同一插件的冷却，0 = 不限制
	PerUser time.Duration
	// PerGroup 同一群对同一插件的冷却
	PerGroup time.Duration
	// PerPlugin 该插件全局冷却（所有用户所有群共享）
	PerPlugin time.Duration
}
