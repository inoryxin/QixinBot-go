package plugin

// Manager 核心：结构体、构造、状态与查询视图。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xin/onebot/internal/override"
	"github.com/xin/onebot/internal/perm"
	"github.com/xin/onebot/internal/registry"
	"github.com/xin/onebot/internal/session"
	"github.com/xin/onebot/internal/shared"
)

type HostCallFunc func(ctx context.Context, m *Manifest, method string, params json.RawMessage) (any, *RPCError)

// capProvider 是一个能力的提供者。同名能力可以有多个，priority 高的赢。

// Status 是给 WebUI / 日志看的插件状态。
type Status struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	State    string `json:"state"` // ready | failed | disabled
	Error    string `json:"error,omitempty"`
	Commands int    `json:"commands"`
	PID      int    `json:"pid,omitempty"`
	// MemoryMB 是插件进程常驻内存（RSS）。进程不在时为 0。
	MemoryMB float64 `json:"memory_mb,omitempty"`
	// Shared = 该插件跑在共享宿主里（与其它 cohost 插件共用一个 Node 进程）。
	Shared bool `json:"shared,omitempty"`
	// CohostPool / CohostState 是池登记（阶段 1 只登记，迁移运行在下一阶段）。
	CohostPool      string `json:"cohost_pool,omitempty"`
	CohostState     string `json:"cohost_state,omitempty"`
	CohostCrashes   int    `json:"cohost_crashes,omitempty"`
	CohostLastCrash int64  `json:"cohost_last_crash,omitempty"`
}

// Manager 负责发现、启动、重载和分发插件。

// Manager 负责发现、启动、重载和分发插件。
type Manager struct {
	Dir           string // 插件源码目录
	DataDir       string // 插件私有数据根目录（每个插件一个子目录）
	SharedDepsDir string // 所有插件共享的依赖根目录（data/deps）
	RuntimeRoot   string // 框架自带运行时根目录（data/runtimes）

	runtimeInstallMu sync.Mutex // 串行化运行时安装，避免同目标并发解压/覆盖
	State            string     // 启用/禁用状态的持久化文件
	Log              *slog.Logger
	Reg              *registry.Registry
	Checker          *perm.Checker

	// Shared 是框架托管的跨插件数据；nil 表示不启用
	Shared *shared.Store

	// debugGate 为 true 时，门禁拒绝会记 debug 日志。
	// atomic：配置热重载在写，消息分发热路径在读，裸 bool 是数据竞争。
	DebugGate atomic.Bool

	// OnPluginCall 每次 handle_message 调用后回调（耗时/成败），
	// main 注入给 metrics 采时间序列。分发热路径上，必须是非阻塞的。
	OnPluginCall func(ms int64, failed bool)

	// Env 会注入到每个插件进程（不影响宿主自己）
	Env map[string]string

	// PackagesDir 是 .anzu 投放区，Remove 时一并删除包文件。
	PackagesDir string

	// Inst 是安装器。上传安装要经过它，才能复用签名校验、
	// 数据保留、install.json 记账这些逻辑。
	Inst *Installer

	// SDKDir 会复制到每个插件的 _sdk/，插件通过兼容层调用宿主
	SDKDir string

	// 停止与调用的竞态协调：stopping 标记"正在停"，inflight 记录在途调用，
	// stopDone 是停止完成信号（调用方等它，避免打到半死的进程上）。
	stopMu   sync.Mutex
	stopping map[string]bool
	inflight map[string]int
	stopDone map[string]chan struct{}

	// upgrading 记录"正在替换文件"的插件；这期间的拉起要等升级结束，
	// 否则旧进程可能占住待替换的文件，或旧版本在替换期间被重新拉起。
	upgrading map[string]chan struct{}

	// upgradedAt 记录最近一次升级时间：运行区监视器看到文件变化时，
	// 如果刚升级过就不要再重载一次（会连续重启）。
	upgradedAt map[string]time.Time

	// callSess 保存本次分发的一次性令牌 → 可信会话。
	// 插件调 host.call_tool 时带回来的令牌只能换到它自己那次分发，
	// 伪造身份或串用别的会话都不行。
	callMu   sync.Mutex
	callSess map[string]*session.Session

	// closingManager 是关闭中标记：关闭开始后禁止任何新启动。
	closingManager atomic.Bool
	shutdownCh     chan struct{}
	shutdownOnce   sync.Once

	// 事件队列：OneBot 读循环不直接等插件，避免慢插件拖住 API 响应。
	eventQ    chan *session.EventPayload
	eventDrop atomic.Int64

	// envMu 保护 Env：WebUI 改解释器路径和插件启动会并发访问。
	envMu sync.RWMutex

	// Overrides 是 WebUI 上改的命令权限与 /help 显示开关；nil 表示只用清单默认值。
	Overrides *override.Store
	// PriorityOf 是管理员改过的优先级（WebUI），优先于清单。main 里接 override.Store。
	PriorityOf func(string) (int, bool)

	// Builtins 是框架自己提供的能力（不走插件）。见 builtin.go。
	Builtins map[string]BuiltinCapability

	// stats 是插件运行统计（调用次数、耗时、崩溃）。
	stats *statsBox

	// cool 是宿主级的命令冷却（防刷屏）。
	cool *cooldown

	// jobs 记录插件起的后台任务（host.background）。
	jobs *jobRegistry

	// capLim 限制打向同一个提供者的并发调用数。
	capLim *capLimiter

	// asks 管理 host.ask 的等待（用户下一条消息）。
	asks *askRegistry

	OnHostCall HostCallFunc

	reloadMu sync.Mutex // 串行化 启动/停止/重载，避免并发换代

	mu       sync.RWMutex
	hosts    map[string]*Host
	order    []string
	states   map[string]string
	errs     map[string]string
	metas    map[string]*Manifest
	disabled map[string]bool
	caps     map[string][]capProvider // 能力名 → 提供者（按 priority 降序）
	restarts map[string]int
	// timeoutStreak 连续消息超时计数（挂死自愈：连续 N 次自动禁用）
	timeoutStreak map[string]int

	// 共享宿主（cohost）：按池隔离，见 COHOST-DESIGN.md
	sharedMu        sync.Mutex
	sharedPools     map[string]*SharedHost
	cohostOn        atomic.Bool
	cohostGroupSize int
	// 熔断/崩溃计数按池/插件分开；deathsMu 同时保护以下 map
	deathsMu       sync.Mutex
	sharedDeaths   map[string][]time.Time
	degradedPools  map[string]bool
	cohostCrashes  map[string][]time.Time
	cohostEvicted  map[string]bool
	cohostLastExit map[string]time.Time

	// Pools 是 cohost 池登记（阶段 1）。
	Pools *PoolStore
}

