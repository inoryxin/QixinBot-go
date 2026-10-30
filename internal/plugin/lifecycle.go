package plugin

// 插件生命周期：加载/启停/监督重启/卸载/停机。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xin/onebot/internal/registry"
)

// StartBackground 启动后台事件循环。
func (m *Manager) StartBackground(ctx context.Context) {
	go m.eventLoop(ctx)
}

// Load 扫描插件目录并启动全部插件。单个插件失败不影响其它插件。

// Load 扫描插件目录并启动全部插件。单个插件失败不影响其它插件。
func (m *Manager) Load(ctx context.Context) {
	ids, err := m.scanIDs()
	if err != nil {
		m.Log.Error("扫描插件目录失败，跳过加载", "err", err)
		return
	}

	// 分两轮加载：先起"被当作系统组件"的插件（服务插件 + 被别人 requires 的），
	// 再起普通插件。这样 ai-chat 依赖 sqlite 时，不会因为字母序排在前面而误报缺依赖。
	needed := m.neededPlugins(ids)

	first := make([]string, 0, len(ids))
	second := make([]string, 0, len(ids))
	// 记下每个插件的 load_order，用于组内排序
	order := make(map[string]int, len(ids))

	for _, id := range ids {
		man, err := LoadManifest(filepath.Join(m.Dir, id))
		if err != nil {
			second = append(second, id)
			continue
		}
		// 设置项类型写错会退化成文本框，装的时候就提醒
		for _, sc := range man.Settings {
			if sc.Type != "" && !knownSettingTypes[sc.Type] {
				m.Log.Warn("设置项类型未知，会显示成文本框",
					"plugin", id, "key", sc.Key, "type", sc.Type)
			}
		}
		order[id] = man.LoadOrder
		if man.Kind == "service" || needed[id] {
			first = append(first, id)
		} else {
			second = append(second, id)
		}
	}

	// 组内按 load_order 升序，同 order 按 id。
	// 这样"服务 B 的 initialize 要调服务 A"时，把 A 的 load_order 设小就能保证顺序。
	byOrder := func(list []string) {
		sort.SliceStable(list, func(i, j int) bool {
			oi, oj := order[list[i]], order[list[j]]
			if oi != oj {
				return oi < oj
			}
			return list[i] < list[j]
		})
	}
	byOrder(first)
	byOrder(second)

	if len(first) > 0 {
		m.Log.Info("第一轮加载系统组件", "plugins", first)
	}
	for _, id := range append(first, second...) {
		if err := m.startPlugin(ctx, id); err != nil {
			m.Log.Error("插件启动失败", "plugin", id, "err", err)
		}
	}

	// 打一条顺序日志，排查"为什么 B 比 A 先起来"时能直接看到
	if len(ids) > 1 {
		m.Log.Debug("加载顺序", "order", append(first, second...))
	}
}

// neededPlugins 找出被其他插件用 [[requires]] kind="plugin" 点名的插件。

// neededPlugins 找出被其他插件用 [[requires]] kind="plugin" 点名的插件。
func (m *Manager) neededPlugins(ids []string) map[string]bool {
	out := make(map[string]bool)
	for _, id := range ids {
		man, err := LoadManifest(filepath.Join(m.Dir, id))
		if err != nil {
			continue
		}
		for _, r := range man.Requires {
			if r.Kind == "plugin" && r.Value != "" {
				out[r.Value] = true
			}
		}
	}
	return out
}

// hostAlive 判断 host 的进程是否还活着（自发崩溃时 closing 不会置位，
// 不能只看 closing）。

// SetEnv 线程安全地设置插件进程环境变量（解释器路径等）。
func (m *Manager) SetEnv(k, v string) {
	m.envMu.Lock()
	if m.Env == nil {
		m.Env = make(map[string]string)
	}
	m.Env[k] = v
	m.envMu.Unlock()
}

// DeleteEnv 线程安全地删除环境变量。

// DeleteEnv 线程安全地删除环境变量。
func (m *Manager) DeleteEnv(k string) {
	m.envMu.Lock()
	delete(m.Env, k)
	m.envMu.Unlock()
}

// startPlugin 加载单个插件。已存在同名 host 时先停掉。

