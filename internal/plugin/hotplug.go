package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// WatchPackages 监测投放区（plugins/*.anzu），有变化就回调。
// 这样丢一个包进去 / 删掉一个包，不用重启也能生效。
func (i *Installer) WatchPackages(ctx context.Context, interval time.Duration, onChange func()) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()

	last := i.PackagesSignature()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sig := i.PackagesSignature()
			if sig == last {
				continue
			}
			last = sig
			onChange()
		}
	}
}

// PackagesSignature 是投放区的指纹：文件名 + 大小 + 修改时间。
func (i *Installer) PackagesSignature() string {
	entries, err := os.ReadDir(i.PackagesDir)
	if err != nil {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".anzu") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		parts = append(parts, e.Name()+":"+strconv.FormatInt(info.Size(), 10)+":"+strconv.FormatInt(info.ModTime().UnixNano(), 10))
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// StopForRemoval 停掉一个即将被移除的插件，释放运行目录占用。
// StopForUpgrade 在"覆盖升级"前停掉插件进程（不标记禁用）。
// 和 StopForRemoval 的区别：升级完还要重新加载，所以不能写 disabled。
func (m *Manager) StopForUpgrade(id string) {
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()
	m.beginUpgrade(id) // 替换完成前，拉起入口要等
	m.stopPlugin(id)
}

func (m *Manager) StopForRemoval(id string) {
	// 删除也要和启停/重载串行：否则"正在拉起"和"删目录"会交错
	m.reloadMu.Lock()
	defer m.reloadMu.Unlock()

	m.mu.Lock()
	m.disabled[id] = true
	m.mu.Unlock()
	m.saveState()
	m.stopPlugin(id)
}

// MissingRuntimeIDs 返回"运行区有目录但没有对应 .anzu"的插件 id。
// Windows 下运行中的插件占着目录，删包前必须先停它，否则改名会失败。
// 注意运行目录按 manifest id 命名，和包文件名不是一回事：
// 对账要读包里的 manifest，按文件名比对会把"改了名的包"对应的
// 健康插件误报成缺失（然后被停用且再也起不来）。
func (i *Installer) MissingRuntimeIDs() []string {
	have := make(map[string]bool)
	entries, _ := os.ReadDir(i.PackagesDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".anzu") {
			continue
		}
		path := filepath.Join(i.PackagesDir, e.Name())
		if id, err := i.peekPackageID(path); err == nil {
			have[id] = true
		}
		// 读不开的包不参与对账：它可能是别人正在复制的中转文件，
		// 拿文件名猜 id 会冤枉健康插件；装它时自然会报错
	}

	var out []string
	rts, _ := os.ReadDir(i.RuntimeDir)
	for _, e := range rts {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !have[name] {
			out = append(out, name)
		}
	}
	return out
}

// peekPackageID 从包里读 manifest id（失败返回错误，不猜）。
func (i *Installer) peekPackageID(path string) (string, error) {
	raw, err := readFileRetry(path)
	if err != nil {
		return "", err
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", err
	}
	return peekZipID(zr)
}