// NewManager 创建插件管理器。

// NewManager 创建插件管理器。
func NewManager(dir, dataDir, statePath string, reg *registry.Registry, checker *perm.Checker, log *slog.Logger) *Manager {
	m := &Manager{
		Dir:             dir,
		DataDir:         dataDir,
		State:           statePath,
		Log:             log.With("component", "plugins"),
		Reg:             reg,
		Checker:         checker,
		hosts:           make(map[string]*Host),
		states:          make(map[string]string),
		stopping:        make(map[string]bool),
		inflight:        make(map[string]int),
		stopDone:        make(map[string]chan struct{}),
		upgrading:       make(map[string]chan struct{}),
		upgradedAt:      make(map[string]time.Time),
		shutdownCh:      make(chan struct{}),
		eventQ:          make(chan *session.EventPayload, 256),
		callSess:        make(map[string]*session.Session),
		errs:            make(map[string]string),
		metas:           make(map[string]*Manifest),
		disabled:        make(map[string]bool),
		caps:            make(map[string][]capProvider),
		restarts:        make(map[string]int),
		timeoutStreak:   make(map[string]int),
		cohostGroupSize: 6,
		sharedPools:     make(map[string]*SharedHost),
		sharedDeaths:    make(map[string][]time.Time),
		degradedPools:   make(map[string]bool),
		cohostCrashes:   make(map[string][]time.Time),
		cohostEvicted:   make(map[string]bool),
		cohostLastExit:  make(map[string]time.Time),
		stats:           newStatsBox(),
		asks:            newAskRegistry(),
		cool:            newCooldown(),
		jobs:            newJobRegistry(),
		capLim:          newCapLimiter(),
	}
	m.loadState()
	if ps, err := NewPoolStore(filepath.Join(dataDir, "cohost-pools.json")); err != nil {
		log.Warn("读取 cohost 池配置失败，池管理不可用", "err", err)
	} else {
		m.Pools = ps
		// 逐出状态要跨重启保留：状态为 evicted 的插件直接按独立进程运行，
		// 且后续独立崩溃仍按 3 次禁用，而不是回落到普通的 6 次。
		if _, ass := ps.List(); ass != nil {
			for id, a := range ass {
				if a.State == "evicted" {
					m.cohostEvicted[id] = true
				}
				if len(a.CrashTimes) > 0 {
					ts := make([]time.Time, 0, len(a.CrashTimes))
					for _, sec := range a.CrashTimes {
						ts = append(ts, time.Unix(sec, 0))
					}
					m.cohostCrashes[id] = ts
				}
			}
		}
	}
	return m
}