// startPlugin 加载单个插件。已存在同名 host 时先停掉。
func (m *Manager) startPlugin(ctx context.Context, id string) error {
	if m.closingManager.Load() {
		return fmt.Errorf("管理器正在关闭，拒绝启动插件 %s", id)
	}
	if m.isDisabled(id) {
		return fmt.Errorf("插件 %s 已禁用", id)
	}
	dir := filepath.Join(m.Dir, id)
	man, err := LoadManifest(dir)
	if err != nil {
		m.setState(id, "failed", err.Error())
		return err
	}

	// 清单要先登记，这样即使插件被禁用，WebUI 也能看到它
	m.mu.Lock()
	m.metas[man.ID] = man
	m.mu.Unlock()

	if m.isDisabled(id) {
		m.setState(id, "disabled", "")
		return nil
	}

	// 校验 manifest 里声明的素材子目录（标准目录不用声明）
	if len(man.Assets.Dirs) > 0 {
		standard := make(map[string]bool, len(StandardAssetDirs))
		for _, d := range StandardAssetDirs {
			standard[d] = true
		}
		for _, sub := range man.Assets.Dirs {
			if standard[sub] {
				m.Log.Warn("标准素材目录不用在 [assets] 里声明", "plugin", id, "dir", sub)
				continue
			}
			// 注意别用 dir 当循环变量：外层 dir 是插件目录
			abs := filepath.Join(dir, "assets", sub)
			if info, err := os.Stat(abs); err != nil || !info.IsDir() {
				m.Log.Warn("声明的素材目录不存在", "plugin", id, "dir", "assets/"+sub)
			}
		}
	}

	// 平台不符就别启动了：带 .node/.so 的插件换系统必然崩
	if man.Runtime != nil && !platformOK(man.Runtime.Platform) {
		m.setState(id, "wrong_platform", fmt.Sprintf("这个插件只支持 %s，当前是 %s", man.Runtime.Platform, runtime.GOOS))
		return nil
	}

	if !man.EnabledOr(true) {
		m.setState(man.ID, "disabled", "")
		m.Log.Info("插件清单里 enabled=false，跳过", "plugin", man.ID)
		return nil
	}

	// 同一个插件不能有两份进程
	m.stopPlugin(man.ID)

	// 框架把适配器复制进运行区，插件不依赖外部 sdk 目录
	// 只在缺失时复制：每次重载都写一遍会改 mtime，触发热重载死循环
	sdkTarget := filepath.Join(dir, "_sdk")
	if m.SDKDir != "" && !dirExists(sdkTarget) {
		_ = copyDir(m.SDKDir, sdkTarget)
	}

	// 新布局：数据就在插件自己的运行目录下，升级时整体保留
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		m.setState(man.ID, "failed", err.Error())
		return err
	}

	// 外部依赖（ffmpeg 之类）先查，缺了给明确提示
	// "装没装"要查磁盘，不能只看 m.metas：
	// 启动时按字母序加载，ai-chat 排在 sqlite 前面，这时 metas 里还没有 sqlite。
	installed := func(id string) bool {
		m.mu.RLock()
		_, loaded := m.metas[id]
		m.mu.RUnlock()
		if loaded {
			return true
		}
		if m.PackagesDir != "" {
			if _, err := os.Stat(filepath.Join(m.PackagesDir, id+".anzu")); err == nil {
				return true
			}
		}
		if _, err := os.Stat(filepath.Join(m.Dir, id)); err == nil {
			return true
		}
		return false
	}
	if err := m.ensureDownloadedDeps(ctx, man, dir); err != nil {
		m.setState(man.ID, "missing_dependency", err.Error())
		return err
	}
	if err := CheckRequires(man, dir, installed, m.SharedDepsDir, map[string]string{"node": RuntimeOverride("node"), "python": RuntimeOverride("python")}); err != nil {
		m.setState(man.ID, "missing_dependency", err.Error())
		return err
	}

	// 先确认运行环境，缺了就直接标 missing_runtime，别等到 fork 才发现
	if rt := DetectRuntime(man, dir); !rt.OK {
		m.setState(man.ID, "missing_runtime", rt.Message)
		return fmt.Errorf("%s", rt.Message)
	}

	h := NewHost(man, dir, dataDir, m.Log)
	h.SharedDepsDir = m.SharedDepsDir
	// 共享池：只有显式分到池、状态为 shared 的插件才进共享组；
	// 未分配/已逐出/池熔断时一律独立进程（quarantine）。
	if m.cohostEnabled() && man.Cohost && man.Kind != "service" && m.Pools != nil {
		if pool, ass, ok := m.Pools.PoolOf(id); ok && ass.State == CohostStateShared {
			if sh, serr := m.ensureSharedHost(ctx, pool.ID, pool.MaxMembers); serr != nil {
				m.Log.Warn("共享池不可用，改用独立进程", "plugin", man.ID, "pool", pool.ID, "err", serr)
			} else {
				h.cohost = sh
			}
		}
	}
	m.envMu.RLock()
	for k, v := range m.Env {
		h.ExtraEnv = append(h.ExtraEnv, k+"="+v)
	}
	m.envMu.RUnlock()
	h.Settings = EffectiveSettings(man, LoadSettings(m.Dir, man.ID))
	h.Handler = func(hctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
		if m.OnHostCall == nil {
			return nil, Errf(CodeMethodNotFound, "宿主未实现 %s", method)
		}
		return m.OnHostCall(hctx, man, method, params)
	}

	if err := h.Start(ctx); err != nil {
		// 组满是设计内的降级，不是失败：回退独占进程，插件不能因此起不来
		if errors.Is(err, ErrCohostFull) && h.cohost != nil {
			m.Log.Info("共享组已满，改用独占进程", "plugin", man.ID)
			h.cohost = nil
			err = h.Start(ctx)
		}
		if err != nil {
			m.setState(man.ID, "failed", err.Error())
			return fmt.Errorf("启动失败: %w", err)
		}
	}

	// 先清掉这个插件可能残留的 Feature 注册。
	// 崩溃自动重启时走不到 stopPlugin，旧条目还在注册表里，
	// 直接 Add 会报"feature 冲突"，插件就永远起不来了。
	m.Reg.RemoveByPlugin(man.ID)

	var registered []*registry.Feature
	for _, f := range man.Features() {
		if err := m.Reg.Add(f); err != nil {
			m.Log.Error("注册 Feature 失败", "plugin", man.ID, "feature", f.ID, "err", err)
			for _, r := range registered {
				_ = r
			}
			m.Reg.RemoveByPlugin(man.ID)
			h.Stop(ctx)
			m.setState(man.ID, "failed", fmt.Sprintf("feature 冲突: %s", f.ID))
			return err
		}
		registered = append(registered, f)
	}

	m.mu.Lock()
	m.hosts[man.ID] = h
	// 停过的插件 order 里还留着（分发时靠它找到插件）。
	// 重启时不能再 append，否则同一插件会在分发顺序里出现两次。
	m.ensureOrderLocked(man.ID)
	m.states[man.ID] = "ready"
	delete(m.errs, man.ID)
	m.mu.Unlock()

	if m.Shared != nil {
		for _, sp := range man.Shared {
			if sp.Key == "" {
				continue
			}
			m.Shared.Register(man.ID, sp.Key, sp.Description, sp.Default)
		}
	}

	for _, c := range man.Provides {
		name := capName(c)
		if name == "" {
			continue
		}
		m.mu.Lock()
		list := append(m.caps[name], capProvider{ID: man.ID, Priority: capPriority(man)})
		// priority 高的排前面
		sort.SliceStable(list, func(i, j int) bool { return list[i].Priority > list[j].Priority })
		m.caps[name] = list
		m.mu.Unlock()
		m.Log.Info("能力已注册", "capability", name, "plugin", man.ID, "priority", capPriority(man))
	}

	m.mu.Lock()
	_, wasUpgrading := m.upgrading[man.ID]
	m.mu.Unlock()
	m.finishUpgrade(man.ID)
	if wasUpgrading {
		m.markUpgraded(man.ID) // 只有升级成功启动后才记录
	}
	m.Log.Info("插件已加载", "plugin", man.ID, "features", len(registered))
	go m.supervise(h)
	return nil
}

