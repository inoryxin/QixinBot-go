package plugin

// Manager 侧的共享宿主 glue：按池隔离、开关、熔断、建组、worker 退出处理。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// SetCohostEnabled 打开/关闭共享宿主（默认开，cohost_enabled = false 显式关闭）。
func (m *Manager) SetCohostEnabled(on bool) { m.cohostOn.Store(on) }

// SetCohostGroupSize 设置未配置池上限时的默认值（0 = 沿用 6）。
func (m *Manager) SetCohostGroupSize(n int) {
	if n > 0 {
		m.cohostGroupSize = n
	}
}

// CohostGroupSizeDefault 是 cohost_group_size 未配置时的默认上限。
const CohostGroupSizeDefault = 6

func (m *Manager) cohostEnabled() bool { return m.cohostOn.Load() }

// CohostPoolRuntime 是池运行时的只读快照（给 WebUI/API 用）。
type CohostPoolRuntime struct {
	Running  bool    `json:"running"`
	PID      int     `json:"pid,omitempty"`
	RSSMB    float64 `json:"rss_mb,omitempty"`
	Members  int     `json:"members"`
	Degraded bool    `json:"degraded"`
}

// CohostPoolRuntime 返回池当前共享进程快照；没有进程时 Running=false。
func (m *Manager) CohostPoolRuntime(poolID string) CohostPoolRuntime {
	m.sharedMu.Lock()
	sh := m.sharedPools[poolID]
	m.sharedMu.Unlock()
	rt := CohostPoolRuntime{Degraded: m.CohostPoolDegraded(poolID)}
	if sh == nil {
		return rt
	}
	select {
	case <-sh.Done():
		return rt
	default:
	}
	rt.Running = true
	rt.PID = sh.PID()
	rt.RSSMB = sh.RSSMB()
	rt.Members = sh.MemberCount()
	return rt
}

// CohostPoolDegraded 报告某个池是否已熔断。
func (m *Manager) CohostPoolDegraded(poolID string) bool {
	m.deathsMu.Lock()
	defer m.deathsMu.Unlock()
	return m.degradedPools[poolID]
}

// recordSharedDeath 记录某个池共享进程死亡；同一池 10 分钟内 ≥3 次熔断该池。
func (m *Manager) recordSharedDeath(poolID string) {
	now := time.Now()
	m.deathsMu.Lock()
	defer m.deathsMu.Unlock()
	list := m.sharedDeaths[poolID]
	keep := list[:0]
	for _, t := range list {
		if now.Sub(t) <= 10*time.Minute {
			keep = append(keep, t)
		}
	}
	keep = append(keep, now)
	m.sharedDeaths[poolID] = keep
	if len(keep) >= 3 && !m.degradedPools[poolID] {
		m.degradedPools[poolID] = true
		m.Log.Error("共享池 10 分钟内死亡 3 次，熔断该池：成员改回独占进程", "pool", poolID)
	}
}