// ReloadFromDisk 让运行中的插件和磁盘上的运行区对齐：
//   - 运行区没了的 → 停掉并从清单移除
//   - 运行区新增的 → 读清单登记（默认不启用，和首次安装一致）
func (m *Manager) ReloadFromDisk(ctx context.Context) {
	ids, scanErr := m.scanIDs()
	if scanErr != nil {
		m.Log.Warn("扫描插件目录失败，跳过对齐", "err", scanErr)
		return
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}

	// 1. 停止已被移除的插件
	m.mu.RLock()
	seenStale := make(map[string]bool)
	var stale []string
	for id := range m.hosts {
		if !want[id] && !seenStale[id] {
			seenStale[id] = true
			stale = append(stale, id)
		}
	}
	// 已禁用的插件没有 Host，但元数据也要清：否则同 ID 包再放回来
	// 会被 m.metas 里的旧记录挡住，界面一直显示旧信息。
	for id := range m.metas {
		if !want[id] && !seenStale[id] {
			seenStale[id] = true
			stale = append(stale, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range stale {
		m.reloadMu.Lock()
		m.Log.Info("包已删除，停止插件", "plugin", id)
		m.mu.Lock()
		m.disabled[id] = true
		m.mu.Unlock()
		m.saveState()
		m.stopPlugin(id)
		m.mu.Lock()
		delete(m.metas, id)
		delete(m.states, id)
		m.order = removeString(m.order, id)
		m.mu.Unlock()
		m.reloadMu.Unlock()
	}

	// 2. 新出现的插件：登记清单，但默认不启动（安全）
	for _, id := range ids {
		m.mu.RLock()
		_, known := m.metas[id]
		_, running := m.hosts[id]
		m.mu.RUnlock()
		if known || running {
			continue
		}

		man, err := LoadManifest(filepath.Join(m.Dir, id))
		if err != nil {
			m.Log.Warn("新插件清单读取失败", "plugin", id, "err", err)
			continue
		}
		m.mu.Lock()
		m.metas[id] = man
		st := "disabled"
		if m.disabled[id] {
			st = "disabled"
		}
		m.states[id] = st
		m.disabled[id] = true
		m.mu.Unlock()
		m.saveState()
		m.Log.Info("发现新插件（默认未启用）", "plugin", id, "name", man.Name)
	}

	// 3. 已启用的插件如果运行区变了（包被替换），重启一次。
	// 没在运行的（刚装好还没起 / 上次启动失败）也会在这里被拉起。
	for _, id := range ids {
		m.finishUpgrade(id) // 包已经替换完成，放行升级等待
		m.reloadMu.Lock()
		m.mu.RLock()
		_, running := m.hosts[id]
		isDisabled := m.disabled[id]
		m.mu.RUnlock()
		if running || isDisabled {
			m.reloadMu.Unlock()
			continue
		}
		err := m.startPlugin(ctx, id)
		m.reloadMu.Unlock()
		if err == nil {
			m.markUpgraded(id) // 升级/同步后启动成功：给运行区监视器去重
		}
		if err != nil {
			m.Log.Warn("按磁盘状态启动插件失败", "plugin", id, "err", err)
		}
	}
}

// Watch 轮询插件目录，文件变化就重载、目录消失就卸载、新目录就加载。
// 用轮询而不是 fsnotify：插件目录通常很小，2 秒一次的开销可以忽略，
// 而且能顺带发现"编辑器先删后建"这类事件。
func (m *Manager) Watch(ctx context.Context) {
	const interval = 2 * time.Second
	// 启动快照可能是 nil（扫描失败）；循环里首轮成功时再建基线
	last := m.scan()
	missing := make(map[string]int)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.Log.Info("插件热重载已开启", "interval", interval)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		curList, err := m.scanIDs()
		if err != nil {
			// 目录短暂不可读 ≠ 插件全被删了：跳过本轮，别清任何东西
			m.Log.Warn("扫描失败，跳过本轮热重载检查", "err", err)
			continue
		}
		cur := make(map[string]string, len(curList))
		for _, id := range curList {
			// 必须算真实签名：填 "" 会和启动快照里的签名全部不等，
			// 第一次轮询把所有插件重载一遍，之后 last 也变空、改动检测不到。
			cur[id] = dirSignature(filepath.Join(m.Dir, id))
		}
		if last == nil {
			last = cur // 第一轮先建基线，不做"新插件"判断
			continue
		}

		for id, sig := range cur {
			old, existed := last[id]
			if !existed {
				delete(missing, id) // 目录回来了，清掉旧的缺失计数
				m.Log.Info("发现新插件", "plugin", id)
				// 升级时目录会短暂消失再出现：刚升级过就别再启动一次
				if m.recentlyUpgraded(id) {
					m.Log.Debug("升级重建目录，跳过再次启动", "plugin", id)
					continue
				}
				// 新插件默认是禁用的：不需要启动，记一条 info 就够，
				// 别当成"加载失败"报错
				if m.isDisabled(id) {
					m.Log.Info("新插件已安装（默认未启用）", "plugin", id)
					continue
				}
				// **走 Reload 而不是 startPlugin**：上传/投放区同步可能已经把它
				// 起起来了，只 start 会重复注册 Feature（"feature id 冲突"）。
				// Reload 内部是 stop → start，天然幂等。
				if err := m.Reload(ctx, id); err != nil {
					m.Log.Error("新插件加载失败", "plugin", id, "err", err)
				}
				continue
			}
			if old != sig {
				if m.recentlyUpgraded(id) {
					m.Log.Debug("升级刚完成，跳过运行区重载", "plugin", id)
					continue
				}
				m.Log.Info("插件文件有变化，重载", "plugin", id)
				if err := m.Reload(ctx, id); err != nil {
					m.Log.Error("重载失败", "plugin", id, "err", err)
				}
			}
		}

		// 遍历 last ∪ missing：第一次缺失后 last 就不再含它了，
		// 只看 last 永远不会到第 2 次确认。
		gone := make(map[string]bool, len(missing)+len(last))
		for id := range last {
			gone[id] = true
		}
		for id := range missing {
			gone[id] = true
		}
		for id := range gone {
			if _, ok := cur[id]; ok {
				delete(missing, id)
				continue
			}
			// 升级替换时目录会短暂消失（旧目录改名 → 新目录就位），
			// 不能凭一次轮询就永久卸载；要求连续两次缺失，且不在升级中。
			if m.upgradingNow(id) || m.recentlyUpgraded(id) {
				m.Log.Debug("目录暂时缺失（升级中），先不卸载", "plugin", id)
				continue
			}
			missing[id]++
			if missing[id] < 2 {
				m.Log.Debug("目录缺失一次，等下一轮确认", "plugin", id)
				continue
			}
			delete(missing, id)
			m.Log.Info("插件目录已移除，卸载", "plugin", id)
			m.reloadMu.Lock()
			// 直接删目录的路径也要停进程并清元数据，不然监视器还会看到它
			m.stopPlugin(id)
			m.mu.Lock()
			delete(m.metas, id)
			delete(m.states, id)
			m.order = removeString(m.order, id)
			m.mu.Unlock()
			m.reloadMu.Unlock()
		}
		last = cur
	}
}

// bindCallSession 为一次分发生成令牌并登记可信会话。