// maxConsecutiveTimeouts 连续这么多次消息超时就自动禁用（挂死自愈）。
// 和崩溃计数同理：给用户一个明确的"它在坏"信号，而不是让整个 bot 变慢。

// supervise 看住一个插件进程：意外退出就退避重启，连续失败则禁用。
func (m *Manager) supervise(h *Host) {
	<-h.Done()

	// 稳定跑过 10 分钟：之前的故障不算"连续崩溃"
	if since, ok := h.StartedSince(); ok && since >= 10*time.Minute {
		m.resetRestarts(h.Manifest.ID)
	}

	// 主动停止（禁用/重载/升级）不重启；故障退出（如写超时）必须重启
	if (h.closing.Load() && !h.faulted.Load()) || m.isDisabled(h.Manifest.ID) {
		return
	}

	id := h.Manifest.ID
	m.Log.Warn("插件进程退出", "plugin", id, "detail", h.ExitSummary())

	m.mu.Lock()
	m.restarts[id]++
	if m.stats != nil {
		m.stats.recordCrash(id, nil)
	}
	n := m.restarts[id]
	m.mu.Unlock()

	m.deathsMu.Lock()
	evicted := m.cohostEvicted[id]
	m.deathsMu.Unlock()
	limit := 5 // 独占插件：连续 6 次才禁用
	if evicted {
		limit = 2 // 已逐出：第 3 次直接禁用
	}
	if n > limit {
		m.Log.Error("插件连续崩溃，已禁用", "plugin", id, "times", n, "cohost_evicted", evicted)
		m.setState(id, "failed", fmt.Sprintf("连续崩溃 %d 次，已停止自动重启", n))
		_ = m.Disable(id)
		return
	}

	delay := time.Duration(1<<uint(n-1)) * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	m.Log.Warn("插件进程退出，准备重启", "plugin", id, "attempt", n, "in", delay)

	select {
	case <-time.After(delay):
	case <-m.shutdownCh:
		return
	}

	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	// 升级替换期间不要启动：文件可能还没就位
	if err := m.waitUpgrade(context.Background(), id); err != nil {
		m.Log.Warn("升级未完成，暂不自动重启", "plugin", id, "err", err)
		return
	}

	// 等待期间可能已经被手动重载/换成了新进程，别再起一份
	m.mu.RLock()
	cur := m.hosts[id]
	m.mu.RUnlock()
	if cur != h {
		return
	}
	if m.closingManager.Load() || m.isDisabled(id) {
		return
	}
	if err := m.startPlugin(context.Background(), id); err != nil {
		m.Log.Error("自动重启失败", "plugin", id, "err", err)
		// startPlugin 在早期路径（清单坏了/缺依赖/缺运行时）失败时
		// 不会清理旧 Host：主动摘掉，否则 ensureRunning 永远拿到
		// "存在但已死"的句柄，插件就再也起不来了
		m.mu.Lock()
		if m.hosts[id] == h {
			delete(m.hosts, id)
		}
		m.mu.Unlock()
	}
}