// ensureSharedHost 返回（必要时创建）某个池的共享宿主。
func (m *Manager) ensureSharedHost(ctx context.Context, poolID string, maxMembers int) (*SharedHost, error) {
	m.sharedMu.Lock()
	defer m.sharedMu.Unlock()

	if sh := m.sharedPools[poolID]; sh != nil {
		select {
		case <-sh.Done():
			m.Log.Warn("共享池进程退出，重建", "pool", poolID)
			m.recordSharedDeath(poolID)
			delete(m.sharedPools, poolID)
		default:
			return sh, nil
		}
	}
	if m.CohostPoolDegraded(poolID) {
		return nil, fmt.Errorf("共享池 %s 已熔断，插件改用独占进程", poolID)
	}
	sdkDir := m.SDKDir
	if sdkDir == "" {
		return nil, fmt.Errorf("共享宿主需要 [plugin] sdk_dir 指向 sdk/node")
	}
	mh := filepath.Join(sdkDir, "multi-host.mjs")
	if _, err := os.Stat(mh); err != nil {
		return nil, fmt.Errorf("找不到 multi-host.mjs: %w", err)
	}
	dir := filepath.Join(m.DataDir, "cohost", poolID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var members []CohostMember
	m.mu.RLock()
	for id, h := range m.hosts {
		if h.cohost == nil || m.Pools == nil {
			continue
		}
		pool, ass, ok := m.Pools.PoolOf(id)
		if !ok || ass.PoolID != poolID || ass.State != CohostStateShared || pool.ID != poolID {
			continue
		}
		entry := "app/index.js"
		if len(h.Manifest.Exec) > 1 {
			entry = h.Manifest.Exec[1]
		}
		settings := json.RawMessage("{}")
		if h.Settings != nil {
			if b, err := json.Marshal(h.Settings); err == nil {
				settings = b
			}
		}
		dataDir := filepath.Join(m.DataDir, "cohost-data", id)
		members = append(members, CohostMember{ID: id, Dir: h.Dir, Entry: entry, DataDir: dataDir, Settings: settings})
	}
	m.mu.RUnlock()
	cfgPath, err := NewSharedHostConfig(dir, sdkDir, members)
	if err != nil {
		return nil, err
	}
	opts := SharedHostOptions{NodeExe: RuntimeOverride("node")}
	m.envMu.RLock()
	for k, v := range m.Env {
		opts.ExtraEnv = append(opts.ExtraEnv, k+"="+v)
	}
	m.envMu.RUnlock()
	if sd := m.SharedDepsDir; sd != "" {
		deps := filepath.Join(sd, "node", "node_modules")
		if fi, err := os.Stat(deps); err == nil && fi.IsDir() {
			opts.ExtraEnv = append(opts.ExtraEnv, "NODE_PATH="+deps)
		}
	}
	sh, err := StartSharedHost(mh, cfgPath, opts, func(ctx context.Context, pluginID, method string, params json.RawMessage) (any, *RPCError) {
		m.mu.RLock()
		man := m.metas[pluginID]
		m.mu.RUnlock()
		if man == nil {
			return nil, Errf(CodeMethodNotFound, "未知插件 %s", pluginID)
		}
		if m.OnHostCall == nil {
			return nil, Errf(CodeMethodNotFound, "宿主未实现 %s", method)
		}
		return m.OnHostCall(ctx, man, method, params)
	}, m.Log)
	if err != nil {
		return nil, err
	}
	if maxMembers <= 0 {
		maxMembers = m.cohostGroupSize
	}
	if maxMembers <= 0 {
		maxMembers = CohostGroupSizeDefault
	}
	sh.maxMembers = maxMembers
	sh.onWorkerExit = func(pluginID string, code int) {
		m.onCohostWorkerExit(poolID, pluginID, code)
	}
	m.sharedPools[poolID] = sh
	m.Log.Info("共享池已启动", "pool", poolID, "members", len(members), "max", maxMembers)
	return sh, nil
}

// onCohostWorkerExit 共享池 worker 级崩溃：24h 内 3 次 → 单插件逐出到独占。
func (m *Manager) onCohostWorkerExit(poolID, pluginID string, code int) {
	if m.asks != nil {
		if n := m.asks.CancelPlugin(pluginID); n > 0 {
			m.Log.Warn("共享池插件退出，已取消交互等待", "plugin", pluginID, "count", n)
		}
	}
	now := time.Now()
	n := 0
	if m.Pools != nil {
		if cnt, err := m.Pools.RecordCrash(pluginID); err == nil {
			n = cnt
		}
	}
	// 没有池归属（理论上不会发生）时退回内存计数，保证策略不失效。
	if n == 0 {
		m.deathsMu.Lock()
		list := m.cohostCrashes[pluginID]
		keep := list[:0]
		for _, t := range list {
			if now.Sub(t) <= 24*time.Hour {
				keep = append(keep, t)
			}
		}
		keep = append(keep, now)
		m.cohostCrashes[pluginID] = keep
		n = len(keep)
		m.deathsMu.Unlock()
	}
	if m.stats != nil {
		m.stats.recordCrash(pluginID, nil)
	}
	if n >= 3 {
		m.evictCohost(pluginID, poolID)
	}
}

// evictCohost 把插件从共享池踢回独立进程；不自动回池。
func (m *Manager) evictCohost(pluginID, poolID string) {
	m.deathsMu.Lock()
	if m.cohostEvicted[pluginID] {
		m.deathsMu.Unlock()
		return
	}
	m.cohostEvicted[pluginID] = true
	m.deathsMu.Unlock()
	if m.Pools != nil {
		_ = m.Pools.Evict(pluginID)
	}
	m.Log.Error("共享池插件 24h 崩溃 3 次，已踢出共享池，改独立进程", "plugin", pluginID, "pool", poolID)
	// 先显式停掉旧 worker（Reload 里还会兜底一次），避免旧 worker 在逐出后
	// 继续被 multi-host 拉起、与新的独立进程同时存在。
	if h := m.HostOf(pluginID); h != nil && h.cohost != nil {
		_ = h.cohost.StopPlugin(pluginID)
	}
	// 不能在 multi-host 的读循环里直接 Stop/Start（本函数由 onCohostWorkerExit
	// 异步派发，但仍可能与读循环并发）：让出读循环后再 Reload。
	// 300ms 是"等旧 worker 停止指令被处理"的经验值，不是精确同步——
	// 这一步不是唯一防线：Reload 内部还会再 StopPlugin 兜底一次；
	// 就算时序错过，cohostEvicted 标记保证不会重复逐出，Reload 重试即收敛。
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := m.Reload(context.Background(), pluginID); err != nil {
			m.Log.Error("逐出后重启为独立进程失败", "plugin", pluginID, "err", err)
		}
	}()
}

// ResetCohostEviction 人工恢复时清零崩溃计数/逐出标记。
func (m *Manager) ResetCohostEviction(pluginID string) {
	m.deathsMu.Lock()
	delete(m.cohostEvicted, pluginID)
	delete(m.cohostCrashes, pluginID)
	m.deathsMu.Unlock()
	if m.Pools != nil {
		_ = m.Pools.ResetCrashes(pluginID)
	}
	m.resetRestarts(pluginID)
}