// SetCohostEnabled 打开/关闭共享宿主（实验功能，默认关）。

// List 返回插件状态列表（供 WebUI / 日志）。

// List 返回插件状态列表（供 WebUI / 日志）。
func (m *Manager) List() []Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	ids := make([]string, 0, len(m.metas))
	for id := range m.metas {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		man := m.metas[id]
		st := Status{
			ID:       id,
			Name:     man.Name,
			Version:  man.Version,
			State:    m.states[id],
			Error:    m.errs[id],
			Commands: len(man.Commands),
		}
		if m.disabled[id] {
			st.State = "disabled"
		}
		if h, ok := m.hosts[id]; ok {
			if h.cohost != nil {
				st.Shared = true
				// 共享组没有独立插件 PID：用共享 Node 进程的 PID/RSS 展示。
				// 同组插件显示同一个进程值，不能按插件相加。
				if pid := h.cohost.PID(); pid != 0 {
					st.PID = pid
					st.MemoryMB = h.cohost.RSSMB()
				}
			} else if h.cmd != nil && h.cmd.Process != nil {
				st.PID = h.cmd.Process.Pid
				st.MemoryMB = processRSSMB(st.PID)
			}
		}
		st.CohostState = CohostStateQuarantine
		if m.Pools != nil {
			if pool, ass, ok := m.Pools.PoolOf(id); ok {
				st.CohostPool = pool.Name
				st.CohostState = ass.State
				st.CohostCrashes = len(ass.CrashTimes)
				if len(ass.CrashTimes) > 0 {
					st.CohostLastCrash = ass.CrashTimes[len(ass.CrashTimes)-1]
				}
			}
		}
		if st.State == "" {
			st.State = "failed"
		}
		out = append(out, st)
	}
	return out
}

// StopAll 关闭全部插件。

// HostOf 返回某个插件的运行句柄；未运行时返回 nil。
func (m *Manager) HostOf(id string) *Host {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.hosts[id]
}

// Describe 返回一段人类可读的插件摘要，用于启动日志。

// Describe 返回一段人类可读的插件摘要，用于启动日志。
func (m *Manager) Describe() string {
	list := m.List()
	if len(list) == 0 {
		return "没有插件"
	}
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "%s(%s)", s.ID, s.State)
		if s.Error != "" {
			fmt.Fprintf(&b, "[%s]", s.Error)
		}
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// ---- 内部工具 ----

func (m *Manager) setState(id, state, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.states[id] = state
	if errMsg != "" {
		m.errs[id] = errMsg
	} else {
		delete(m.errs, id)
	}
}

func (m *Manager) isDisabled(id string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.disabled[id]
}

// StateOf 返回插件的运行状态（ready/disabled/failed/...）。
func (m *Manager) StateOf(id string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.states[id]
}

// Manifests 返回当前已加载插件的清单副本。

// Manifests 返回当前已加载插件的清单副本。
func (m *Manager) Manifests() map[string]*Manifest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*Manifest, len(m.metas))
	for k, v := range m.metas {
		out[k] = v
	}
	return out
}

// InstallZip 从 zip 安装插件，返回插件 id。已存在同名插件时拒绝，避免误覆盖。

type persistedState struct {
	Disabled []string `json:"disabled"`
}

func (m *Manager) loadState() {
	if m.State == "" {
		return
	}
	data, err := os.ReadFile(m.State)
	if err != nil {
		return
	}
	var st persistedState
	if err := json.Unmarshal(data, &st); err != nil {
		m.Log.Warn("插件状态文件损坏，忽略", "path", m.State, "err", err)
		return
	}
	for _, id := range st.Disabled {
		m.disabled[id] = true
	}
}

func (m *Manager) saveState() {
	if m.State == "" {
		return
	}
	m.mu.RLock()
	ids := make([]string, 0, len(m.disabled))
	for id := range m.disabled {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	sort.Strings(ids)

	data, err := json.MarshalIndent(persistedState{Disabled: ids}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.State), 0o755); err != nil {
		m.Log.Warn("创建状态目录失败", "err", err)
		return
	}
	tmp := m.State + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		m.Log.Warn("写插件状态失败", "err", err)
		return
	}
	if err := os.Rename(tmp, m.State); err != nil {
		m.Log.Warn("提交插件状态失败", "err", err)
	}
}

// IsService 判断插件是不是服务插件（只提供能力，不接消息）。

// truncateForLog 截断日志里的用户输入（避免刷屏，也避免日志里出现超长消息）。
func truncateForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