// resetRestarts 在手动操作时清零崩溃计数。
// ensureOrderLocked 把插件放进分发顺序（去重）。调用方必须持有 m.mu。

// resetRestarts 在手动操作时清零崩溃计数。
// ensureOrderLocked 把插件放进分发顺序（去重）。调用方必须持有 m.mu。
func (m *Manager) ensureOrderLocked(id string) {
	for _, x := range m.order {
		if x == id {
			return
		}
	}
	m.order = append(m.order, id)
}

func (m *Manager) resetRestarts(id string) {
	m.mu.Lock()
	delete(m.restarts, id)
	m.mu.Unlock()
}

// stopPlugin 停止并移除一个插件（不关心它是否被禁用）。

// stopPlugin 停止并移除一个插件（不关心它是否被禁用）。
func (m *Manager) stopPlugin(id string) {
	// 插件停了，它挂着的等待也一起清掉（否则用户下一条消息会被"鬼"消费）
	if m.asks != nil {
		if n := m.asks.CancelPlugin(id); n > 0 {
			m.Log.Debug("清理交互等待", "plugin", id, "count", n)
		}
	}
	if m.jobs != nil {
		if n := m.jobs.CancelPlugin(id); n > 0 {
			m.Log.Debug("清理后台任务记录", "plugin", id, "count", n)
		}
	}

	m.mu.Lock()
	h := m.hosts[id]
	delete(m.hosts, id)
	delete(m.timeoutStreak, id)
	m.order = removeString(m.order, id)
	m.mu.Unlock()

	if h != nil {
		m.Log.Info("正在停止插件", "plugin", id)
		h.Stop(context.Background())
		// 并发上限的信号量跟着插件一起删：留着会累积死条目，
		// 改了 max_concurrent 后旧等待者也会挂在废弃的 channel 上
		if m.capLim != nil {
			m.capLim.Forget(id)
		}
	}
	m.Reg.RemoveByPlugin(id)

	m.mu.Lock()
	for name, list := range m.caps {
		kept := list[:0]
		for _, p := range list {
			if p.ID != id {
				kept = append(kept, p)
			}
		}
		if len(kept) == 0 {
			delete(m.caps, name)
		} else {
			m.caps[name] = kept
		}
	}
	m.mu.Unlock()
}

// Reload 重载单个插件：停旧进程 → 重读清单 → 起新进程。

// Reload 重载单个插件：停旧进程 → 重读清单 → 起新进程。
func (m *Manager) Reload(ctx context.Context, id string) error {
	if err := m.waitUpgrade(ctx, id); err != nil {
		return err
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	// 拿锁后再确认没有升级：否则检查通过后升级可能刚开始
	if err := m.waitUpgrade(ctx, id); err != nil {
		return err
	}

	if m.isDisabled(id) {
		return fmt.Errorf("插件 %s 已禁用", id)
	}
	m.resetRestarts(id)
	if m.stats != nil {
		m.stats.recordRestart(id)
	}
	m.stopPlugin(id)
	if err := m.startPlugin(ctx, id); err != nil {
		// 新版本起不来：旧进程已停，标记 failed，别让界面看起来还在正常跑
		m.setState(id, "failed", "重载失败，请检查插件: "+err.Error())
		return err
	}
	m.Log.Info("插件已重载", "plugin", id)
	return nil
}

// Enable 启用插件并立即启动。

// Enable 启用插件并立即启动。
func (m *Manager) Enable(ctx context.Context, id string) error {
	if err := m.waitUpgrade(ctx, id); err != nil {
		return err
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if err := m.waitUpgrade(ctx, id); err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.disabled, id)
	m.mu.Unlock()
	m.saveState()
	m.resetRestarts(id)

	m.stopPlugin(id)
	if err := m.startPlugin(ctx, id); err != nil {
		return err
	}
	m.Log.Info("插件已启用", "plugin", id)

	// 重新检查依赖：别的插件可能正等着这个插件提供的能力
	// （比如先启用 weather，selftest 才会从 degraded 恢复）
	m.CheckUses()
	return nil
}

// Disable 禁用插件：停进程、移除全部 Feature、记住状态。

// Disable 禁用插件：停进程、移除全部 Feature、记住状态。
func (m *Manager) Disable(id string) error {
	if err := m.waitUpgrade(context.Background(), id); err != nil {
		return err
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if err := m.waitUpgrade(context.Background(), id); err != nil {
		return err
	}

	m.mu.Lock()
	m.disabled[id] = true
	m.mu.Unlock()
	m.saveState()

	m.stopPlugin(id)
	m.setState(id, "disabled", "")
	m.Log.Info("插件已禁用", "plugin", id)
	return nil
}

// Watch 轮询插件目录，文件变化就重载、目录消失就卸载、新目录就加载。
// 用轮询而不是 fsnotify：插件目录通常很小，2 秒一次的开销可以忽略，
// 而且能顺带发现"编辑器先删后建"这类事件。

// CheckNames 检查插件中文名是否重复。
// 不阻止加载（可能是有意的），但日志里会警告，WebUI 上会加 id 区分。
func (m *Manager) CheckNames() {
	m.mu.RLock()
	counts := make(map[string][]string)
	for id, man := range m.metas {
		if man.Name != "" {
			counts[man.Name] = append(counts[man.Name], id)
		}
	}
	m.mu.RUnlock()

	for name, ids := range counts {
		if len(ids) > 1 {
			m.Log.Warn("插件中文名重复，显示时会自动加 id 区分", "name", name, "plugins", ids)
		}
	}
}

// CheckUses 在所有插件加载完后统一检查依赖。
// 缺依赖的插件标成 degraded：仍然运行（可能部分功能可用），但 UI 会显示缺什么。

// CheckUses 在所有插件加载完后统一检查依赖。
// 缺依赖的插件标成 degraded：仍然运行（可能部分功能可用），但 UI 会显示缺什么。
func (m *Manager) CheckUses() {
	m.mu.RLock()
	type item struct {
		id  string
		man *Manifest
	}
	items := make([]item, 0, len(m.metas))
	for id, man := range m.metas {
		items = append(items, item{id, man})
	}
	caps := make(map[string][]capProvider, len(m.caps))
	maps.Copy(caps, m.caps)
	// 框架内置能力也算数（db.sqlite 就不需要插件提供）
	builtins := make(map[string]bool, len(m.Builtins))
	for name := range m.Builtins {
		builtins[name] = true
	}
	m.mu.RUnlock()

	for _, it := range items {
		var missing []string
		for _, u := range it.man.Uses {
			name := capName(u)
			if _, ok := caps[name]; ok {
				continue
			}
			if builtins[name] {
				continue
			}
			missing = append(missing, u)
		}
		if len(missing) == 0 {
			continue
		}
		m.Log.Warn("插件缺依赖能力", "plugin", it.id, "missing", missing)
		m.setState(it.id, "degraded", "缺少能力: "+strings.Join(missing, ", "))
	}
}

// lockFor 决定这次消息处理用哪种锁。
// 默认整插件串行；声明 concurrency = "session" 的插件按会话分片。

// StopAll 关闭全部插件。
func (m *Manager) StopAll(ctx context.Context) {
	// 先标记关闭：禁止 supervise 再拉起新进程
	m.closingManager.Store(true)
	m.shutdownOnce.Do(func() { close(m.shutdownCh) })

	// 等在途的启停/重载走完，再快照：否则刚启动的 Host 可能不在快照里，
	// 主程序退出后留下孤儿子进程。
	m.reloadMu.Lock()
	//lint:ignore SA2001 空临界区就是等待屏障
	m.reloadMu.Unlock()

	m.mu.RLock()
	hosts := make([]*Host, 0, len(m.hosts))
	for _, h := range m.hosts {
		hosts = append(hosts, h)
	}
	m.mu.RUnlock()

	// 共享宿主一起关（它不是 Host，上面的循环覆盖不到）
	m.sharedMu.Lock()
	pools := make([]*SharedHost, 0, len(m.sharedPools))
	for id, sh := range m.sharedPools {
		if sh != nil {
			pools = append(pools, sh)
		}
		delete(m.sharedPools, id)
	}
	m.sharedMu.Unlock()
	for _, sh := range pools {
		sh.Stop()
	}

	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h *Host) {
			defer wg.Done()
			h.Stop(ctx)
		}(h)
	}
	wg.Wait()
}

// ToolInfo 是暴露给 WebUI / AI 的工具描述。

func (m *Manager) scanIDs() ([]string, error) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		m.Log.Warn("插件目录不可读", "dir", m.Dir, "err", err)
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// .staging-* 是上传过程中的临时目录，不算插件
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		ids = append(ids, e.Name())
	}
	sort.Strings(ids)
	return ids, nil
}

// scan 返回 插件名 → 目录签名。签名基于所有文件的路径和 mtime。

// scan 返回 插件名 → 目录签名。签名基于所有文件的路径和 mtime。
func (m *Manager) scan() map[string]string {
	out := make(map[string]string)
	ids, err := m.scanIDs()
	if err != nil {
		m.Log.Warn("扫描失败，本轮不产生快照", "err", err)
		return nil
	}
	for _, id := range ids {
		out[id] = dirSignature(filepath.Join(m.Dir, id))
	}
	return out
}

// dirSignature 只关心代码和清单文件；node_modules 可能很大，跳过。

// dirSignature 只关心代码和清单文件；node_modules 可能很大，跳过。
func dirSignature(dir string) string {
	h := fnv.New64a()
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			// **data/ 是插件私有的运行数据**，不是代码。
			// 不跳过的话，插件每次写状态（缓存、进度）都会让
			// dirSignature 变，触发一次无谓的重载——
			// 15 秒轮询的插件就会每 15 秒重载一次。
			if name == "node_modules" || name == ".git" || name == "web" ||
				name == "dist" || name == "_sdk" || name == "data" {
				return filepath.SkipDir
			}
			return nil
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case ".js", ".mjs", ".cjs", ".ts", ".py", ".json", ".toml", ".yaml", ".yml":
		default:
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		h.Write([]byte(path))
		h.Write([]byte(strconv.FormatInt(info.ModTime().UnixNano(), 10)))
		return nil
	})
	return strconv.FormatUint(h.Sum64(), 16)
}

// Remove 删除插件：停进程、删目录、清状态。数据目录保留，避免误删数据。

// Remove 删除插件：停进程、删目录、清状态。数据目录保留，避免误删数据。
func (m *Manager) Remove(id string) error {
	if err := m.waitUpgrade(context.Background(), id); err != nil {
		return err
	}
	// 和安装器同步串行：否则同步可能已读入包并在删除后把插件装回来。
	// 顺序固定为 waitUpgrade → syncMu → reloadMu，和升级流程一致，避免互等。
	if m.Inst != nil {
		m.Inst.LockSync()
		defer m.Inst.UnlockSync()
	}
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	if err := m.waitUpgrade(context.Background(), id); err != nil {
		return err
	}
	// 停进程必须在锁内：否则并发的拉起可能在停止之后又把它起回来
	m.stopPlugin(id)
	if m.Shared != nil {
		m.Shared.Unregister(id)
	}

	dir := filepath.Join(m.Dir, id)
	root := filepath.Clean(m.Dir) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(dir)+string(os.PathSeparator), root) {
		return fmt.Errorf("非法插件 id: %s", id)
	}
	// 删除代码前先把 data/ 挪出运行区，否则整个目录会被 RemoveAll 一起删掉。
	// 备份路径：data/removed-plugins/<id>-<时间>/data
	dataSrc := filepath.Join(dir, "data")
	var dataBackup string
	if _, err := os.Stat(dataSrc); err == nil {
		dataBackup = filepath.Join(filepath.Dir(m.Dir), "removed-plugins",
			id+"-"+time.Now().Format("20060102-150405"))
		if err := os.MkdirAll(dataBackup, 0o755); err != nil {
			return fmt.Errorf("备份插件数据失败: %w", err)
		}
		if err := os.Rename(dataSrc, filepath.Join(dataBackup, "data")); err != nil {
			return fmt.Errorf("备份插件数据失败: %w", err)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	// 包文件一起删，否则下次 Sync 又会把它装回来。
	// 包文件名记录在 install.json 里，不一定等于 "<id>.anzu"
	//（包可以改名投放）：按记录删，删不掉再兜底按 id 删
	if m.PackagesDir != "" {
		pkg := id + ".anzu"
		if m.Inst != nil {
			if info, err := readInstallInfo(m.Dir, id); err == nil && info.Package != "" {
				pkg = info.Package
			}
		}
		_ = os.Remove(filepath.Join(m.PackagesDir, pkg))
	}

	if m.Pools != nil {
		if err := m.Pools.removePlugin(id); err != nil {
			m.Log.Warn("清理插件池归属失败", "plugin", id, "err", err)
		}
	}
	m.deathsMu.Lock()
	delete(m.cohostEvicted, id)
	delete(m.cohostCrashes, id)
	m.deathsMu.Unlock()

	m.mu.Lock()
	delete(m.metas, id)
	delete(m.states, id)
	delete(m.disabled, id)
	delete(m.errs, id)
	m.mu.Unlock()
	m.saveState()

	if dataBackup != "" {
		m.Log.Info("插件已删除，数据已备份", "plugin", id, "data", dataBackup)
	} else {
		m.Log.Info("插件已删除（没有 data/ 需要备份）", "plugin", id)
	}
	return nil
}

// StateOf 返回插件的运行状态（ready/disabled/failed/...）。

// IsService 判断插件是不是服务插件（只提供能力，不接消息）。
func (m *Manager) IsService(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if man, ok := m.metas[id]; ok {
		return man.Kind == "service"
	}
	return false
}

// CapabilitiesOf 返回某个插件声明的能力名列表。
